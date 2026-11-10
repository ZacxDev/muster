package branding

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
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
	img := Images{Logo: []byte("L"), Icon192: []byte("I1"), Icon512: []byte("I5"), ETag: "0123456789abcdef"}
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
	img2 := Images{Logo: []byte("L2"), Icon192: []byte("J1"), Icon512: []byte("J5"), ETag: "fedcba9876543210"}
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
	img := Images{Logo: []byte("L"), Icon192: []byte("I"), Icon512: []byte("I"), ETag: "0123456789abcdef"}
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
