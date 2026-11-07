package agentoutcome

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store writes an agent's outcome columns (migration 0088). Reads come with the
// row itself (agents.Agent's Outcome fields, scanned by agents.PGStore).
//
// Every method acts as ctx's principal: agents is under row-level security, so a
// background caller passes authz.System and a request passes its own.
type Store interface {
	// Start marks agent id as started on a first turn: outcome running, the
	// clocks at now, and the chief that started it (nil for another door). It
	// re-arms an agent that ended (a new dispatch is a new piece of work).
	Start(ctx context.Context, id int64, dispatchedBy *int64, now time.Time) error
	// Activity records activity at now. An agent that is stuck recovers (its
	// outcome goes back to running); recovered reports whether that happened.
	// An untracked agent gets its activity time and nothing else.
	Activity(ctx context.Context, id int64, now time.Time) (recovered bool, err error)
	// Replied records the agent's first reply at now (kept when one is
	// recorded already), and activity.
	Replied(ctx context.Context, id int64, now time.Time) error
	// Transition moves the outcome to `to` with reason at now when — and only
	// when — the stored outcome is one of from. applied is whether it moved:
	// the caller that pushes is the one whose write applied.
	Transition(ctx context.Context, id int64, from []string, to, reason string, now time.Time) (applied bool, err error)
	// MarkStuck moves a running agent to stuck with reason at now when — and
	// only when — it is still running AND its activity is still seen (the value
	// the verdict was taken on; nil for none): activity that landed after the
	// read wins, and nothing is said.
	MarkStuck(ctx context.Context, id int64, reason string, now time.Time, seen *time.Time) (applied bool, err error)
	// Fail records a failed turn: outcome failed with reason (from any outcome
	// but failed), and — when setStatus — status `error` with reason as its
	// error_message, which moves updated_at (a real transition). applied is
	// whether the OUTCOME moved (the one push); the status is written either way.
	Fail(ctx context.Context, id int64, reason string, setStatus bool, now time.Time) (applied bool, err error)
	// Clear makes the agent untracked again ("" outcome) when its outcome is one
	// of from: a restart after a failure, or a chief's own recovery.
	Clear(ctx context.Context, id int64, from []string) (bool, error)
}

// PGStore is the Postgres Store.
type PGStore struct{ pool querier }

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewPG is a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

func (s *PGStore) Start(ctx context.Context, id int64, dispatchedBy *int64, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE agents SET outcome = 'running', outcome_reason = '', outcome_at = $2,
		activity_at = $2, first_reply_at = NULL, dispatched_by = COALESCE($3, dispatched_by) WHERE id = $1`, id, now, dispatchedBy)
	return err
}

func (s *PGStore) Activity(ctx context.Context, id int64, now time.Time) (bool, error) {
	// The old outcome is read in the same statement (the UPDATE's FROM sees the
	// row before it), so "it was stuck" and "it is running now" are one write.
	var was string
	err := s.pool.QueryRow(ctx, `UPDATE agents a SET
			activity_at = GREATEST(COALESCE(a.activity_at, $2), $2),
			outcome = CASE WHEN a.outcome = 'stuck' THEN 'running' ELSE a.outcome END,
			outcome_reason = CASE WHEN a.outcome = 'stuck' THEN '' ELSE a.outcome_reason END,
			outcome_at = CASE WHEN a.outcome = 'stuck' THEN $2 ELSE a.outcome_at END
		FROM (SELECT id, outcome FROM agents WHERE id = $1 FOR UPDATE) old
		WHERE a.id = old.id
		RETURNING old.outcome`, id, now).Scan(&was)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil && was == Stuck, err
}

func (s *PGStore) Replied(ctx context.Context, id int64, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE agents SET first_reply_at = COALESCE(first_reply_at, $2),
		activity_at = GREATEST(COALESCE(activity_at, $2), $2) WHERE id = $1`, id, now)
	return err
}

func (s *PGStore) Transition(ctx context.Context, id int64, from []string, to, reason string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE agents SET outcome = $3, outcome_reason = $4, outcome_at = $5
		WHERE id = $1 AND outcome = ANY($2)`, id, from, to, reason, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PGStore) MarkStuck(ctx context.Context, id int64, reason string, now time.Time, seen *time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE agents SET outcome = 'stuck', outcome_reason = $2, outcome_at = $3
		WHERE id = $1 AND outcome = 'running' AND activity_at IS NOT DISTINCT FROM $4`, id, reason, now, seen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PGStore) Fail(ctx context.Context, id int64, reason string, setStatus bool, now time.Time) (bool, error) {
	var applied bool
	err := s.pool.QueryRow(ctx, `UPDATE agents a SET
			outcome = 'failed', outcome_reason = $2,
			outcome_at = CASE WHEN old.outcome = 'failed' THEN a.outcome_at ELSE $4 END,
			status = CASE WHEN $3 THEN 'error' ELSE a.status END,
			error_message = CASE WHEN $3 THEN $2 ELSE a.error_message END,
			updated_at = CASE WHEN $3 AND a.status <> 'error' THEN now() ELSE a.updated_at END
		FROM (SELECT id, outcome FROM agents WHERE id = $1 FOR UPDATE) old
		WHERE a.id = old.id
		RETURNING old.outcome <> 'failed'`, id, reason, setStatus, now).Scan(&applied)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return applied, err
}

func (s *PGStore) Clear(ctx context.Context, id int64, from []string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE agents SET outcome = '', outcome_reason = '' WHERE id = $1 AND outcome = ANY($2)`, id, from)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
