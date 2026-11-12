package officers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

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
//   - an announcement that never got HQ's 200 is re-announced by the draft
//     sweep, and ONLY such a one. 🔴 HQ's pr-opened is idempotent only while
//     the pull request's loop item is live: once that item has finished, a
//     repeat enqueues a NEW opened item and restarts the loop, and the
//     author's token cannot read the shepherd's items to tell. So muster
//     re-announces from its own record of "never acknowledged", never from
//     what it can see on GitHub.
//   - once HQ put the pull request in a loop (its 200 named a loop item), the
//     authoring officer writes to it only under a held fix item
//     (officership's loopWrite).
//
// Kept in muster's database (PGPRLoopStore, migration 0118) when it has one,
// so a restart and a second replica see the same record; else in this
// process's memory (MemPRLoopStore), which a restart forgets.
// ---------------------------------------------------------------------------

// The states of an announcement.
const (
	// PRLoopPending: muster has not had HQ's 200 for it (the sweep retries).
	PRLoopPending = "pending"
	// PRLoopInLoop: HQ's 200 named a loop item for it.
	PRLoopInLoop = "in_loop"
	// PRLoopNoTaker: HQ's 200 named no item (no shepherd takes it).
	PRLoopNoTaker = "no_taker"
	// PRLoopClosed: never re-announced — HQ answered 409 officer_shadow (a
	// shadow officer's pull request is never shepherded), or the pull request
	// was closed before an announcement got through.
	PRLoopClosed = "closed"
)

// PRLoop is one pull request's announcement.
type PRLoop struct {
	Repo    string
	PR      int
	HeadSHA string
	State   string
}

// PRLoopStore keeps the announcements, per officer. ctx acts as the
// officer's owner (the database store is under row-level security).
type PRLoopStore interface {
	// PRLoopAnnounced records repo#pr (at head) as pending, unless it is
	// already recorded, which keeps its state.
	PRLoopAnnounced(ctx context.Context, officer, repo string, pr int, head string) error
	// PRLoopSettle sets repo#pr's state.
	PRLoopSettle(ctx context.Context, officer, repo string, pr int, state string) error
	// PRLoopsPending lists officer's pending announcements.
	PRLoopsPending(ctx context.Context, officer string) ([]PRLoop, error)
	// PRLoopState is repo#pr's state ("" when it was never announced).
	PRLoopState(ctx context.Context, officer, repo string, pr int) (string, error)
}

func prLoopKey(officer, repo string, pr int) string {
	return officer + " " + strings.ToLower(repo) + "#" + strconv.Itoa(pr)
}

// MemPRLoopStore is PRLoopStore in this process's memory.
type MemPRLoopStore struct {
	mu   sync.Mutex
	rows map[string]PRLoop
	who  map[string]string // key -> officer
}

// NewMemPRLoopStore is an empty memory store.
func NewMemPRLoopStore() *MemPRLoopStore {
	return &MemPRLoopStore{rows: map[string]PRLoop{}, who: map[string]string{}}
}

// PRLoopAnnounced implements PRLoopStore.
func (m *MemPRLoopStore) PRLoopAnnounced(_ context.Context, officer, repo string, pr int, head string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := prLoopKey(officer, repo, pr)
	if _, ok := m.rows[k]; !ok {
		m.rows[k] = PRLoop{Repo: strings.ToLower(repo), PR: pr, HeadSHA: head, State: PRLoopPending}
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
		r = PRLoop{Repo: strings.ToLower(repo), PR: pr}
		m.who[k] = officer
	}
	r.State = state
	m.rows[k] = r
	return nil
}

// PRLoopsPending implements PRLoopStore.
func (m *MemPRLoopStore) PRLoopsPending(_ context.Context, officer string) ([]PRLoop, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PRLoop
	for k, r := range m.rows {
		if m.who[k] == officer && r.State == PRLoopPending {
			out = append(out, r)
		}
	}
	return out, nil
}

// PRLoopState implements PRLoopStore.
func (m *MemPRLoopStore) PRLoopState(_ context.Context, officer, repo string, pr int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[prLoopKey(officer, repo, pr)].State, nil
}

// PGPRLoopStore is PRLoopStore on officer_pr_loops (migration 0118), under
// row-level security: ctx must act as the officer's owner.
type PGPRLoopStore struct{ pool *pgxpool.Pool }

// NewPGPRLoopStore is the store over pool.
func NewPGPRLoopStore(pool *pgxpool.Pool) *PGPRLoopStore { return &PGPRLoopStore{pool: pool} }

// PRLoopAnnounced implements PRLoopStore.
func (s *PGPRLoopStore) PRLoopAnnounced(ctx context.Context, officer, repo string, pr int, head string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_pr_loops (officer, repo, pr, head_sha, state) VALUES ($1, lower($2), $3, $4, 'pending')
		ON CONFLICT (owner_id, officer, repo, pr) DO NOTHING`, officer, repo, pr, head)
	return err
}

// PRLoopSettle implements PRLoopStore.
func (s *PGPRLoopStore) PRLoopSettle(ctx context.Context, officer, repo string, pr int, state string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_pr_loops (officer, repo, pr, head_sha, state) VALUES ($1, lower($2), $3, '', $4)
		ON CONFLICT (owner_id, officer, repo, pr) DO UPDATE SET state = EXCLUDED.state, updated_at = now()`, officer, repo, pr, state)
	return err
}

// PRLoopsPending implements PRLoopStore.
func (s *PGPRLoopStore) PRLoopsPending(ctx context.Context, officer string) ([]PRLoop, error) {
	rows, err := s.pool.Query(ctx, `SELECT repo, pr, head_sha, state FROM officer_pr_loops WHERE officer = $1 AND state = 'pending' ORDER BY repo, pr`, officer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PRLoop
	for rows.Next() {
		var r PRLoop
		if err := rows.Scan(&r.Repo, &r.PR, &r.HeadSHA, &r.State); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
