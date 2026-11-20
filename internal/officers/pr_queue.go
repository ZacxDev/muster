package officers

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// A DRAFT OFFICER'S PULL-REQUEST QUEUE (docs/officers.md "The open-pull-request
// queue").
//
// A draft officer may hold MaxOpenPRsPerRepo pull requests open in one
// repository. A create past that cap is not refused: the push gate records it
// here — the branch, the head it was asked at, the title and body — and
// answers the officer that it is queued. The draft sweep then opens the oldest
// queued branch of a repository as a draft itself (with the officer App's
// token, after the same gates) once one of the officer's pull requests there
// closes or merges.
//
// One row per (officer, repository, branch): a second create from a QUEUED
// branch refreshes its head, title and body and keeps its place (queued_at);
// a create from a branch whose earlier row was settled queues it again, at
// the back. A row leaves the queue settled: opened (with the pull request's
// number) or skipped (with why: the branch is gone, its head moved with no new
// create, a pull request is already open from it, or its change was refused).
//
// Kept in muster's database (PGPRQueueStore, migration 0169) when it has one,
// so a restart and a second replica see the same queue; else in this
// process's memory (MemPRQueueStore), which a restart forgets.
// ---------------------------------------------------------------------------

// The states of a queued create.
const (
	// PRQueueQueued: waiting for a slot.
	PRQueueQueued = "queued"
	// PRQueueOpened: muster opened it (QueuedPR.PR is its number).
	PRQueueOpened = "opened"
	// PRQueueSkipped: muster dropped it (QueuedPR.Note says why).
	PRQueueSkipped = "skipped"
)

// QueuedPR is one create the open cap held back.
type QueuedPR struct {
	ID     int64
	Repo   string
	Branch string
	// HeadSHA is the branch's head when the create was (last) asked for: the
	// drain opens the pull request only while the branch is still there.
	HeadSHA string
	Title   string
	Body    string
	// Item is the HQ item the branch is for; Instance the officer instance
	// that asked.
	Item     int
	Instance string
	State    string
	// PR is the pull request muster opened (state opened); Note why it was
	// skipped — or, on a row still queued, what HQ was told about why it
	// waits (PRQueueMark), so it is told once.
	PR       int
	Note     string
	QueuedAt time.Time
	// Fresh is set on PRQueueAdd's answer only: true when that Add made the
	// row queued — a new row, or a settled one queued again — and false when
	// it refreshed a row that was already queued (the cap-triggered close
	// runs for a fresh one only).
	Fresh bool
}

// PRQueueStore keeps the queue, per officer. ctx acts as the officer's owner
// (the database store is under row-level security).
type PRQueueStore interface {
	// PRQueueAdd queues q for officer (q's Repo, Branch, HeadSHA, Title, Body,
	// Item and Instance; repo compared without case) and returns the row: a
	// branch already queued keeps its place and takes q's head, title, body,
	// item and instance; one settled before is queued again at the back.
	PRQueueAdd(ctx context.Context, officer string, q QueuedPR) (QueuedPR, error)
	// PRQueueList is officer's QUEUED rows in repo ("" for every repository),
	// oldest first (queued_at, then id): the order the drain opens them in.
	PRQueueList(ctx context.Context, officer, repo string) ([]QueuedPR, error)
	// PRQueueSettle settles officer's row id to state (PRQueueOpened or
	// PRQueueSkipped) with pr and note — only while it is still queued AT
	// head: a create that refreshed it since is not settled by a decision
	// made on the older head. ok false when nothing was settled.
	PRQueueSettle(ctx context.Context, officer string, id int64, head, state string, pr int, note string) (bool, error)
	// PRQueueMark sets note on officer's row id while it is still queued AT
	// head and its note is still empty: ok true only for the call that set
	// it, so a caller that tells HQ only on ok tells it once per row and
	// head. A create that refreshes the row (PRQueueAdd) clears the note.
	PRQueueMark(ctx context.Context, officer string, id int64, head, note string) (bool, error)
}

