package suggest

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// PGPrefs is Prefs on suggest_preferences (migration 0048), under row-level
// security: ctx must act as the owner it names (a request's, or the rendered
// view socket's, principal), and a row of another owner's is invisible.
type PGPrefs struct{ pool *pgxpool.Pool }

// ErrNotOwner refuses a toggle read whose ctx does not act as the owner.
var ErrNotOwner = errors.New("suggest: the toggle is read only as its owner")

// NewPGPrefs is Prefs over pool.
func NewPGPrefs(pool *pgxpool.Pool) *PGPrefs { return &PGPrefs{pool: pool} }

// Choice implements Prefs: a row is the user's saved choice, on or off; no
// row is saved=false (the engine gives it DefaultDisabled).
//
// 🔴 FAILS CLOSED WITHOUT THE OWNER'S PRINCIPAL. Row-level security hides a
// row from any other principal, and a hidden row would read as "no row" — the
// default, whatever the user saved. So a ctx that does not act as owner is an
// error, which the engine reads as off.
func (p *PGPrefs) Choice(ctx context.Context, owner int64) (disabled, saved bool, err error) {
	if pr, ok := authz.From(ctx); !ok || pr.UserID != owner {
		return false, false, ErrNotOwner
	}
	err = p.pool.QueryRow(ctx, `SELECT disabled FROM suggest_preferences WHERE owner_id = $1`, owner).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return disabled, true, nil
}

// SetDisabled implements Prefs: the CALLER's row (owner_id defaults to the
// principal; RLS refuses another owner's).
func (p *PGPrefs) SetDisabled(ctx context.Context, owner int64, disabled bool) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO suggest_preferences (owner_id, disabled) VALUES ($1, $2)
		ON CONFLICT (owner_id) DO UPDATE SET disabled = EXCLUDED.disabled, updated_at = now()`, owner, disabled)
	return err
}
