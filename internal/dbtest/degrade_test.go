package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestCloseQuietlyToleratesANilConn — layer 2 of the nil defence, on its own.
//
// ⚠ AN INVARIANT GUARD, NOT A REGRESSION GUARD, AND SAYING SO IS THE POINT. The
// panic was real in the original: ensure's deferred close captures the
// connection VARIABLE, the degraded path USED TO reassign it from connect() —
// which returns (nil, err) — and a failed reconnect ran that defer on a nil
// *pgx.Conn, taking the whole test binary down with a stack pointing at dbtest.
//
// 🔴 BUT NO PRODUCTION CALL SITE CAN PASS NIL TODAY — layer 1 (assign only on
// success) sees to that — so this test pins an invariant of the code as it
// stands, and would only have caught the original bug. Do not count it as
// coverage of the seam. TestEnsureSurvivesAFailedReconnect is the guard that
// actually exercises a failed reconnect, and it is what kills the mutant that
// removes BOTH layers.
//
// No Postgres, no I/O: a pure nil-safety assertion that dies with a panic rather
// than an assertion failure if the check is removed — which is the behaviour
// being pinned.
func TestCloseQuietlyToleratesANilConn(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("closeQuietly(nil) panicked: %v.\n\n"+
				"ensure()'s deferred close runs on a connection variable that the degraded path "+
				"reassigns from connect(), which returns nil on error. Without the nil guard a "+
				"failed reconnect panics the whole test binary.", r)
		}
	}()
	closeQuietly(nil)
}

// TestAdminDatabaseStepsAsideWhenTheTemplateIsPostgres — the guard for a defect
// that only appears under `go test ./...`.
//
// 🔴 A SINGLE PACKAGE PASSES EITHER WAY, which is what made this survive review
// in the original. `CREATE DATABASE ... TEMPLATE t` refuses while any session is
// connected to t, but Postgres does not count the ASKING backend — so one binary
// provisioning from `postgres` while connected to `postgres` succeeds. Run the
// whole module and every other binary's admin session is on that database:
// dozens of failures with `source database "postgres" is being accessed by other
// users (SQLSTATE 55006)`.
func TestAdminDatabaseStepsAsideWhenTheTemplateIsPostgres(t *testing.T) {
	if got := adminDatabaseFor("muster_test"); got != adminDatabase {
		t.Fatalf("adminDatabaseFor(ordinary template) = %q, want %q", got, adminDatabase)
	}
	got := adminDatabaseFor(adminDatabase)
	if got == adminDatabase {
		t.Fatalf("adminDatabaseFor(%q) = %q — the setup connection would sit on the very database "+
			"it is about to copy, and every OTHER test binary's CREATE DATABASE would fail with "+
			"55006", adminDatabase, got)
	}
	if got != fallbackAdminDatabase {
		t.Fatalf("adminDatabaseFor(%q) = %q, want %q", adminDatabase, got, fallbackAdminDatabase)
	}
}

