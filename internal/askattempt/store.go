package askattempt

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/redact"
)

// Store is the answer-attempt log, under row-level security: every method but
// Prune acts as ctx's principal, on its rows only.
type Store interface {
	// Record writes a, numbered after the caller's earlier attempts on the
	// same card, and returns it as stored (ID, At, Outcome, Number, the
	// redacted and capped Snapshot).
	Record(ctx context.Context, a Attempt) (Attempt, error)
	// List is the caller's attempts at or after since, newest first, at most
	// limit.
	List(ctx context.Context, since time.Time, limit int) ([]Attempt, error)
	// ForCard is the caller's attempts on card id, oldest first: its timeline.
	ForCard(ctx context.Context, cardID string) ([]Attempt, error)
	// Prune is the retention sweep, for every owner (the system scope): it
	// deletes rows written before before, and returns how many.
	Prune(ctx context.Context, before time.Time) (int, error)
}

// ErrBadAttempt: a write refused before it reached the database.
var ErrBadAttempt = errors.New("askattempt: not a valid attempt")

// RedactSnapshot is a pane snapshot as a row keeps it: redacted
// (redact.Secrets, the one redactor) and cut to MaxSnapshot characters.
// 🔴 Redaction BEFORE the cut, so a cut cannot split a secret into a fragment
// the redactor no longer recognises; the tail is kept, not the head — the
// picker is at the bottom of the pane.
func RedactSnapshot(s string) string {
	r := []rune(redact.Secrets(s))
	if len(r) <= MaxSnapshot {
		return string(r)
	}
	return string(r[len(r)-MaxSnapshot:])
}

// prepare is a as every store writes it: checked against the enumerations,
// its outcome its reason's, its names and snapshot bounded. The one place.
func prepare(a Attempt) (Attempt, error) {
	r, ok := Reasons[a.Reason]
	if !ok {
		return Attempt{}, fmt.Errorf("%w: reason %q is not in askattempt.Reasons", ErrBadAttempt, a.Reason)
	}
	switch {
	case a.Runtime != RuntimeClaudeCode && a.Runtime != RuntimeOpenCode,
		a.Door != DoorCard && a.Door != DoorAttention && a.Door != DoorPhone,
		a.Action != ActionSubmit && a.Action != ActionDismiss,
		a.Method != MethodTyped && a.Method != MethodRelayed && a.Method != MethodEscape && a.Method != MethodMessage,
		len(a.Shape.Options) != len(a.Shape.Multi), len(a.Shape.Options) > MaxQuestions:
		return Attempt{}, fmt.Errorf("%w: %s/%s/%s/%s, %d/%d questions", ErrBadAttempt, a.Runtime, a.Door, a.Action, a.Method,
			len(a.Shape.Options), len(a.Shape.Multi))
	}
	a.Outcome = r.Outcome
	a.Host, a.Session, a.Window, a.CardID = cut(a.Host, MaxNameRunes), cut(a.Session, MaxNameRunes), cut(a.Window, MaxWindow), cut(a.CardID, MaxNameRunes)
	a.Snapshot = RedactSnapshot(a.Snapshot)
	a.Keys = max(a.Keys, 0)
	a.Duration = max(a.Duration, 0)
	if a.At.IsZero() {
		a.At = time.Now()
	}
	if a.Shape.Options == nil {
		a.Shape.Options, a.Shape.Multi = []int{}, []bool{}
	}
	return a, nil
}

// PG is Store on Postgres.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// sys is the store's one system-scope helper: the retention sweep, which
// deletes rows BY AGE ONLY and returns a count (internal/authz's systemLedger
// lists it).
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

const cols = `id, owner_id, at, runtime, door, action, method, mirror_host, mirror_session, mirror_window, card_id,
	options, multi_select, other_used, outcome, reason, keys, duration_ms, attempt, snapshot`

func scan(row pgx.Row) (Attempt, error) {
	var a Attempt
	var ms int64
	err := row.Scan(&a.ID, &a.Owner, &a.At, &a.Runtime, &a.Door, &a.Action, &a.Method, &a.Host, &a.Session, &a.Window, &a.CardID,
		&a.Shape.Options, &a.Shape.Multi, &a.Shape.Other, &a.Outcome, &a.Reason, &a.Keys, &ms, &a.Number, &a.Snapshot)
	a.Duration = time.Duration(ms) * time.Millisecond
	return a, err
}

