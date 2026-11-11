package branding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func pgStore(t *testing.T) (*PG, string) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'admin') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE instance_branding`)
	return NewPG(pool), dsn
}

func as(uid int64, role authz.Role) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

func strp(s string) *string { return &s }

// images is an Images as Process makes it: its ETag is the ETagOf its bytes,
// which is what the store serves (Stored.Resolve).
func images(logo, i192, i512 string) Images {
	return Images{Logo: []byte(logo), Icon192: []byte(i192), Icon512: []byte(i512), ETag: ETagOf([]byte(logo), []byte(i192), []byte(i512))}
}

// TestStoreReadsNothingSetThenWhatAnAdminWrote: an empty table reads as
// nothing set (to anyone, with or without a user); an admin's name and logo
// are read back whole, by an anonymous context too (the sign-in page has no
// user); a name-only change keeps the logo and a logo-only change keeps the
// name; clearing the logo keeps the name; "" clears the name; updated_by is
// the admin who wrote last.
func TestStoreReadsNothingSetThenWhatAnAdminWrote(t *testing.T) {
	p, _ := pgStore(t)
	admin, admin3 := as(authz.OperatorUserID, authz.RoleAdmin), as(3, authz.RoleAdmin)
	anon := context.Background()
	if b, err := p.Load(anon); err != nil || b.Name != "" || b.HasLogo() || b.Logo != nil {
		t.Fatalf("empty table: %+v err=%v; want nothing set", b, err)
	}
	img := images("L", "I1", "I5")
	b, err := p.Apply(admin, Change{Name: strp("Team Example"), Logo: &img})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "Team Example" || b.ETag != img.ETag || !bytes.Equal(b.Icon512, img.Icon512) || b.UpdatedBy != authz.OperatorUserID {
		t.Fatalf("after set: %+v", b)
	}
	got, err := p.Load(anon)
	if err != nil || got.Name != "Team Example" || !bytes.Equal(got.Logo, []byte("L")) || !bytes.Equal(got.Icon192, []byte("I1")) || got.ETag != img.ETag {
		t.Fatalf("anonymous read: %+v err=%v", got, err)
	}
	if b, err = p.Apply(admin3, Change{Name: strp("Renamed")}); err != nil || b.Name != "Renamed" || b.ETag != img.ETag || b.UpdatedBy != 3 {
		t.Fatalf("name only: %+v err=%v; the logo must stay and updated_by be 3", b, err)
	}
	img2 := images("L2", "J1", "J5")
	if b, err = p.Apply(admin, Change{Logo: &img2}); err != nil || b.Name != "Renamed" || b.ETag != img2.ETag {
		t.Fatalf("logo only: %+v err=%v; the name must stay", b, err)
	}
	if b, err = p.Apply(admin, Change{ClearLogo: true}); err != nil || b.HasLogo() || b.Logo != nil || b.Icon192 != nil || b.Name != "Renamed" {
		t.Fatalf("clear logo: %+v err=%v", b, err)
	}
	if b, err = p.Apply(admin, Change{Name: strp("")}); err != nil || b.Name != "" {
		t.Fatalf("clear name: %+v err=%v", b, err)
	}
}

// TestOnlyAnAdminWrites: a member, an anonymous context and the system scope
// (no user to record) are refused with ErrNotAdmin BEFORE any write, and the
// row is unchanged; an admin is the control. A change that changes nothing,
// or both sets and clears the logo, is refused.
func TestOnlyAnAdminWrites(t *testing.T) {
	p, _ := pgStore(t)
	admin := as(authz.OperatorUserID, authz.RoleAdmin)
	if _, err := p.Apply(admin, Change{Name: strp("Before")}); err != nil {
		t.Fatalf("control: the admin's write: %v", err)
	}
	for name, ctx := range map[string]context.Context{
		"member":    as(2, authz.RoleMember),
		"anonymous": context.Background(),
		"system":    authz.System(context.Background()),
	} {
		if _, err := p.Apply(ctx, Change{Name: strp("Hijacked")}); !errors.Is(err, ErrNotAdmin) {
			t.Errorf("%s: Apply err = %v, want ErrNotAdmin", name, err)
		}
	}
	if b, _ := p.Load(context.Background()); b.Name != "Before" {
		t.Fatalf("a refused write changed the name to %q", b.Name)
	}
	img := images("L", "I", "I")
	for name, c := range map[string]Change{"nothing": {}, "set and clear": {Logo: &img, ClearLogo: true}} {
		if _, err := p.Apply(admin, c); !errors.Is(err, ErrEmptyChange) {
			t.Errorf("%s: err = %v, want ErrEmptyChange", name, err)
		}
	}
}

// TestRowLevelSecurityKeepsUserPrincipalsOut: the table is under forced
// row-level security with the scope_all policy only (0105), so a query run
// with a user's principal — an admin's included — sees no row and writes
// none, while the system scope sees the row. This is the guard for any code
// path OTHER than this store.
func TestRowLevelSecurityKeepsUserPrincipalsOut(t *testing.T) {
	p, _ := pgStore(t)
	admin := as(authz.OperatorUserID, authz.RoleAdmin)
	if _, err := p.Apply(admin, Change{Name: strp("Team Example")}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := p.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM instance_branding`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("control: the system scope sees %d row(s), err=%v", n, err)
	}
	for name, ctx := range map[string]context.Context{"admin": admin, "member": as(2, authz.RoleMember)} {
		if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM instance_branding`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s's principal sees %d row(s), err=%v; want 0", name, n, err)
		}
		tag, err := p.pool.Exec(ctx, `UPDATE instance_branding SET name = 'x'`)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("%s's principal updated %d row(s), err=%v; want 0", name, tag.RowsAffected(), err)
		}
		if _, err := p.pool.Exec(ctx, `INSERT INTO instance_branding (singleton, name) VALUES (true, 'x') ON CONFLICT DO NOTHING`); err == nil {
			t.Errorf("%s's principal inserted a row", name)
		}
	}
}

// etagOver is the ETag a server must serve for these bytes, computed here
// with crypto/sha256 rather than ETagOf, so the store's choice of WHICH bytes
// it hashes is what the assertions pin.
func etagOver(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])[:16]
}

// TestAnAppIconReplacesTheServedIcons: with a logo (icons I1/I5), an app icon
// (A1/A5) becomes the icons Load serves, and the ETag is over the logo and
// THOSE icons; a new logo keeps the app icon; removing it serves the logo's
// own icons again, under their own ETag; resetting the logo removes the app
// icon too. An app icon on a server with no logo is refused before any write,
// unless the logo comes in the same change, and an app icon with a logo reset
// is a contradiction.
func TestAnAppIconReplacesTheServedIcons(t *testing.T) {
	p, _ := pgStore(t)
	admin := as(authz.OperatorUserID, authz.RoleAdmin)
	anon := context.Background()
	icon := &AppIcon{Icon192: []byte("A1"), Icon512: []byte("A5")}
	served := func(when, i192, i512, etag string, appIcon bool) {
		t.Helper()
		b, err := p.Load(anon)
		if err != nil || string(b.Icon192) != i192 || string(b.Icon512) != i512 || b.ETag != etag || b.HasAppIcon != appIcon {
			t.Fatalf("%s: icons %q/%q etag %s app icon %v (err %v); want %q/%q %s %v",
				when, b.Icon192, b.Icon512, b.ETag, b.HasAppIcon, err, i192, i512, etag, appIcon)
		}
	}

	if _, err := p.Apply(admin, Change{AppIcon: icon}); !errors.Is(err, ErrAppIconNeedsLogo) {
		t.Fatalf("an app icon with no logo: err = %v, want ErrAppIconNeedsLogo", err)
	}
	if b, _ := p.Load(anon); b.HasAppIcon || b.HasLogo() || !b.UpdatedAt.IsZero() {
		t.Fatalf("the refused app icon wrote a row: %+v", b)
	}
	if _, err := p.Apply(admin, Change{Logo: ptr(images("L", "I1", "I5"))}); err != nil {
		t.Fatal(err)
	}
	served("the logo alone", "I1", "I5", etagOver("L", "I1", "I5"), false)

	b, err := p.Apply(admin, Change{AppIcon: icon})
	if err != nil || string(b.Icon192) != "A1" || b.ETag != etagOver("L", "A1", "A5") {
		t.Fatalf("Apply's own answer with the app icon: %+v err=%v", b, err)
	}
	served("with the app icon", "A1", "A5", etagOver("L", "A1", "A5"), true)
	if b, _ := p.Load(anon); string(b.Logo) != "L" {
		t.Fatalf("the app icon changed the logo to %q", b.Logo)
	}

	if _, err := p.Apply(admin, Change{Logo: ptr(images("L2", "J1", "J5"))}); err != nil {
		t.Fatal(err)
	}
	served("a new logo under the app icon", "A1", "A5", etagOver("L2", "A1", "A5"), true)

	if _, err := p.Apply(admin, Change{ClearAppIcon: true}); err != nil {
		t.Fatal(err)
	}
	served("the app icon removed", "J1", "J5", etagOver("L2", "J1", "J5"), false)

	if _, err := p.Apply(admin, Change{AppIcon: icon}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(admin, Change{ClearLogo: true}); err != nil {
		t.Fatal(err)
	}
	served("the logo reset", "", "", "", false)
	if _, err := p.Apply(admin, Change{Logo: ptr(images("L3", "K1", "K5"))}); err != nil {
		t.Fatal(err)
	}
	served("a logo after the reset", "K1", "K5", etagOver("L3", "K1", "K5"), false)

	if _, err := p.Apply(admin, Change{ClearLogo: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(admin, Change{Logo: ptr(images("L4", "M1", "M5")), AppIcon: icon}); err != nil {
		t.Fatalf("a logo and an app icon together: %v", err)
	}
	served("a logo and an app icon together", "A1", "A5", etagOver("L4", "A1", "A5"), true)

	for name, c := range map[string]Change{
		"set and clear the app icon":      {AppIcon: icon, ClearAppIcon: true},
		"an app icon with the logo reset": {AppIcon: icon, ClearLogo: true},
	} {
		if _, err := p.Apply(admin, c); !errors.Is(err, ErrEmptyChange) {
			t.Errorf("%s: err = %v, want ErrEmptyChange", name, err)
		}
	}
	if _, err := p.Apply(as(2, authz.RoleMember), Change{ClearAppIcon: true}); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("a member removing the app icon: err = %v, want ErrNotAdmin", err)
	}
	served("after the refusals", "A1", "A5", etagOver("L4", "A1", "A5"), true)
}

// TestLogoShowsNameIsKeptUntilChanged: false with nothing set; true once an
// admin says so; kept by a name-only change, a new logo, and a logo reset (it
// is the admin's statement, and means nothing until a logo is set again); a
// change that sets only it is a change; false clears it.
func TestLogoShowsNameIsKeptUntilChanged(t *testing.T) {
	p, _ := pgStore(t)
	admin := as(authz.OperatorUserID, authz.RoleAdmin)
	shows := func(when string, want bool) {
		t.Helper()
		if b, err := p.Load(context.Background()); err != nil || b.LogoShowsName != want {
			t.Fatalf("%s: LogoShowsName = %v (err %v), want %v", when, b.LogoShowsName, err, want)
		}
	}
	shows("nothing set", false)
	yes, no := true, false
	if _, err := p.Apply(admin, Change{LogoShowsName: &yes}); err != nil {
		t.Fatalf("a change of only logo_shows_name: %v", err)
	}
	shows("set alone", true)
	if _, err := p.Apply(admin, Change{Name: strp("Team Example"), Logo: ptr(images("L", "I1", "I5"))}); err != nil {
		t.Fatal(err)
	}
	shows("after a name and a logo", true)
	if _, err := p.Apply(admin, Change{ClearLogo: true}); err != nil {
		t.Fatal(err)
	}
	shows("after a logo reset", true)
	if _, err := p.Apply(admin, Change{LogoShowsName: &no}); err != nil {
		t.Fatal(err)
	}
	shows("cleared", false)
	if b, _ := p.Load(context.Background()); b.Name != "Team Example" {
		t.Fatalf("clearing logo_shows_name changed the name to %q", b.Name)
	}
}

func ptr[T any](v T) *T { return &v }
