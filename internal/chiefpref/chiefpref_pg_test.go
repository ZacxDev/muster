package chiefpref

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
	dbtest.OwnerExec(t, dsn, `TRUNCATE chief_preferences`)
	return NewPG(pool)
}

func asUser(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

// TestNoChoiceUntilOneIsMadeAndItIsPerUser: a user who never chose has no
// choice (the caller's rule decides the default); off and on are stored as
// that user's alone; another user can neither read nor write it (row-level
// security, and a refusal before the query); the system scope reads it.
func TestNoChoiceUntilOneIsMadeAndItIsPerUser(t *testing.T) {
	p := pgStore(t)
	if chose, on, err := p.Choice(asUser(2), 2); err != nil || chose || on {
		t.Fatalf("default: chose=%v on=%v err=%v", chose, on, err)
	}
	if err := p.SetEnabled(asUser(2), 2, false); err != nil {
		t.Fatal(err)
	}
	if chose, on, _ := p.Choice(asUser(2), 2); !chose || on {
		t.Fatalf("user 2 turned it off: chose=%v on=%v", chose, on)
	}
	if chose, _, _ := p.Choice(asUser(3), 3); chose {
		t.Fatal("user 2's choice reached user 3")
	}
	// Reading another user's row is refused before the query: RLS would hide
	// it, and a hidden row reads as "never chose", which can mean ON.
	if _, _, err := p.Choice(asUser(3), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("user 3 read user 2's choice: err=%v", err)
	}
	if _, _, err := p.Choice(context.Background(), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a principal-less read: err=%v", err)
	}
	if chose, on, err := p.Choice(authz.System(context.Background()), 2); err != nil || !chose || on {
		t.Fatalf("the system scope reads user 2's off: chose=%v on=%v err=%v", chose, on, err)
	}
	if err := p.SetEnabled(asUser(3), 2, true); err == nil {
		t.Fatal("user 3 wrote user 2's choice")
	}
	if _, on, _ := p.Choice(asUser(2), 2); on {
		t.Fatal("user 3's write changed user 2's choice")
	}
	if err := p.SetEnabled(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if chose, on, _ := p.Choice(asUser(2), 2); !chose || !on {
		t.Fatalf("on again: chose=%v on=%v", chose, on)
	}
}
