package officers

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGRestartStore is RestartStore on officer_auto_restarts (migration 0167),
// under row-level security: ctx must act as the officer's owner
// (Config.RestartContext).
type PGRestartStore struct{ pool *pgxpool.Pool }

// NewPGRestartStore is the store over pool.
func NewPGRestartStore(pool *pgxpool.Pool) *PGRestartStore { return &PGRestartStore{pool: pool} }

var _ RestartStore = (*PGRestartStore)(nil)

// RestartDay implements RestartStore.
func (s *PGRestartStore) RestartDay(ctx context.Context, officer, day string) (map[string]RestartRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance, restarts, last_restart_at, held_told, last_error
		FROM officer_auto_restarts WHERE officer = $1 AND day = $2::date`, officer, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RestartRecord{}
	for rows.Next() {
		var name string
		var rec RestartRecord
		var at *time.Time
		if err := rows.Scan(&name, &rec.Restarts, &at, &rec.HeldTold, &rec.LastError); err != nil {
			return nil, err
		}
		if at != nil {
			rec.LastRestartAt = *at
		}
		out[name] = rec
	}
	return out, rows.Err()
}

// RecordRestart implements RestartStore: one more restart, in one statement
// (an upsert adding 1), so two replicas restarting at once both count.
func (s *PGRestartStore) RecordRestart(ctx context.Context, officer, day, instance string, at time.Time, errText string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_auto_restarts (officer, day, instance, restarts, last_restart_at, last_error)
		VALUES ($1, $2::date, $3, 1, $4, $5)
		ON CONFLICT (owner_id, officer, day, instance) DO UPDATE
		SET restarts = officer_auto_restarts.restarts + 1, last_restart_at = $4, last_error = $5, updated_at = now()`,
		officer, day, instance, at, truncateRunes(errText, MaxRestartErrorText))
	return err
}

// SetRestartHeld implements RestartStore. Clearing keeps the row's last
// error: it is the record of the episode that ended.
func (s *PGRestartStore) SetRestartHeld(ctx context.Context, officer, day, instance string, held bool, errText string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_auto_restarts (officer, day, instance, held_told, last_error)
		VALUES ($1, $2::date, $3, $4, $5)
		ON CONFLICT (owner_id, officer, day, instance) DO UPDATE
		SET held_told = $4, last_error = CASE WHEN $4 THEN $5 ELSE officer_auto_restarts.last_error END, updated_at = now()`,
		officer, day, instance, held, truncateRunes(errText, MaxRestartErrorText))
	return err
}
