package officers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// A DRAFT OFFICER'S REVIEW LOOPS, AS muster TOLD HQ (docs/officers.md "The
// review loop through HQ").
//
// muster announces each clean new pull request to HQ (pr-opened). The record
// here is what HQ ANSWERED, kept so that:
//
//   - an announcement that never got HQ's answer is re-announced by the draft
//     sweep, and ONLY such a one. 🔴 HQ's pr-opened is idempotent only while
//     the pull request's loop item is live: once that item has finished, a
//     repeat enqueues a NEW opened item and restarts the loop, and the
//     author's token cannot read the shepherd's items to tell. So muster
//     re-announces from its own record of "never answered", never from what
//     it can see on GitHub.
//   - once HQ put the pull request in a loop (its 200 named a loop item), the
//     authoring officer writes to it only under a held fix item
//     (officership's loopWrite).
//
// 🔴 A ROW IS CLAIMED BEFORE IT IS SENT, so a send whose answer muster could
// not record is never sent again — not after a restart, not by another
// replica. A row is 'sending' while one process announces it (written by the
// create, or claimed from 'pending' by the sweep in ONE conditional UPDATE, so
// two replicas cannot both claim it); the answer settles it, and only an
// outage the process recorded puts it back to 'pending'. A 'sending' row whose
// answer was lost (a crash, a write that failed after HQ answered) stays
// 'sending': never re-sent — an announcement lost, never a loop restarted.
//
// Kept in muster's database (PGPRLoopStore, migration 0119) when it has one,
// so a restart and a second replica see the same record; else in this
// process's memory (MemPRLoopStore), which a restart forgets.
// ---------------------------------------------------------------------------

// The states of an announcement.
const (
	// PRLoopSending: one process is announcing it now (or lost its answer):
	// never claimed by the sweep.
	PRLoopSending = "sending"
	// PRLoopPending: HQ was unreachable (an outage muster recorded): the sweep
	// claims and re-sends it, within its age bound.
	PRLoopPending = "pending"
	// PRLoopInLoop: HQ's 200 named a loop item for it.
	PRLoopInLoop = "in_loop"
	// PRLoopNoTaker: HQ's 200 named no item (no shepherd takes it).
	PRLoopNoTaker = "no_taker"
	// PRLoopClosed: never re-announced — HQ answered 409 officer_shadow (a
	// shadow officer's pull request is never shepherded), or the pull request
	// was seen closed or merged.
	PRLoopClosed = "closed"
	// PRLoopFailed: never re-announced — HQ refused it with a permanent 4xx (a
	// decision, not an outage), or it stayed pending past the sweep's age
	// bound.
	PRLoopFailed = "failed"
)

// PRLoop is one pull request's announcement.
type PRLoop struct {
	Repo    string
	PR      int
	HeadSHA string
	State   string
	// AnnouncedAt is when the pull request was first announced (the sweep
	// re-sends a pending one only within its age bound).
	AnnouncedAt time.Time
}

// PRLoopStore keeps the announcements, per officer. ctx acts as the
// officer's owner (the database store is under row-level security).
type PRLoopStore interface {
	// PRLoopAnnounced records repo#pr (at head) as 'sending' — the announcing
	// process's claim — unless it is already recorded, which keeps its state.
	PRLoopAnnounced(ctx context.Context, officer, repo string, pr int, head string) error
	// PRLoopSettle sets repo#pr's state.
	PRLoopSettle(ctx context.Context, officer, repo string, pr int, state string) error
	// PRLoopsClaim fails officer's pending announcements first made before
	// notBefore (returned as expired), and CLAIMS the rest — pending ->
	// sending, atomically, so a concurrent claimer gets none of them —
	// returning them as claimed.
	PRLoopsClaim(ctx context.Context, officer string, notBefore time.Time) (claimed, expired []PRLoop, err error)
	// PRLoopState is repo#pr's state ("" when it was never announced).
	PRLoopState(ctx context.Context, officer, repo string, pr int) (string, error)
	// PRLoopsInLoop lists officer's pull requests in a loop.
	PRLoopsInLoop(ctx context.Context, officer string) ([]PRLoop, error)
}

func prLoopKey(officer, repo string, pr int) string {
	return officer + " " + strings.ToLower(repo) + "#" + strconv.Itoa(pr)
}

// MemPRLoopStore is PRLoopStore in this process's memory.
type MemPRLoopStore struct {
	mu   sync.Mutex
	rows map[string]PRLoop
	who  map[string]string // key -> officer
	// Now is its clock (AnnouncedAt); nil is time.Now. For tests.
	Now func() time.Time
}

// NewMemPRLoopStore is an empty memory store.
func NewMemPRLoopStore() *MemPRLoopStore {
	return &MemPRLoopStore{rows: map[string]PRLoop{}, who: map[string]string{}}
}

