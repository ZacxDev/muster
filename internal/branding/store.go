package branding

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Branding is the instance's branding as stored. A zero value is "nothing
// set": no name and no logo.
type Branding struct {
	Name string
	// Logo, Icon192 and Icon512 are nil, together, when no logo is set.
	Logo, Icon192, Icon512 []byte
	// ETag is "" when no logo is set (Images.ETag otherwise).
	ETag      string
	UpdatedAt time.Time
	UpdatedBy int64
}

// HasLogo reports whether a logo is set.
func (b Branding) HasLogo() bool { return b.ETag != "" }

// Change is one write. A nil field is left as it is.
type Change struct {
	// Name, when set, replaces the name; "" clears it. It must already be
	// NormalizeName's output.
	Name *string
	// Logo, when set, replaces the logo and icons.
	Logo *Images
	// ClearLogo removes the logo and icons. It and Logo are exclusive.
	ClearLogo bool
}

// Store keeps the instance's branding.
type Store interface {
	// Load is the branding; a zero Branding when nothing was ever set.
	Load(ctx context.Context) (Branding, error)
	// Apply writes c as the admin ctx names, and returns the result.
	Apply(ctx context.Context, c Change) (Branding, error)
}

// ErrNotAdmin refuses a write whose ctx is not an admin's (authz.IsAdmin).
var ErrNotAdmin = errors.New("branding: only an admin may change the instance's branding")

// ErrEmptyChange refuses a Change that changes nothing, and one that both
// sets and clears the logo.
var ErrEmptyChange = errors.New("branding: a change must set the name, the logo, or clear the logo (and not both set and clear it)")

// PG is Store on instance_branding (migration 0105).
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

const selectBranding = `SELECT coalesce(name, ''), logo_png, icon192_png, icon512_png, coalesce(logo_etag, ''),
	updated_at, coalesce(updated_by, 0) FROM instance_branding WHERE singleton`

// Load implements Store. It reads as authz.System: the branding is the
// server's, read on the sign-in page and by the public /brand/ images, where
// there is no user, and row-level security admits only the system scope
// (0105).
func (p *PG) Load(ctx context.Context) (Branding, error) {
	b, err := scan(p.pool.QueryRow(authz.System(ctx), selectBranding))
	if errors.Is(err, pgx.ErrNoRows) {
		return Branding{}, nil
	}
	return b, err
}

// Apply implements Store.
//
// 🔴 IT CHECKS THE CALLER IS AN ADMIN BEFORE IT ACTS AS THE SYSTEM. The
// write needs the system scope (row-level security admits nothing else), and
// a store that elevated any caller would make the route's admin check the
// only one; this keeps a handler wired without it from writing.
func (p *PG) Apply(ctx context.Context, c Change) (Branding, error) {
	if !authz.IsAdmin(ctx) {
		return Branding{}, ErrNotAdmin
	}
	if (c.Name == nil && c.Logo == nil && !c.ClearLogo) || (c.Logo != nil && c.ClearLogo) {
		return Branding{}, ErrEmptyChange
	}
	by := authz.MustFrom(ctx).UserID
	var name *string
	if c.Name != nil && *c.Name != "" {
		name = c.Name
	}
	var logo, i192, i512 []byte
	var etag *string
	if c.Logo != nil {
		logo, i192, i512, etag = c.Logo.Logo, c.Logo.Icon192, c.Logo.Icon512, &c.Logo.ETag
	}
	return scan(p.pool.QueryRow(authz.System(ctx), `
		INSERT INTO instance_branding AS b (singleton, name, logo_png, icon192_png, icon512_png, logo_etag, updated_by)
		VALUES (true, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (singleton) DO UPDATE SET
			name        = CASE WHEN $1::int & 1 = 1 THEN EXCLUDED.name ELSE b.name END,
			logo_png    = CASE WHEN $1::int & 2 = 2 THEN EXCLUDED.logo_png ELSE b.logo_png END,
			icon192_png = CASE WHEN $1::int & 2 = 2 THEN EXCLUDED.icon192_png ELSE b.icon192_png END,
			icon512_png = CASE WHEN $1::int & 2 = 2 THEN EXCLUDED.icon512_png ELSE b.icon512_png END,
			logo_etag   = CASE WHEN $1::int & 2 = 2 THEN EXCLUDED.logo_etag ELSE b.logo_etag END,
			updated_at  = now(),
			updated_by  = EXCLUDED.updated_by
		RETURNING coalesce(name, ''), logo_png, icon192_png, icon512_png, coalesce(logo_etag, ''),
			updated_at, coalesce(updated_by, 0)`,
		changeMask(c), name, logo, i192, i512, etag, by))
}

// changeMask is which column groups c writes: 1 the name, 2 the logo (set or
// cleared — a clear writes the NULLs the VALUES row carries).
func changeMask(c Change) int {
	m := 0
	if c.Name != nil {
		m |= 1
	}
	if c.Logo != nil || c.ClearLogo {
		m |= 2
	}
	return m
}

func scan(row pgx.Row) (Branding, error) {
	var b Branding
	err := row.Scan(&b.Name, &b.Logo, &b.Icon192, &b.Icon512, &b.ETag, &b.UpdatedAt, &b.UpdatedBy)
	return b, err
}
