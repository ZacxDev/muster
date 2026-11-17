package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// THE FLEET DIGEST'S MARKS (migration 0147). Every method runs as ctx's
// principal: fleet_owner_marks is under FORCE row-level security, so an owner
// reads and writes only its own row. (The fleet's action log is the plan's
// Phase 2.)

// LookGap is how long the owner must have been away for an open of the panel
// to count as a NEW look: opens closer together than this are one look, so the
// "since you last looked" line does not reset every time the panel is
// reopened within a few minutes.
const LookGap = 10 * time.Minute

// Store is the digest's marks.
type Store interface {
	// Look records a look at the digest at at and returns the baseline the
	// card compares against: the previous look, when the one before at is
	// LookGap or more ago (it then becomes the baseline), else the baseline
	// kept. Zero: no look before this one.
	Look(ctx context.Context, at time.Time) (time.Time, error)
	// Baseline is the stored baseline, without recording a look.
	Baseline(ctx context.Context) (time.Time, error)
	// FleetChat is the principal's Fleet side chat's session id (0: none).
	FleetChat(ctx context.Context) (int64, error)
	// FleetChatOr is the principal's Fleet side chat: the recorded one while
	// keep(id) holds, else a new one create makes, recorded. 🔴 ONE CHAT
	// HOWEVER MANY CALLERS RACE: the owner's marks row is locked (FOR UPDATE)
	// from the read to the record, so a second caller waits, then reads the
	// chat the first recorded instead of making another.
	FleetChatOr(ctx context.Context, keep func(id int64) bool, create func() (int64, error)) (int64, error)
}

// PG is Store on Postgres.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// Look implements Store.
func (p *PG) Look(ctx context.Context, at time.Time) (time.Time, error) {
	var seen *time.Time
	err := p.pool.QueryRow(ctx, `INSERT INTO fleet_owner_marks (looked_at) VALUES ($1)
		ON CONFLICT (owner_id) DO UPDATE SET
			seen_at = CASE WHEN fleet_owner_marks.looked_at IS NOT NULL AND $1 - fleet_owner_marks.looked_at >= make_interval(secs => $2)
				THEN fleet_owner_marks.looked_at ELSE fleet_owner_marks.seen_at END,
			looked_at = greatest($1, fleet_owner_marks.looked_at), updated_at = now()
		RETURNING seen_at`, at, LookGap.Seconds()).Scan(&seen)
	if err != nil || seen == nil {
		return time.Time{}, err
	}
	return *seen, nil
}

// Baseline implements Store.
func (p *PG) Baseline(ctx context.Context) (time.Time, error) {
	var seen *time.Time
	err := p.pool.QueryRow(ctx, `SELECT seen_at FROM fleet_owner_marks`).Scan(&seen)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && seen == nil) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return *seen, nil
}

// FleetChat implements Store.
func (p *PG) FleetChat(ctx context.Context) (int64, error) {
	var id *int64
	err := p.pool.QueryRow(ctx, `SELECT fleet_chat_id FROM fleet_owner_marks`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && id == nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return *id, nil
}

// FleetChatOr implements Store.
func (p *PG) FleetChatOr(ctx context.Context, keep func(id int64) bool, create func() (int64, error)) (int64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The row first (a first-ever caller has none to lock), then the lock.
	if _, err := tx.Exec(ctx, `INSERT INTO fleet_owner_marks DEFAULT VALUES ON CONFLICT (owner_id) DO NOTHING`); err != nil {
		return 0, err
	}
	var cur *int64
	if err := tx.QueryRow(ctx, `SELECT fleet_chat_id FROM fleet_owner_marks FOR UPDATE`).Scan(&cur); err != nil {
		return 0, err
	}
	if cur != nil && *cur > 0 && keep(*cur) {
		return *cur, tx.Commit(ctx)
	}
	id, err := create()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE fleet_owner_marks SET fleet_chat_id = $1, updated_at = now()`, id); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}
