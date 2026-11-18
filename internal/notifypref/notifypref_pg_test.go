package notifypref

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

func pgStore(t *testing.T) *PG {
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
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE notify_preferences`)
	return NewPG(pool)
}

func asUser(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

// TestTmuxAddressIsOffUntilTurnedOnAndIsPerUser: every user — the operator
// and a member, none of whom ever chose — reads off; on and off are stored as
// that user's alone; another user can neither read nor write it (refused
// before the query, and row-level security); the system scope reads it.
func TestTmuxAddressIsOffUntilTurnedOnAndIsPerUser(t *testing.T) {
	p := pgStore(t)
	for _, uid := range []int64{authz.OperatorUserID, 2, 3} {
		if on, err := p.TmuxAddress(asUser(uid), uid); err != nil || on {
			t.Fatalf("user %d by default: on=%v err=%v, want off", uid, on, err)
		}
	}
	if err := p.SetTmuxAddress(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if on, err := p.TmuxAddress(asUser(2), 2); err != nil || !on {
		t.Fatalf("user 2 turned it on: on=%v err=%v", on, err)
	}
	if on, _ := p.TmuxAddress(asUser(3), 3); on {
		t.Fatal("user 2's choice reached user 3")
	}
	if _, err := p.TmuxAddress(asUser(3), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("user 3 read user 2's preference: err=%v", err)
	}
	if _, err := p.TmuxAddress(context.Background(), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a principal-less read: err=%v", err)
	}
	if on, err := p.TmuxAddress(authz.System(context.Background()), 2); err != nil || !on {
		t.Fatalf("the system scope reads user 2's on: on=%v err=%v", on, err)
	}
	if err := p.SetTmuxAddress(asUser(3), 2, false); err == nil {
		t.Fatal("user 3 wrote user 2's preference")
	}
	if on, _ := p.TmuxAddress(asUser(2), 2); !on {
		t.Fatal("user 3's write changed user 2's preference")
	}
	if err := p.SetTmuxAddress(asUser(2), 2, false); err != nil {
		t.Fatal(err)
	}
	if on, _ := p.TmuxAddress(asUser(2), 2); on {
		t.Fatal("off again still reads on")
	}
}

// TestCodenameFirstIsOffUntilTurnedOnAndIsPerUserAndIndependent: "Start
// notification titles with the tmux name" (migration 0117) reads off for
// every user who never chose — and for a user whose row exists because they
// turned "Show tmux sessions" on; on and off are that user's alone, refused to
// another user, readable by the system scope; and each switch's write leaves
// the other one as it was.
func TestCodenameFirstIsOffUntilTurnedOnAndIsPerUserAndIndependent(t *testing.T) {
	p := pgStore(t)
	for _, uid := range []int64{authz.OperatorUserID, 2, 3} {
		if on, err := p.CodenameFirst(asUser(uid), uid); err != nil || on {
			t.Fatalf("user %d by default: on=%v err=%v, want off", uid, on, err)
		}
	}
	if err := p.SetTmuxAddress(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if on, err := p.CodenameFirst(asUser(2), 2); err != nil || on {
		t.Fatalf("a row written by the tmux switch: codename on=%v err=%v, want off", on, err)
	}
	if err := p.SetCodenameFirst(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if on, err := p.CodenameFirst(asUser(2), 2); err != nil || !on {
		t.Fatalf("user 2 turned it on: on=%v err=%v", on, err)
	}
	if on, _ := p.TmuxAddress(asUser(2), 2); !on {
		t.Fatal("turning the codename switch on turned the tmux switch off")
	}
	if err := p.SetCodenameFirst(asUser(3), 3, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := p.TmuxAddress(asUser(3), 3); on {
		t.Fatal("a row written by the codename switch reads the tmux switch on")
	}
	if on, _ := p.CodenameFirst(asUser(authz.OperatorUserID), authz.OperatorUserID); on {
		t.Fatal("user 2's choice reached the operator")
	}
	if _, err := p.CodenameFirst(asUser(3), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("user 3 read user 2's preference: err=%v", err)
	}
	if _, err := p.CodenameFirst(context.Background(), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a principal-less read: err=%v", err)
	}
	if on, err := p.CodenameFirst(authz.System(context.Background()), 2); err != nil || !on {
		t.Fatalf("the system scope reads user 2's on: on=%v err=%v", on, err)
	}
	if err := p.SetCodenameFirst(asUser(3), 2, false); err == nil {
		t.Fatal("user 3 wrote user 2's preference")
	}
	if on, _ := p.CodenameFirst(asUser(2), 2); !on {
		t.Fatal("user 3's write changed user 2's preference")
	}
	if err := p.SetTmuxAddress(asUser(2), 2, false); err != nil {
		t.Fatal(err)
	}
	if on, _ := p.CodenameFirst(asUser(2), 2); !on {
		t.Fatal("turning the tmux switch off turned the codename switch off")
	}
	if err := p.SetTmuxAddress(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCodenameFirst(asUser(2), 2, false); err != nil {
		t.Fatal(err)
	}
	if on, _ := p.CodenameFirst(asUser(2), 2); on {
		t.Fatal("off again still reads on")
	}
	if on, _ := p.TmuxAddress(asUser(2), 2); !on {
		t.Fatal("turning the codename switch off turned the tmux switch off")
	}
}