// MemPRQueueStore is PRQueueStore in this process's memory.
type MemPRQueueStore struct {
	mu   sync.Mutex
	next int64
	rows map[int64]*memQueued
	// now is the clock (time.Now when nil); seq breaks a tie of equal times.
	now func() time.Time
}

type memQueued struct {
	officer string
	q       QueuedPR
	seq     int64
}

// NewMemPRQueueStore is an empty memory queue.
func NewMemPRQueueStore() *MemPRQueueStore { return &MemPRQueueStore{rows: map[int64]*memQueued{}} }

// PRQueueAdd implements PRQueueStore.
func (m *MemPRQueueStore) PRQueueAdd(_ context.Context, officer string, q QueuedPR) (QueuedPR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	q.Repo = strings.ToLower(q.Repo)
	for _, r := range m.rows {
		if r.officer != officer || r.q.Repo != q.Repo || r.q.Branch != q.Branch {
			continue
		}
		r.q.HeadSHA, r.q.Title, r.q.Body, r.q.Item, r.q.Instance, r.q.Note = q.HeadSHA, q.Title, q.Body, q.Item, q.Instance, ""
		fresh := r.q.State != PRQueueQueued
		if fresh {
			m.next++
			r.q.State, r.q.PR, r.q.Note, r.q.QueuedAt, r.seq = PRQueueQueued, 0, "", now(), m.next
		}
		out := r.q
		out.Fresh = fresh
		return out, nil
	}
	m.next++
	q.ID, q.State, q.PR, q.Note, q.QueuedAt, q.Fresh = m.next, PRQueueQueued, 0, "", now(), false
	m.rows[q.ID] = &memQueued{officer: officer, q: q, seq: m.next}
	q.Fresh = true
	return q, nil
}

// PRQueueList implements PRQueueStore.
func (m *MemPRQueueStore) PRQueueList(_ context.Context, officer, repo string) ([]QueuedPR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []*memQueued
	for _, r := range m.rows {
		if r.officer == officer && r.q.State == PRQueueQueued && (repo == "" || r.q.Repo == strings.ToLower(repo)) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].q.QueuedAt.Equal(rows[j].q.QueuedAt) {
			return rows[i].q.QueuedAt.Before(rows[j].q.QueuedAt)
		}
		return rows[i].seq < rows[j].seq
	})
	out := make([]QueuedPR, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.q)
	}
	return out, nil
}