func (m *MemPRLoopStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// PRLoopAnnounced implements PRLoopStore.
func (m *MemPRLoopStore) PRLoopAnnounced(_ context.Context, officer, repo string, pr int, head string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := prLoopKey(officer, repo, pr)
	if _, ok := m.rows[k]; !ok {
		m.rows[k] = PRLoop{Repo: strings.ToLower(repo), PR: pr, HeadSHA: head, State: PRLoopSending, AnnouncedAt: m.now()}
		m.who[k] = officer
	}
	return nil
}

// PRLoopSettle implements PRLoopStore.
func (m *MemPRLoopStore) PRLoopSettle(_ context.Context, officer, repo string, pr int, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := prLoopKey(officer, repo, pr)
	r, ok := m.rows[k]
	if !ok {
		r = PRLoop{Repo: strings.ToLower(repo), PR: pr, AnnouncedAt: m.now()}
		m.who[k] = officer
	}
	r.State = state
	m.rows[k] = r
	return nil
}

// PRLoopsClaim implements PRLoopStore.
func (m *MemPRLoopStore) PRLoopsClaim(_ context.Context, officer string, notBefore time.Time) (claimed, expired []PRLoop, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.rows {
		if m.who[k] != officer || r.State != PRLoopPending {
			continue
		}
		if r.AnnouncedAt.Before(notBefore) {
			r.State = PRLoopFailed
			expired = append(expired, r)
		} else {
			r.State = PRLoopSending
			claimed = append(claimed, r)
		}
		m.rows[k] = r
	}
	return claimed, expired, nil
}

// PRLoopState implements PRLoopStore.
func (m *MemPRLoopStore) PRLoopState(_ context.Context, officer, repo string, pr int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[prLoopKey(officer, repo, pr)].State, nil
}

// PRLoopsInLoop implements PRLoopStore.
func (m *MemPRLoopStore) PRLoopsInLoop(_ context.Context, officer string) ([]PRLoop, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PRLoop
	for k, r := range m.rows {
		if m.who[k] == officer && r.State == PRLoopInLoop {
			out = append(out, r)
		}
	}
	return out, nil
}

// PGPRLoopStore is PRLoopStore on officer_pr_loops (migration 0119), under
// row-level security: ctx must act as the officer's owner.
type PGPRLoopStore struct{ pool *pgxpool.Pool }

// NewPGPRLoopStore is the store over pool.
func NewPGPRLoopStore(pool *pgxpool.Pool) *PGPRLoopStore { return &PGPRLoopStore{pool: pool} }

// PRLoopAnnounced implements PRLoopStore.
func (s *PGPRLoopStore) PRLoopAnnounced(ctx context.Context, officer, repo string, pr int, head string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_pr_loops (officer, repo, pr, head_sha, state) VALUES ($1, lower($2), $3, $4, 'sending')
		ON CONFLICT (owner_id, officer, repo, pr) DO NOTHING`, officer, repo, pr, head)
	return err
}

// PRLoopSettle implements PRLoopStore.
func (s *PGPRLoopStore) PRLoopSettle(ctx context.Context, officer, repo string, pr int, state string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_pr_loops (officer, repo, pr, head_sha, state) VALUES ($1, lower($2), $3, '', $4)
		ON CONFLICT (owner_id, officer, repo, pr) DO UPDATE SET state = EXCLUDED.state, updated_at = now()`, officer, repo, pr, state)
	return err
}

// PRLoopsClaim implements PRLoopStore: each UPDATE takes the row locks of
// the rows it changes and re-reads `state = 'pending'` after waiting for a
// concurrent claimer, so of two replicas sweeping at once only one claims a
// row.
func (s *PGPRLoopStore) PRLoopsClaim(ctx context.Context, officer string, notBefore time.Time) (claimed, expired []PRLoop, err error) {
	scan := func(q string) ([]PRLoop, error) {
		rows, err := s.pool.Query(ctx, q, officer, notBefore)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []PRLoop
		for rows.Next() {
			var r PRLoop
			if err := rows.Scan(&r.Repo, &r.PR, &r.HeadSHA, &r.State, &r.AnnouncedAt); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	if expired, err = scan(`UPDATE officer_pr_loops SET state = 'failed', updated_at = now()
		WHERE officer = $1 AND state = 'pending' AND created_at < $2 RETURNING repo, pr, head_sha, state, created_at`); err != nil {
		return nil, nil, err
	}
	claimed, err = scan(`UPDATE officer_pr_loops SET state = 'sending', updated_at = now()
		WHERE officer = $1 AND state = 'pending' AND created_at >= $2 RETURNING repo, pr, head_sha, state, created_at`)
	return claimed, expired, err
}

// PRLoopState implements PRLoopStore.
func (s *PGPRLoopStore) PRLoopState(ctx context.Context, officer, repo string, pr int) (string, error) {
	var st string
	err := s.pool.QueryRow(ctx, `SELECT state FROM officer_pr_loops WHERE officer = $1 AND repo = lower($2) AND pr = $3`, officer, repo, pr).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return st, err
}

// PRLoopsInLoop implements PRLoopStore.
func (s *PGPRLoopStore) PRLoopsInLoop(ctx context.Context, officer string) ([]PRLoop, error) {
	rows, err := s.pool.Query(ctx, `SELECT repo, pr, head_sha, state, created_at FROM officer_pr_loops WHERE officer = $1 AND state = 'in_loop' ORDER BY repo, pr`, officer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PRLoop
	for rows.Next() {
		var r PRLoop
		if err := rows.Scan(&r.Repo, &r.PR, &r.HeadSHA, &r.State, &r.AnnouncedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
