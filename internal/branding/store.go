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
	// Icon192 and Icon512 are the icons muster SERVES: the app icon's when one
	// is set (HasAppIcon), the logo's otherwise.
	Logo, Icon192, Icon512 []byte
	// ETag is "" when no logo is set, else the ETagOf the logo and the served
	// icons — so it changes when the app icon is set or removed, too.
	ETag string
	// HasAppIcon reports that the served icons were drawn from a separate app
	// icon upload (ProcessIcon), not from the logo (migration 0113).
	HasAppIcon bool
	// LogoShowsName is the admin's statement that the logo already spells the
	// name (a wordmark), so the pages draw the logo alone (migration 0113). It
	// is kept when the logo is reset, and means nothing while no logo is set.
	LogoShowsName bool
	UpdatedAt     time.Time
	UpdatedBy     int64
}

// HasLogo reports whether a logo is set.
func (b Branding) HasLogo() bool { return b.ETag != "" }

// Change is one write. A nil field is left as it is.
type Change struct {
	// Name, when set, replaces the name; "" clears it. It must already be
	// NormalizeName's output.
	Name *string
	// Logo, when set, replaces the logo and the icons drawn from it. An app
	// icon already set stays, and is still what is served.
	Logo *Images
	// ClearLogo removes the logo, its icons and the app icon. It and Logo are
	// exclusive, and so are it and AppIcon.
	ClearLogo bool
	// AppIcon, when set, replaces the served icons with these. It needs a
	// logo: one already set, or Logo in the same change (ErrAppIconNeedsLogo).
	AppIcon *AppIcon
	// ClearAppIcon removes the app icon, so the logo's icons are served
	// again. It and AppIcon are exclusive.
	ClearAppIcon bool
	// LogoShowsName, when set, replaces Branding.LogoShowsName.
	LogoShowsName *bool
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
// sets and clears the logo or the app icon, or sets an app icon while
// clearing the logo.
var ErrEmptyChange = errors.New("branding: a change must set or clear something (and not both set and clear the logo or the app icon)")

// ErrAppIconNeedsLogo refuses an app icon on a server with no logo: the app
// icon replaces the logo's icons, it is not a brand on its own.
var ErrAppIconNeedsLogo = &Error{"app_icon_needs_logo", "an app icon needs a logo: upload a logo first, or both together"}

// Empty reports whether c changes nothing or contradicts itself
// (ErrEmptyChange).
func (c Change) Empty() bool {
	nothing := c.Name == nil && c.Logo == nil && !c.ClearLogo && c.AppIcon == nil && !c.ClearAppIcon && c.LogoShowsName == nil
	return nothing || (c.Logo != nil && c.ClearLogo) || (c.AppIcon != nil && c.ClearAppIcon) || (c.AppIcon != nil && c.ClearLogo)
}

// Stored is the row as written: the logo's own icons and the app icon's kept
// apart. Resolve is the Branding it serves.
type Stored struct {
	Name                           string
	Logo, LogoIcon192, LogoIcon512 []byte
	AppIcon192, AppIcon512         []byte
	LogoShowsName                  bool
}

// Next is s with c applied, or ErrAppIconNeedsLogo. It is the one statement
// of what each Change field does, used by PG.Apply (and by test stores).
func (s Stored) Next(c Change) (Stored, error) {
	if c.Name != nil {
		s.Name = *c.Name
	}
	if c.LogoShowsName != nil {
		s.LogoShowsName = *c.LogoShowsName
	}
	if c.ClearLogo {
		s.Logo, s.LogoIcon192, s.LogoIcon512, s.AppIcon192, s.AppIcon512 = nil, nil, nil, nil, nil
	}
	if c.Logo != nil {
		s.Logo, s.LogoIcon192, s.LogoIcon512 = c.Logo.Logo, c.Logo.Icon192, c.Logo.Icon512
	}
	if c.ClearAppIcon {
		s.AppIcon192, s.AppIcon512 = nil, nil
	}
	if c.AppIcon != nil {
		if s.Logo == nil {
			return Stored{}, ErrAppIconNeedsLogo
		}
		s.AppIcon192, s.AppIcon512 = c.AppIcon.Icon192, c.AppIcon.Icon512
	}
	return s, nil
}

// Resolve is the Branding s serves: the app icon's icons when it has one,
// the logo's otherwise, and the ETag over the logo and THOSE icons.
func (s Stored) Resolve() Branding {
	b := Branding{Name: s.Name, LogoShowsName: s.LogoShowsName}
	if s.Logo == nil {
		return b
	}
	b.Logo, b.Icon192, b.Icon512 = s.Logo, s.LogoIcon192, s.LogoIcon512
	if s.AppIcon192 != nil {
		b.Icon192, b.Icon512, b.HasAppIcon = s.AppIcon192, s.AppIcon512, true
	}
	b.ETag = ETagOf(b.Logo, b.Icon192, b.Icon512)
	return b
}

// PG is Store on instance_branding (migrations 0105, 0113).
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

const selectBranding = `SELECT coalesce(name, ''), logo_png,
	coalesce(appicon192_png, icon192_png), coalesce(appicon512_png, icon512_png), coalesce(logo_etag, ''),
	appicon192_png IS NOT NULL, logo_shows_name, updated_at, coalesce(updated_by, 0)
	FROM instance_branding WHERE singleton`

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

// Apply implements Store: it reads the row FOR UPDATE, applies c in Go
// (Stored.Next), and writes the whole row back with the ETag of what it now
// serves (Stored.Resolve), in one transaction.
//
// 🔴 IT CHECKS THE CALLER IS AN ADMIN BEFORE IT ACTS AS THE SYSTEM. The
// write needs the system scope (row-level security admits nothing else), and
// a store that elevated any caller would make the route's admin check the
// only one; this keeps a handler wired without it from writing.
func (p *PG) Apply(ctx context.Context, c Change) (Branding, error) {
	if !authz.IsAdmin(ctx) {
		return Branding{}, ErrNotAdmin
	}
	if c.Empty() {
		return Branding{}, ErrEmptyChange
	}
	by := authz.MustFrom(ctx).UserID
	sctx := authz.System(ctx)
	tx, err := p.pool.Begin(sctx)
	if err != nil {
		return Branding{}, err
	}
	defer func() { _ = tx.Rollback(sctx) }()

	var cur Stored
	err = tx.QueryRow(sctx, `SELECT coalesce(name, ''), logo_png, icon192_png, icon512_png, appicon192_png, appicon512_png, logo_shows_name
		FROM instance_branding WHERE singleton FOR UPDATE`).
		Scan(&cur.Name, &cur.Logo, &cur.LogoIcon192, &cur.LogoIcon512, &cur.AppIcon192, &cur.AppIcon512, &cur.LogoShowsName)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Branding{}, err
	}
	next, err := cur.Next(c)
	if err != nil {
		return Branding{}, err
	}
	served := next.Resolve()
	var name, etag *string
	if next.Name != "" {
		name = &next.Name
	}
	if served.ETag != "" {
		etag = &served.ETag
	}
	if _, err := tx.Exec(sctx, `
		INSERT INTO instance_branding AS b (singleton, name, logo_png, icon192_png, icon512_png, appicon192_png, appicon512_png,
			logo_etag, logo_shows_name, updated_by)
		VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (singleton) DO UPDATE SET
			name = EXCLUDED.name, logo_png = EXCLUDED.logo_png,
			icon192_png = EXCLUDED.icon192_png, icon512_png = EXCLUDED.icon512_png,
			appicon192_png = EXCLUDED.appicon192_png, appicon512_png = EXCLUDED.appicon512_png,
			logo_etag = EXCLUDED.logo_etag, logo_shows_name = EXCLUDED.logo_shows_name,
			updated_at = now(), updated_by = EXCLUDED.updated_by`,
		name, next.Logo, next.LogoIcon192, next.LogoIcon512, next.AppIcon192, next.AppIcon512,
		etag, next.LogoShowsName, by); err != nil {
		return Branding{}, err
	}
	b, err := scan(tx.QueryRow(sctx, selectBranding))
	if err != nil {
		return Branding{}, err
	}
	if err := tx.Commit(sctx); err != nil {
		return Branding{}, err
	}
	return b, nil
}

func scan(row pgx.Row) (Branding, error) {
	var b Branding
	err := row.Scan(&b.Name, &b.Logo, &b.Icon192, &b.Icon512, &b.ETag, &b.HasAppIcon, &b.LogoShowsName, &b.UpdatedAt, &b.UpdatedBy)
	return b, err
}
