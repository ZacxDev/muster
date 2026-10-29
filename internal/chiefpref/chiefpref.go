// Package chiefpref stores each user's choice to turn their chief on or off
// (migration 0060, chief_preferences). It stores only the CHOICE: whether the
// chief is on for a user who never chose is internal/api's rule (chiefOn), so
// the default lives in one place.
package chiefpref

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Store is the per-user chief toggle.
type Store interface {
	// Choice is owner's stored choice: chose is false when owner never
	// turned the chief on or off (no row), and on is then meaningless.
	Choice(ctx context.Context, owner int64) (chose, on bool, err error)
	// SetEnabled records owner's choice.
	SetEnabled(ctx context.Context, owner int64, on bool) error
}

// ErrNotOwner refuses a read whose ctx can see neither owner's row nor every
// row.
var ErrNotOwner = errors.New("chiefpref: the toggle is read only as its owner or as the system")

// PG is Store on chief_preferences, under row-level security.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// Choice implements Store.
//
// 🔴 IT REFUSES A ctx THAT ROW-LEVEL SECURITY WOULD BLIND. A row of owner's is
// invisible to any other user's principal and to a principal-less ctx, and
// an invisible row reads as "never chose", which falls back to "on if a chief
// agent exists": a user who turned their chief OFF would get it back on any
// path that read the toggle with the wrong principal. So only owner's own
// principal or the system scope may read, and anything else is an error the
// caller reads as off.
func (p *PG) Choice(ctx context.Context, owner int64) (bool, bool, error) {
	pr, ok := authz.From(ctx)
	if !ok || (!pr.All() && pr.UserID != owner) {
		return false, false, ErrNotOwner
	}
	var on bool
	err := p.pool.QueryRow(ctx, `SELECT enabled FROM chief_preferences WHERE owner_id = $1`, owner).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, on, nil
}

// SetEnabled implements Store: the CALLER's row (row-level security refuses
// another owner's).
func (p *PG) SetEnabled(ctx context.Context, owner int64, on bool) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO chief_preferences (owner_id, enabled) VALUES ($1, $2)
		ON CONFLICT (owner_id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`, owner, on)
	return err
}
