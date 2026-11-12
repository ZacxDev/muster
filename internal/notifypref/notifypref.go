// Package notifypref stores each user's notification preferences (migration
// 0094, notify_preferences). There are two, independent of each other, both on
// Account → Notifications:
//   - TmuxAddress, "Show tmux sessions", which internal/api's name rule reads
//     when a notification names a mirrored session (names.go there,
//     notifyName; docs/focus.md#show-tmux-sessions);
//   - CodenameFirst (migration 0117), "Start notification titles with the
//     tmux name", which internal/api's push title sink reads (server.go
//     goNotify, the title names.go codenameTitle made;
//     docs/focus.md#tmux-name-first-in-push-titles).
//
// 🔴 OFF UNLESS THE USER TURNED IT ON. No row, a row the user wrote off, and a
// read that fails are all off: off is exactly the text every user had before
// the preference existed.
package notifypref

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Store is the per-user notification preferences.
type Store interface {
	// TmuxAddress reports whether owner turned on "Show tmux session in
	// notifications". No row is false.
	TmuxAddress(ctx context.Context, owner int64) (bool, error)
	// SetTmuxAddress records owner's choice.
	SetTmuxAddress(ctx context.Context, owner int64, on bool) error
	// CodenameFirst reports whether owner turned on "Start notification
	// titles with the tmux name". No row is false.
	CodenameFirst(ctx context.Context, owner int64) (bool, error)
	// SetCodenameFirst records owner's choice, leaving TmuxAddress as it is.
	SetCodenameFirst(ctx context.Context, owner int64, on bool) error
}

// ErrNotOwner refuses a read whose ctx can see neither owner's row nor every
// row.
var ErrNotOwner = errors.New("notifypref: a preference is read only as its owner or as the system")

// PG is Store on notify_preferences, under row-level security.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// TmuxAddress implements Store.
//
// 🔴 IT REFUSES A ctx THAT ROW-LEVEL SECURITY WOULD BLIND. A row of owner's
// is invisible to another user's principal and to a principal-less ctx, and
// would read as "no row" — off — for a user who turned it on: their
// notifications would silently lose the address on that path. So only owner's
// own principal or the system scope may read; anything else is an error (the
// caller reads it as off, and logs it, so the path is found).
func (p *PG) TmuxAddress(ctx context.Context, owner int64) (bool, error) {
	return p.read(ctx, owner, `SELECT tmux_address FROM notify_preferences WHERE owner_id = $1`)
}

// CodenameFirst implements Store, refusing what TmuxAddress refuses.
func (p *PG) CodenameFirst(ctx context.Context, owner int64) (bool, error) {
	return p.read(ctx, owner, `SELECT codename_first FROM notify_preferences WHERE owner_id = $1`)
}

// read is one boolean preference of owner's (q selects it by owner_id), as
// owner's own principal or the system scope only: see TmuxAddress.
func (p *PG) read(ctx context.Context, owner int64, q string) (bool, error) {
	pr, ok := authz.From(ctx)
	if !ok || (!pr.All() && pr.UserID != owner) {
		return false, ErrNotOwner
	}
	var on bool
	err := p.pool.QueryRow(ctx, q, owner).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return on, nil
}

// SetTmuxAddress implements Store: the CALLER's row (row-level security
// refuses another owner's).
func (p *PG) SetTmuxAddress(ctx context.Context, owner int64, on bool) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO notify_preferences (owner_id, tmux_address) VALUES ($1, $2)
		ON CONFLICT (owner_id) DO UPDATE SET tmux_address = EXCLUDED.tmux_address, updated_at = now()`, owner, on)
	return err
}

// SetCodenameFirst implements Store: the CALLER's row (row-level security
// refuses another owner's). A new row's tmux_address is its default, off; an
// existing row's is left as it is.
func (p *PG) SetCodenameFirst(ctx context.Context, owner int64, on bool) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO notify_preferences (owner_id, codename_first) VALUES ($1, $2)
		ON CONFLICT (owner_id) DO UPDATE SET codename_first = EXCLUDED.codename_first, updated_at = now()`, owner, on)
	return err
}
