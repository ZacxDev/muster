package suggest

import (
	"context"
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
	dbtest.OwnerExec(t, dsn, `TRUNCATE suggest_preferences`)
	return NewPGPrefs(pool)
}

func asUser(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

// TestTheToggleIsOnByDefaultAndPerUser: no row is on; one user's off is
// theirs alone; another user can neither read nor write it (row-level
// security); on again is on.
func TestTheToggleIsOnByDefaultAndPerUser(t *testing.T) {
	p := pgPrefs(t)
	if off, err := p.Disabled(asUser(2), 2); err != nil || off {
		t.Fatalf("default: off=%v err=%v", off, err)
	}
	if err := p.SetDisabled(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if off, _ := p.Disabled(asUser(2), 2); !off {
		t.Fatal("user 2 turned it off")
	}
	if off, _ := p.Disabled(asUser(3), 3); off {
		t.Fatal("user 2's toggle reached user 3")
	}
	// User 3 reading user 2's row sees nothing (RLS): it reads as the default.
	if off, err := p.Disabled(asUser(3), 2); err != nil || off {
		t.Fatalf("user 3 read user 2's toggle: off=%v err=%v", off, err)
	}
	// User 3 writing user 2's row is refused.
	if err := p.SetDisabled(asUser(3), 2, false); err == nil {
		t.Fatal("user 3 wrote user 2's toggle")
	}
	if off, _ := p.Disabled(asUser(2), 2); !off {
		t.Fatal("user 3's write changed user 2's toggle")
	}
	if err := p.SetDisabled(asUser(2), 2, false); err != nil {
		t.Fatal(err)
	}
	if off, _ := p.Disabled(asUser(2), 2); off {
		t.Fatal("on again")
	}
}