// TestEnsureDegradesWhenTheSetupLockIsHeld drives the fallback END TO END —
// including the reconnect, which is the part that was dead on arrival twice in
// the original.
//
// 🔴 IT REACHES THE PATH BY HOLDING THE REAL LOCK, not by faking one. A budget
// that is already expired makes pgx return before it writes a byte, leaving the
// connection healthy — which exercises a *different*, easier path than the one
// that actually breaks. Holding the lock from a second session forces the wait
// to be cancelled IN FLIGHT, which is what puts a deadline on the socket and
// makes the reconnect load-bearing.
//
// 🔴 BOUNDED, AND IT MUST STAY THAT WAY. It goes through Base(t). With a
// database: one connect, a 400ms lock budget, one CREATE DATABASE, and it
// provisions ONE extra database named for this test.
//
// ⚠ THE HOLD IS 400ms PLUS A CREATE, AND THE CREATE IS THE PART THAT VARIES.
// release() runs after ensure() returns, so the window includes the CREATE
// DATABASE. On a slow disk the hold is bounded only by createBudget. The
// consequence is benign either way: another binary waiting on the lock either
// gets it late or takes the fallback, and the fallback is what this test proves
// works.
func TestEnsureDegradesWhenTheSetupLockIsHeld(t *testing.T) {
	base := Base(t)
	template, err := TemplateName(base)
	if err != nil {
		t.Fatalf("TemplateName: %v", err)
	}
	adminDSN, err := withDatabase(base, adminDatabaseFor(template))
	if err != nil {
		t.Fatalf("adminDSN: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), budget(t, 30*time.Second))
	defer cancel()
	holder, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("holder connect: %v", err)
	}
	defer func() { _ = holder.Close(context.Background()) }()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		t.Fatalf("holder could not take the setup lock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rcancel()
		if _, err := holder.Exec(rctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
			t.Errorf("holder unlock failed (%v); closing the connection so the lock cannot be stranded", err)
			_ = holder.Close(context.Background())
		}
	}
	defer release()

	// Positive control: the lock really is held, so the wait below cannot pass
	// for the trivial reason that nothing was contended.
	var held int
	if err := holder.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
		   AND classid = ($1::bigint >> 32) AND objid = ($1::bigint & x'FFFFFFFF'::bigint)`,
		setupLockKey).Scan(&held); err != nil {
		t.Fatalf("positive control query: %v", err)
	}
	if held != 1 {
		t.Fatalf("positive control: pg_locks reports %d holder(s) of the setup key while one is "+
			"deliberately held, want 1 — the wait below would not be contended and this test "+
			"would prove nothing", held)
	}

	name := DatabaseName("dbtest-degrade-probe|" + t.Name())
	started := time.Now()
	dsn, _, err := ensure(t, base, name, 400*time.Millisecond)
	release() // free the lock as soon as ensure() is past it
	if err != nil {
		t.Fatalf("ensure() failed on the degraded path after %s: %v.\n\n"+
			"Giving up on the setup lock must NOT fail: the lock is an optimisation, and an "+
			"optimisation that reddens the gate is worse than none.",
			time.Since(started).Round(time.Millisecond), err)
	}
	if !strings.Contains(dsn, name) {
		t.Fatalf("degraded ensure() returned %q, which does not name %q", dsn, name)
	}

	// And the database it made is real and usable — a DSN naming nothing would
	// satisfy the string check above.
	cctx, ccancel := context.WithTimeout(context.Background(), budget(t, 30*time.Second))
	defer ccancel()
	conn, err := pgx.Connect(cctx, dsn)
	if err != nil {
		t.Fatalf("connect to the database the degraded path created: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var current string
	if err := conn.QueryRow(cctx, `SELECT current_database()`).Scan(&current); err != nil {
		t.Fatalf("current_database(): %v", err)
	}
	if current != name {
		t.Fatalf("connected to %q, want %q", current, name)
	}
}

// TestEnsureSurvivesAFailedReconnect pins the SEAM the helper's own test cannot
// see: ensure's deferred close, run after a reconnect that FAILED.
//
// 🔴 THE HELPER TEST IS NOT ENOUGH, AND THAT WAS MEASURED, NOT SUSPECTED.
// TestCloseQuietlyToleratesANilConn proves closeQuietly tolerates nil. It says
// nothing about whether ensure's defer GOES THROUGH IT — and a delta audit on
// the original reverted that one line to `defer func() { _ = conn.Close(...) }()`
// and watched `go test -race ./internal/dbtest/` exit 0. The suite could not see
// it because the only other test that reaches the degraded path has a reconnect
// that SUCCEEDS, so conn is never nil when the defer runs. This is the case
// where it is: a guard's description claimed a relationship while the test
// inspected one side.
//
// It reaches the branch by injecting the dialer — first call real, every later
// call an error — which is what ensureWith's connect parameter exists for. The
// setup lock is genuinely held so the wait is genuinely cancelled; `calls` is
// the positive control that the reconnect was actually attempted, because if the
// lock were free the whole test would pass without ever entering the branch.
//
// No database is created: ensure returns before the lookup.
func TestEnsureSurvivesAFailedReconnect(t *testing.T) {
	base := Base(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ensure() PANICKED when the reconnect failed: %v.\n\n"+
				"The degraded path must return a named error. A panic here takes the whole test "+
				"binary down with a stack pointing at dbtest instead of at a named test — the "+
				"misattribution this package exists to remove.", r)
		}
	}()

	template, err := TemplateName(base)
	if err != nil {
		t.Fatalf("TemplateName: %v", err)
	}
	realConnect, err := adminConnector(t, base, template)
	if err != nil {
		t.Fatalf("adminConnector: %v", err)
	}
	adminDSN, err := withDatabase(base, adminDatabaseFor(template))
	if err != nil {
		t.Fatalf("adminDSN: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), budget(t, 30*time.Second))
	defer cancel()
	holder, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("holder connect: %v", err)
	}
	defer func() { _ = holder.Close(context.Background()) }()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		t.Fatalf("holder could not take the setup lock: %v", err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rcancel()
		if _, err := holder.Exec(rctx, `SELECT pg_advisory_unlock($1)`, setupLockKey); err != nil {
			t.Errorf("holder unlock failed (%v); closing the connection so the lock cannot be stranded", err)
			_ = holder.Close(context.Background())
		}
	}()

	calls := 0
	forced := errors.New("dbtest seam test: forced reconnect failure")
	connect := func() (*pgx.Conn, error) {
		calls++
		if calls == 1 {
			return realConnect()
		}
		return nil, forced
	}

	_, _, err = ensureWith(t, base, DatabaseName("dbtest-reconnect-probe|"+t.Name()),
		300*time.Millisecond, connect)

	// Positive control FIRST: without it, a run where the lock happened to be
	// free never enters the degraded branch and this test asserts nothing.
	//
	// 🔴 IT PRINTS err, BECAUSE calls==1 HAS TWO CAUSES AND THE ORIGINAL MESSAGE
	// ASSERTED THE WRONG ONE. The lock being free is one; the FIRST connect
	// failing — saturated node, `too many clients`, an expired budget, i.e.
	// exactly what this test models — is the other, and it returns at "connect to
	// the %q database". Reporting only "the reconnect was never attempted" sends
	// a maintainer to look at lock contention when the connection failed.
	if calls != 2 {
		t.Fatalf("the dialer was called %d time(s), want 2 — the degraded reconnect was never "+
			"attempted, so nothing below is a claim about it. ensureWith returned: %v "+
			"(calls==1 means either the setup lock was free, or the FIRST connect failed — "+
			"that error says which)", calls, err)
	}
	if err == nil {
		t.Fatal("ensureWith returned nil error although the reconnect was forced to fail")
	}
	if !errors.Is(err, forced) {
		t.Fatalf("ensureWith error = %v; it must wrap the dialer's own error so the cause is "+
			"attributable", err)
	}
	if !strings.Contains(err.Error(), "reconnect to") {
		t.Fatalf("ensureWith error = %q; it must name the step that failed", err)
	}
}