// PRQueueSettle implements PRQueueStore.
func (m *MemPRQueueStore) PRQueueSettle(_ context.Context, officer string, id int64, head, state string, pr int, note string) (bool, error) {
	if state != PRQueueOpened && state != PRQueueSkipped {
		return false, errors.New("officers: a queued create settles opened or skipped")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.officer != officer || r.q.State != PRQueueQueued || r.q.HeadSHA != head {
		return false, nil
	}
	r.q.State, r.q.PR, r.q.Note = state, pr, truncateNote(note)
	return true, nil
}

// PRQueueMark implements PRQueueStore.
func (m *MemPRQueueStore) PRQueueMark(_ context.Context, officer string, id int64, head, note string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.officer != officer || r.q.State != PRQueueQueued || r.q.HeadSHA != head || r.q.Note != "" {
		return false, nil
	}
	r.q.Note = truncateNote(note)
	return true, nil
}

// PGPRQueueStore is PRQueueStore on officer_pr_queue (migration 0169), under
// row-level security: ctx must act as the officer's owner.
type PGPRQueueStore struct{ pool *pgxpool.Pool }

// NewPGPRQueueStore is the store over pool.
func NewPGPRQueueStore(pool *pgxpool.Pool) *PGPRQueueStore { return &PGPRQueueStore{pool: pool} }

const prQueueColumns = `id, repo, branch, head_sha, title, body, item, instance, state, coalesce(pr, 0), note, queued_at`

// PRQueueAdd implements PRQueueStore: ONE upsert on the (owner, officer,
// repository, branch) key. A queued row keeps queued_at; a settled one is
// queued again with queued_at now. Fresh is an INSERT (xmax = 0), or an update
// of a row the statement's snapshot (prev) shows settled: a concurrent Add
// that lost the insert race to a new row reads it as a refresh, not fresh.
func (s *PGPRQueueStore) PRQueueAdd(ctx context.Context, officer string, q QueuedPR) (QueuedPR, error) {
	var out QueuedPR
	err := s.pool.QueryRow(ctx, `WITH prev AS (SELECT state FROM officer_pr_queue
			WHERE owner_id = nullif(current_setting('muster.uid', true), '')::bigint AND officer = $1 AND repo = lower($2) AND branch = $3)
		INSERT INTO officer_pr_queue (officer, repo, branch, head_sha, title, body, item, instance)
		VALUES ($1, lower($2), $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner_id, officer, repo, branch) DO UPDATE SET
			head_sha = EXCLUDED.head_sha, title = EXCLUDED.title, body = EXCLUDED.body, item = EXCLUDED.item, instance = EXCLUDED.instance,
			queued_at = CASE WHEN officer_pr_queue.state = 'queued' THEN officer_pr_queue.queued_at ELSE now() END,
			state = 'queued', pr = NULL, note = '', settled_at = NULL
		RETURNING `+prQueueColumns+`, (xmax = 0 OR coalesce((SELECT state <> 'queued' FROM prev), false))`,
		officer, q.Repo, q.Branch, q.HeadSHA, q.Title, q.Body, q.Item, q.Instance).Scan(
		&out.ID, &out.Repo, &out.Branch, &out.HeadSHA, &out.Title, &out.Body, &out.Item, &out.Instance, &out.State, &out.PR, &out.Note, &out.QueuedAt,
		&out.Fresh)
	return out, err
}

// PRQueueList implements PRQueueStore.
func (s *PGPRQueueStore) PRQueueList(ctx context.Context, officer, repo string) ([]QueuedPR, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+prQueueColumns+` FROM officer_pr_queue
		WHERE officer = $1 AND state = 'queued' AND ($2 = '' OR repo = lower($2)) ORDER BY queued_at, id`, officer, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedPR
	for rows.Next() {
		var q QueuedPR
		if err := rows.Scan(&q.ID, &q.Repo, &q.Branch, &q.HeadSHA, &q.Title, &q.Body, &q.Item, &q.Instance, &q.State, &q.PR, &q.Note, &q.QueuedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// PRQueueSettle implements PRQueueStore: one conditional UPDATE.
func (s *PGPRQueueStore) PRQueueSettle(ctx context.Context, officer string, id int64, head, state string, pr int, note string) (bool, error) {
	if state != PRQueueOpened && state != PRQueueSkipped {
		return false, errors.New("officers: a queued create settles opened or skipped")
	}
	var prArg any
	if pr > 0 {
		prArg = pr
	}
	tag, err := s.pool.Exec(ctx, `UPDATE officer_pr_queue SET state = $4, pr = $5, note = $6, settled_at = now()
		WHERE officer = $1 AND id = $2 AND head_sha = $3 AND state = 'queued'`, officer, id, head, state, prArg, truncateNote(note))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// PRQueueMark implements PRQueueStore: one conditional UPDATE.
func (s *PGPRQueueStore) PRQueueMark(ctx context.Context, officer string, id int64, head, note string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE officer_pr_queue SET note = $4
		WHERE officer = $1 AND id = $2 AND head_sha = $3 AND state = 'queued' AND note = ''`, officer, id, head, truncateNote(note))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// maxQueueNote bounds a skip's note (0169's CHECK).
const maxQueueNote = 1000

func truncateNote(s string) string {
	if len(s) <= maxQueueNote {
		return s
	}
	cut := maxQueueNote
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