// Record implements Store.
//
// The attempt number is counted in the INSERT, under the caller's row-level
// security: the caller's earlier rows on the same card, plus one.
func (p *PG) Record(ctx context.Context, a Attempt) (Attempt, error) {
	a, err := prepare(a)
	if err != nil {
		return Attempt{}, err
	}
	out, err := scan(p.pool.QueryRow(ctx, `INSERT INTO ask_attempts (at, runtime, door, action, method, mirror_host, mirror_session, mirror_window,
			card_id, questions, options, multi_select, other_used, outcome, reason, keys, duration_ms, attempt, snapshot)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
			(SELECT count(*) + 1 FROM ask_attempts WHERE card_id = $9 AND $9 <> ''), $18)
		RETURNING `+cols,
		a.At, a.Runtime, a.Door, a.Action, a.Method, a.Host, a.Session, a.Window, a.CardID, len(a.Shape.Options), a.Shape.Options,
		a.Shape.Multi, a.Shape.Other, a.Outcome, a.Reason, a.Keys, a.Duration.Milliseconds(), a.Snapshot))
	if err != nil {
		return Attempt{}, fmt.Errorf("askattempt: record: %w", err)
	}
	return out, nil
}

func (p *PG) query(ctx context.Context, sql string, args ...any) ([]Attempt, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("askattempt: list: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MaxList bounds List.
const MaxList = 5000

// List implements Store.
func (p *PG) List(ctx context.Context, since time.Time, limit int) ([]Attempt, error) {
	if limit <= 0 || limit > MaxList {
		limit = MaxList
	}
	return p.query(ctx, `SELECT `+cols+` FROM ask_attempts WHERE at >= $1 ORDER BY at DESC, id DESC LIMIT $2`, since, limit)
}

// ForCard implements Store.
func (p *PG) ForCard(ctx context.Context, cardID string) ([]Attempt, error) {
	if cardID == "" {
		return nil, nil
	}
	return p.query(ctx, `SELECT `+cols+` FROM ask_attempts WHERE card_id = $1 ORDER BY id LIMIT 200`, cardID)
}

// Prune implements Store.
func (p *PG) Prune(ctx context.Context, before time.Time) (int, error) {
	tag, err := p.pool.Exec(sys(ctx), `DELETE FROM ask_attempts WHERE at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("askattempt: prune: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Mem is Store in memory, each principal's rows its own (its owner is ctx's
// principal's user id, as row-level security keys a row): for tests, and the
// e2e fixture servers that have no database.
type Mem struct {
	mu   sync.Mutex
	rows []Attempt
	next int64
}

func memOwner(ctx context.Context) int64 {
	if p, ok := authz.From(ctx); ok {
		return p.UserID
	}
	return 0
}

// Record implements Store.
func (m *Mem) Record(ctx context.Context, a Attempt) (Attempt, error) {
	a, err := prepare(a)
	if err != nil {
		return Attempt{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a.Owner = memOwner(ctx)
	m.next++
	a.ID, a.Number = m.next, 1
	for _, r := range m.rows {
		if r.Owner == a.Owner && a.CardID != "" && r.CardID == a.CardID {
			a.Number++
		}
	}
	m.rows = append(m.rows, a)
	return a, nil
}

// List implements Store.
func (m *Mem) List(ctx context.Context, since time.Time, limit int) ([]Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := memOwner(ctx)
	var out []Attempt
	for i := len(m.rows) - 1; i >= 0; i-- {
		if r := m.rows[i]; r.Owner == o && !r.At.Before(since) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ForCard implements Store.
func (m *Mem) ForCard(ctx context.Context, cardID string) ([]Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := memOwner(ctx)
	var out []Attempt
	for _, r := range m.rows {
		if r.Owner == o && cardID != "" && r.CardID == cardID {
			out = append(out, r)
		}
	}
	return out, nil
}

// Prune implements Store.
func (m *Mem) Prune(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.rows[:0]
	n := 0
	for _, r := range m.rows {
		if r.At.Before(before) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	m.rows = kept
	return n, nil
}

// All is every row, every owner's, in order written (tests).
func (m *Mem) All() []Attempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Attempt(nil), m.rows...)
}
