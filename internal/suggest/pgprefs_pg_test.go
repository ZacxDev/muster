package suggest

import (
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

func pgPrefs(t *testing.T) *PGPrefs {
	t.Helper()
	p, _ := pgPrefsDSN(t)
	return p
}

// pgPrefsDSN is pgPrefs and its database's DSN (for a row written as the
// table's owner, as an older install's saved choice is).
func pgPrefsDSN(t *testing.T) (*PGPrefs, string) {
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
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member'), (4, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE suggest_preferences`)
	return NewPGPrefs(pool), dsn
}

func asUser(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

// TestTheToggleIsOffByDefaultAndPerUser: no row is OFF (DefaultDisabled);
// one user's ON is theirs alone — it never turns another user's on; another
// user can neither read nor write it (row-level security); off again is off.
func TestTheToggleIsOffByDefaultAndPerUser(t *testing.T) {
	p := pgPrefs(t)
	for _, u := range []int64{2, 3} {
		if off, err := p.Disabled(asUser(u), u); err != nil || !off {
			t.Fatalf("user %d, never chose: off=%v err=%v (want off)", u, off, err)
		}
	}
	if err := p.SetDisabled(asUser(2), 2, false); err != nil {
		t.Fatal(err)
	}
	if off, err := p.Disabled(asUser(2), 2); err != nil || off {
		t.Fatalf("user 2 turned it on: off=%v err=%v", off, err)
	}
	if off, err := p.Disabled(asUser(3), 3); err != nil || !off {
		t.Fatalf("user 2's ON reached user 3: off=%v err=%v", off, err)
	}
	// User 3 reading user 2's row is refused before the query (RLS would hide
	// the row, which would read as the default): the engine reads the error
	// as off.
	if _, err := p.Disabled(asUser(3), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("user 3 read user 2's toggle: err=%v", err)
	}
	if _, err := p.Disabled(context.Background(), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a principal-less read: err=%v", err)
	}
	// User 3 writing user 2's row (off, or on) is refused, and changes nothing.
	for _, d := range []bool{true, false} {
		if err := p.SetDisabled(asUser(3), 2, d); err == nil {
			t.Fatalf("user 3 wrote user 2's toggle (disabled=%v)", d)
		}
	}
	if off, _ := p.Disabled(asUser(2), 2); off {
		t.Fatal("user 3's write changed user 2's toggle")
	}
	if off, _ := p.Disabled(asUser(3), 3); !off {
		t.Fatal("user 3's refused write of user 2's row turned user 3 on")
	}
	if err := p.SetDisabled(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if off, _ := p.Disabled(asUser(2), 2); !off {
		t.Fatal("off again")
	}
}

// TestASavedChoiceKeepsItsMeaning: a row saved while the default was ON — an
// explicit ON (disabled=false, written by "turn back on") and an explicit OFF
// (disabled=true) — reads exactly as saved; only a user with NO row is off by
// default. The rows are written as the table's owner, as they sit on an
// install upgraded from the ON default (no migration rewrote them).
func TestASavedChoiceKeepsItsMeaning(t *testing.T) {
	p, dsn := pgPrefsDSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO suggest_preferences (owner_id, disabled) VALUES (2, false), (3, true)`)
	for _, c := range []struct {
		owner int64
		off   bool
		what  string
	}{{2, false, "a saved ON"}, {3, true, "a saved OFF"}, {4, true, "no row (the default)"}} {
		if off, err := p.Disabled(asUser(c.owner), c.owner); err != nil || off != c.off {
			t.Errorf("%s (user %d): off=%v err=%v, want off=%v", c.what, c.owner, off, err, c.off)
		}
	}
}

// TestAFreshUserGetsNoSuggestionsAndNoCall: through the engine on the real
// toggle, a user who never chose gets no set and the model (a fake that
// counts) is NEVER called; turned on, they get a generated set (one call);
// another user is still off and costs no call; turned off again, nothing and
// no call.
func TestAFreshUserGetsNoSuggestionsAndNoCall(t *testing.T) {
	p := pgPrefs(t)
	g := &countingGen{}
	e := &Engine{Gen: g, Prefs: p}
	b := turnInput(t, turn1)
	set, out, err := e.Suggest(asUser(2), 2, "s", b, 1)
	if err != nil || out != OutcomeUserOff || len(set.Suggestions) != 0 {
		t.Fatalf("a fresh user: out=%v err=%v chips=%d (want user-off, none)", out, err, len(set.Suggestions))
	}
	if g.n() != 0 {
		t.Fatalf("a fresh user cost %d model calls (want 0)", g.n())
	}
	if o := e.Enabled(asUser(2), 2); o != OutcomeUserOff {
		t.Fatalf("a fresh user is enabled: %q", o)
	}
	// ON (the Account switch's write).
	if err := p.SetDisabled(asUser(2), 2, false); err != nil {
		t.Fatal(err)
	}
	set, out, err = e.Suggest(asUser(2), 2, "s", b, 2)
	if err != nil || out != OutcomeGenerated || len(set.Suggestions) == 0 || g.n() != 1 {
		t.Fatalf("turned on: out=%v err=%v chips=%d calls=%d (want generated, chips, 1 call)", out, err, len(set.Suggestions), g.n())
	}
	// User 2's ON never enables user 3.
	if set, out, _ := e.Suggest(asUser(3), 3, "s", b, 2); out != OutcomeUserOff || len(set.Suggestions) != 0 || g.n() != 1 {
		t.Fatalf("user 3 after user 2 turned on: out=%v chips=%d calls=%d", out, len(set.Suggestions), g.n())
	}
	// OFF again: nothing, and no call for a new turn.
	if err := p.SetDisabled(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if set, out, _ := e.Suggest(asUser(2), 2, "s", turnInput(t, turn1+" Or (c)?"), 3); out != OutcomeUserOff || len(set.Suggestions) != 0 || g.n() != 1 {
		t.Fatalf("turned off: out=%v chips=%d calls=%d", out, len(set.Suggestions), g.n())
	}
}
