package main

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// TestTheRecoveryCommandClearsTheStrongFactorRecord: `muster-server user
// clear-strong-factor <id|email>` — the recovery for a sole admin who lost
// every passkey and phone — clears the account's sticky record (migration
// 0188), by id and by email, audited as the CLI; an unknown user is refused.
// It is a server command only: no HTTP route names it (the control reads the
// route ledger).
func TestTheRecoveryCommandClearsTheStrongFactorRecord(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	email := "recover-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	var id int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name, email) VALUES ('admin', 'sole admin', $1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM audit_log WHERE object = $1`, users.UserObject(id))
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
	})
	st := users.NewPG(pool)
	env := map[string]string{envDatabase: dsn}
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		rc := runAccountCommand(ctx, append([]string{"user", "clear-strong-factor"}, args...), func(k string) string { return env[k] }, &out, &errb)
		return rc, out.String() + errb.String()
	}
	for _, who := range []string{strconv.FormatInt(id, 10), email} {
		if err := st.MarkStrongFactor(ctx, id); err != nil {
			t.Fatal(err)
		}
		if rc, out := run(who); rc != 0 {
			t.Fatalf("clear %s: rc %d %s", who, rc, out)
		}
		if had, err := st.HadStrongFactor(ctx, id); err != nil || had {
			t.Fatalf("after clearing by %s the account is still marked (%v %v)", who, had, err)
		}
	}
	var n int
	if err := pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = 'user.strong_factor_cleared' AND object = $1`, users.UserObject(id)).Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit rows: %d %v, want 2", n, err)
	}
	if rc, _ := run("nobody-" + email); rc == 0 {
		t.Fatal("an unknown user was cleared")
	}
	golden, err := os.ReadFile("../../internal/api/testdata/routes.golden")
	if err != nil || len(golden) < 100 {
		t.Fatalf("CONTROL: the route ledger could not be read: %v", err)
	}
	if strings.Contains(string(golden), "strong") {
		t.Fatal("an HTTP route names the strong-factor record")
	}
}
