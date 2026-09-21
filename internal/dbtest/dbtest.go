// Package dbtest gives every Postgres-backed test PACKAGE its own database, and
// decides — in ONE place — whether a missing database is a skip or a failure.
//
// # WHY PER-PACKAGE DATABASES
//
// The obvious design is one test database shared by every package. `go test
// ./...` runs packages in PARALLEL (there is no -p 1), and three things then go
// wrong, all of them measured on the project muster was extracted from rather
// than argued:
//
//   - CROSS-PACKAGE CONTAMINATION. One package's fixture DELETEs a table and
//     COUNTs it; another package, running at the same time, writes rows into it.
//     The failure names a row that no test in the failing package wrote, and it
//     is invisible from inside either package.
//   - SCHEMA CONTAMINATION. A test that ALTERs the shared schema — dropping a
//     CHECK constraint to ask what an ORDER BY does with an out-of-vocabulary
//     row — leaves every other package running against a weakened schema for the
//     duration.
//   - ADVISORY-LOCK CONTENTION. Migrate takes ONE key and advisory locks are
//     DATABASE-scoped, so every package queues on it at fixture setup.
//
// Per-package databases remove all three: nothing another package writes is
// visible, an ALTER is local, and the migration key is scoped to a database only
// one package connects to.
//
// # HOW
//
// MUSTER_TEST_DATABASE_URL keeps its meaning as the SERVER coordinates, and the
// database it names becomes the TEMPLATE. On first use inside a test binary
// dbtest connects to the `postgres` maintenance database, takes a session
// advisory lock there, and runs
//
//	CREATE DATABASE "<pkg db>" TEMPLATE "<the database in the URL>"
//
// which copies an ALREADY-MIGRATED schema instead of replaying every migration.
// The packages' own Migrate calls then take the no-op path.
//
// 🔴 THE CREATES ARE SERIALISED ON PURPOSE. Postgres WAL-logs every block of the
// template, so firing one CREATE DATABASE per package concurrently is the
// pathological shape — on a busy node with a slow disk a single one has been
// observed taking over 90 seconds. One advisory lock in the maintenance database
// turns the burst into a queue whose total cost is bounded, and every wait is
// reported with its duration.
//
// ⚠ "SERIALISED" IS BEST-EFFORT, NOT GUARANTEED — see ensureWith. A binary that
// cannot take the lock inside its budget logs that and creates anyway, because
// an optimisation that can fail the build is worse than no optimisation. So the
// worst case is the unserialised burst above, never a red run; do not read the
// paragraph above as a promise that at most one CREATE is ever in flight.
//
// ⚠ THOSE REPORTS ARE t.Logf, SO THEY APPEAR ONLY WHEN THE PACKAGE FAILS. `go
// test` without -v discards a passing package's log. They turn a mystery TIMEOUT
// into a number, which is what they are for, but a run that degraded and still
// passed says nothing. Do not read "every wait is reported" as "you will see it".
//
// 🔴 NOTHING IS DROPPED. `DROP DATABASE` forces an IMMEDIATE checkpoint across
// EVERY database on the server. CI's Postgres is a throwaway that dies with the
// job, so there is nothing to clean up; locally the databases are named
// deterministically and are REUSED on the next run rather than recreated.
//
// # LOCALLY
//
// `make test-db` starts the server this package expects and prints the two
// exports. By hand, and note the database is NAMED:
//
//	docker compose -f docker-compose.test.yml up -d
//	export MUSTER_TEST_DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable'
//	DATABASE_URL="$MUSTER_TEST_DATABASE_URL" go run ./cmd/muster-migrate
//	go test ./...
//
// 🔴 NAME A DATABASE, NOT `postgres`. Pointing the variable at `/postgres` makes
// the TEMPLATE the same database as the maintenance one this package connects
// to — so binary A's admin session sits on it while binary B runs `CREATE
// DATABASE ... TEMPLATE postgres`, and B gets `source database "postgres" is
// being accessed by other users (SQLSTATE 55006)`. A SINGLE package passes
// (Postgres excludes the asking backend), so it only breaks under `go test
// ./...`, which is the way anyone actually runs it. adminDatabaseFor below steps
// aside to `template1` in that case so the mistake is survivable, but the recipe
// above is the one to copy — and docker-compose.test.yml names the database so
// the question does not arise.
//
// ⚠ If you skip the migrate step nothing BREAKS — the template is copied empty
// and each package bootstraps its own — but every package then pays the full
// bootstrap instead of one of them. dbtest says so once, in a t.Log, rather than
// fixing it silently: it deliberately does not import internal/db, because that
// would be an import cycle for internal/db's own tests.
package dbtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EnvVar is the one environment variable this module reads for test-database
// coordinates. It is named here so the ledger guard in ledger_test.go can assert
// that NOTHING else in the module reads it.
const EnvVar = "MUSTER_TEST_DATABASE_URL"

// RequireEnvVar turns an unavailable database from a SKIP into a FAILURE.
//
// 🔴 THIS IS THE CONTROL THE DESIGN THIS PACKAGE WAS PORTED FROM DID NOT HAVE,
// AND ITS ABSENCE IS THE SINGLE MOST LIKELY WAY THIS PROJECT SHIPS BROKEN. With
// only the DSN gate, a fresh clone runs `go test ./...`, every Postgres-backed
// test calls t.Skipf, and the suite reports GREEN having executed none of the
// store logic. A contributor who cannot get Postgres running then opens a pull
// request against tests they never executed, and the reviewer sees the same
// green.
//
// A skip is invisible in a verdict. So CI sets this, and in CI an unavailable
// database is a hard failure with a message that names the variable.
//
// ⚠ IT IS DELIBERATELY NOT THE DEFAULT. Requiring Postgres for `go test ./...`
// on a laptop would make the cheap tests unrunnable, and a gate nobody can run
// locally is a gate people route around. The default is "skip and say so"; CI is
// where the requirement is enforced.
const RequireEnvVar = "MUSTER_TEST_REQUIRE_DB"

// setupLockKey serialises CREATE DATABASE across the test binaries of one
// `go test ./...` run.
//
// 🔴 IT IS DELIBERATELY NOT IN internal/db's advisory-lock key registry. This
// lock is taken in the `postgres` MAINTENANCE database; advisory locks are
// DATABASE-scoped, so it shares no namespace with any key muster takes against
// its own database and cannot collide with one. The value is spelled distinctly
// anyway so a stray pg_locks dump is readable.
const setupLockKey int64 = 0x6d_75_73_74_db

// adminDatabase is the database the setup connection attaches to by default.
const adminDatabase = "postgres"

// fallbackAdminDatabase is where the setup connection goes when the TEMPLATE is
// `postgres` itself. See adminDatabaseFor.
const fallbackAdminDatabase = "template1"

// requireDB reports whether an unavailable database must FAIL rather than skip.
//
// ⚠ THE FALSE SPELLINGS ARE ENUMERATED, AND THE DEFAULT FOR ANYTHING ELSE IS
// TRUE. A variable somebody set to `yes`, `on` or `1` means they want the
// requirement; treating an unrecognised value as "off" would silently disable
// the control in exactly the case where someone was trying to turn it on, which
// is the failure direction that matters.
func requireDB(getenv func(string) string) bool {
	v := strings.ToLower(strings.TrimSpace(getenv(RequireEnvVar)))
	switch v {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// Base returns the value of EnvVar, or ends the test according to the gate.
//
// 🔴 EVERY PG-GATED TEST IN THIS MODULE MUST COME THROUGH HERE. It is the ONE
// place that decides skip-versus-fail; a fixture that reads the variable itself
// is exempt from the requirement, and its skip is then invisible to a CI run
// that believes it set the control. ledger_test.go pins the sole-reader
// property for exactly that reason.
func Base(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base != "" {
		return base
	}
	if requireDB(os.Getenv) {
		t.Fatalf("%s=%s demands a database, but %s is unset.\n\n"+
			"This is a HARD FAILURE on purpose. Without it this test would SKIP, the run "+
			"would be green, and the green would be a claim about a suite that executed no "+
			"store logic at all. Start one with `make test-db`, or unset %s to go back to "+
			"skipping.",
			RequireEnvVar, os.Getenv(RequireEnvVar), EnvVar, RequireEnvVar)
	}
	t.Skipf("set %s to run the Postgres-backed tests (or %s=1 to make their absence a failure)",
		EnvVar, RequireEnvVar)
	return ""
}

// Budgets. Every one of them is clamped against the running test's own deadline
// by budget(), so a slow node produces a NAMED context error attributed to the
// step that ran long instead of a whole-binary `go test` panic.
const (
	connectBudget = 30 * time.Second
	// lockBudget must cover the queue: one `go test ./...` run has one PG-backed
	// package per entry in the ledger, and each holds the lock for one CREATE
	// DATABASE.
	lockBudget   = 5 * time.Minute
	createBudget = 3 * time.Minute
)

var (
	mu    sync.Mutex
	cache = map[string]string{}
	// 🔴 FAILURES ARE CACHED TOO, and that is not tidiness. Without it every test
	// in the package re-runs ensure(). Measured on the project this was ported
	// from: one package went from ~24s to 522s across 16 tests, each paying a
	// fresh lock wait and a fresh create attempt. `go test` with no -timeout has
	// a 10-MINUTE per-binary default, so that turns ONE provisioning hiccup into
	// a whole-binary PANIC instead of one named failure — the misattribution this
	// package exists to remove, arriving by a new route.
	cacheErr = map[string]error{}
)

// DSN returns a DSN for a Postgres database private to the CALLING PACKAGE,
// creating it on first use. When EnvVar is unset it SKIPS — or FAILS, if
// RequireEnvVar is on; see Base.
//
// The database is created once per test binary and reused by every test in it,
// so per-test DELETE/TRUNCATE fixtures keep doing what they did — the difference
// is that no OTHER package can write rows they will see.
func DSN(t testing.TB) string {
	t.Helper()
	base := Base(t)
	id := callerIdentity(1)
	name := DatabaseName(id)

	mu.Lock()
	defer mu.Unlock()
	if dsn, ok := cache[name]; ok {
		return dsn
	}
	if err, ok := cacheErr[name]; ok {
		t.Fatalf("dbtest: the private database %q for %s could not be provisioned earlier in this "+
			"binary and the failure is not retried: %v", name, id, err)
	}
	dsn, created, err := ensure(t, base, name, lockBudget)
	if err != nil {
		cacheErr[name] = err
		t.Fatalf("dbtest: could not provision the private database %q for %s: %v", name, id, err)
	}
	cache[name] = dsn
	if created {
		// Deliberately AFTER ensure() has returned, so it is outside the setup
		// lock: it costs a connect and a query, and the whole point of that lock
		// is to keep the critical section down to one CREATE DATABASE.
		adviseIfUnmigrated(t, dsn, base)
	}
	return dsn
}

// TemplateName reports the database EnvVar points at — the one every private
// database is copied from. Exported for the guards.
func TemplateName(base string) (string, error) {
	u, err := parseBase(base)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(u.Path, "/"), nil
}

// DatabaseName maps a package identity to its database name. Pure, so the guard
// that proves two packages cannot collide needs no server.
//
// The hash is over the WHOLE identity and the slug is only there to make a
// pg_database listing readable; truncating the slug can therefore never make two
// packages share a name.
func DatabaseName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	slug := slugify(identity)
	return fmt.Sprintf("ms_%s_%s", slug, hex.EncodeToString(sum[:5]))
}

// Identity reports the key DSN would use for the CALLING package. Exported so a
// `package main` test binary can pin what the runtime actually reports about
// itself.
func Identity() string { return callerIdentity(1) }

// callerIdentity names the package that called DSN, in a form that is distinct
// for every test binary in this module AND stable across machines.
//
// 🔴 THE DIRECTORY HALF IS DEFENCE, NOT NECESSITY. On the toolchains measured so
// far the runtime reports the FULL import path even for a `package main` test,
// so the package half alone would separate every binary. But that is not
// something the runtime package promises, and the failure mode if it regressed
// is SILENT — several `package main` binaries quietly sharing one database.
// With the directory in the key they stay distinct even then.
func callerIdentity(skip int) string {
	pc, file, _, ok := runtime.Caller(skip + 1)
	if !ok {
		return "unknown"
	}
	pkg := "unknown"
	if fn := runtime.FuncForPC(pc); fn != nil {
		pkg = packageOfFunc(fn.Name())
	}
	return pkg + "|" + filepath.Base(filepath.Dir(file))
}

// packageOfFunc extracts the package path from a runtime function name such as
// "github.com/ZacxDev/muster/internal/notes.(*T).M.func1" -> the part before the
// first "." that follows the last "/".
func packageOfFunc(name string) string {
	head := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		head = name[i+1:]
	}
	if j := strings.Index(head, "."); j >= 0 {
		head = head[:j]
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i+1] + head
	}
	return head
}

func slugify(identity string) string {
	// The readable half is the directory, which is the part after the "|".
	s := identity
	if i := strings.LastIndex(identity, "|"); i >= 0 {
		s = identity[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 20 {
		out = out[:20]
	}
	if out == "" {
		out = "pkg"
	}
	return out
}

// parseBase rejects anything that is not a URL-form DSN, loudly. The
// keyword/value form ("host=... dbname=...") would silently parse as a relative
// URL with an empty path, which would make the template name "" and the CREATE
// fail somewhere much less obvious.
func parseBase(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", EnvVar, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, fmt.Errorf("%s must be a postgres:// URL, got scheme %q", EnvVar, u.Scheme)
	}
	if strings.TrimPrefix(u.Path, "/") == "" {
		return nil, fmt.Errorf("%s names no database (path is %q)", EnvVar, u.Path)
	}
	return u, nil
}

// withDatabase returns base with its database replaced.
func withDatabase(base, name string) (string, error) {
	u, err := parseBase(base)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// adminDatabaseFor picks the maintenance database to run CREATE DATABASE from.
//
// 🔴 IT MUST NOT BE THE TEMPLATE, and the failure when it is looks like someone
// else's bug. `CREATE DATABASE ... TEMPLATE t` refuses while ANY session is
// connected to t, and every other test binary's setup connection is sitting on
// exactly this database — so pointing EnvVar at `/postgres` turns `go test ./...`
// into a pile of `source database "postgres" is being accessed by other users
// (SQLSTATE 55006)`, while a single package still passes because Postgres does
// not count the asking backend.
//
// `template1` is safe as the alternative: it is a normal connectable database,
// and nothing here ever creates FROM it — the TEMPLATE clause always names the
// database the caller's DSN pointed at.
func adminDatabaseFor(template string) string {
	if template == adminDatabase {
		return fallbackAdminDatabase
	}
	return adminDatabase
}

// budget bounds one operation by the SMALLER of its own budget and half of
// whatever time the running test has left, so the failure is a named context
// error rather than `go test`'s whole-binary panic.
func budget(t testing.TB, base time.Duration) time.Duration {
	type deadliner interface{ Deadline() (time.Time, bool) }
	d, ok := t.(deadliner)
	if !ok {
		return base
	}
	deadline, has := d.Deadline()
	if !has {
		return base
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return time.Millisecond
	}
	if half := remaining / 2; half < base {
		return half
	}
	return base
}

// adminConnector builds the dialer ensure uses for its maintenance connection.
//
// It is split out so ensureWith can be handed a DIFFERENT one. That is not
// generality for its own sake: the only way to reach the branch where a
// RECONNECT fails — the branch that once shipped a nil-pointer panic — is to
// make a connect attempt fail on demand, and a parameter does that without a
// test-only global in production code.
func adminConnector(t testing.TB, base, template string) (func() (*pgx.Conn, error), error) {
	adminDSN, err := withDatabase(base, adminDatabaseFor(template))
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("parse admin dsn: %w", err)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	// 🔴 The server-side budget is switched OFF and the CONTEXT is the only
	// bound. CREATE DATABASE copies the template block by block; a
	// statement_timeout inherited from the caller's DSN would cancel it mid-copy
	// and report a 57014 that reads like the bug this package removes.
	cfg.RuntimeParams["statement_timeout"] = "0"
	cfg.RuntimeParams["application_name"] = "muster-dbtest"
	return func() (*pgx.Conn, error) {
		ctx, cancel := context.WithTimeout(context.Background(), budget(t, connectBudget))
		defer cancel()
		return pgx.ConnectConfig(ctx, cfg)
	}, nil
}

// ensure provisions the database, returning whether it CREATED it.
//
// lockWait is a parameter rather than a read of lockBudget so the degraded path
// — the one that used to panic — is reachable from a test without a test-only
// global in production code.
func ensure(t testing.TB, base, name string, lockWait time.Duration) (dsn string, created bool, err error) {
	template, err := TemplateName(base)
	if err != nil {
		return "", false, err
	}
	connect, err := adminConnector(t, base, template)
	if err != nil {
		return "", false, err
	}
	return ensureWith(t, base, name, lockWait, connect)
}

// ensureWith is ensure with the maintenance dialer supplied.
func ensureWith(t testing.TB, base, name string, lockWait time.Duration, connect func() (*pgx.Conn, error)) (dsn string, created bool, err error) {
	template, err := TemplateName(base)
	if err != nil {
		return "", false, err
	}
	if name == template {
		return "", false, fmt.Errorf("refusing to use the template %q as a package database", template)
	}
	conn, err := connect()
	if err != nil {
		return "", false, fmt.Errorf("connect to the %q database: %w", adminDatabaseFor(template), err)
	}
	// Closing the connection drops the session lock even if a later step panics.
	//
	// 🔴 TWO INDEPENDENT REASONS THIS CANNOT SEE A NIL CONNECTION, because in the
	// original it once did and PANICKED the whole test binary — a stack pointing
	// at dbtest instead of a named failure, found by an adversarial audit that
	// killed a TCP proxy mid-lock-wait.
	//   1. STRUCTURAL: the degraded path below reassigns `conn` only AFTER a
	//      successful reconnect. A failed one leaves the previous (closed)
	//      connection in the variable, and pgx's Close is idempotent.
	//   2. closeQuietly is nil-safe anyway.
	// 🔴 ONLY THE PAIR IS PINNED, AND THAT IS WHAT DEFENCE IN DEPTH MEANS —
	// measured on the original, so nobody re-derives it as a gap. Removing BOTH
	// layers is killed by TestEnsureSurvivesAFailedReconnect. Removing EITHER ONE
	// alone SURVIVES a fully green suite, in both directions. So a reviewer who
	// reverts one and sees rc=0 has NOT found a missing fix; they have found the
	// other layer doing its job.
	defer func() { closeQuietly(conn) }()

	// 🔴 THE LOCK DEGRADES; IT DOES NOT FAIL. Waiting for it is an optimisation,
	// and an optimisation that can REDDEN THE GATE is worse than no optimisation
	// — "the queue was slow" reported as a test failure is precisely the
	// misattribution this change exists to remove. So a lock we cannot get in
	// time is logged and stepped over: the worst case is the unserialised burst
	// we would have had without any of this, never a red run.
	lockCtx, cancelLock := context.WithTimeout(context.Background(), budget(t, lockWait))
	defer cancelLock()
	waited := time.Now()
	locked := true
	if _, err := conn.Exec(lockCtx, `SELECT pg_advisory_lock($1)`, setupLockKey); err != nil {
		locked = false
		t.Logf("dbtest: could not take the setup lock after %s (%v); creating %s WITHOUT serialising. "+
			"That is a slower-disk symptom, not a failure — the create below is unaffected.",
			time.Since(waited).Round(time.Millisecond), err, name)
		// 🔴 RECONNECT, AND THIS IS NOT DEFENSIVE PADDING — it is the difference
		// between a degraded path and no degraded path at all. pgx cancels an
		// in-flight query by putting a deadline on the SOCKET, so the connection
		// that just failed to take the lock is BROKEN: every statement after it
		// returns "conn closed". The first version of this fallback did exactly
		// that and turned a slow queue into a hard failure — found by holding the
		// lock from an outside session and watching, not by reading the code.
		closeQuietly(conn)
		// 🔴 ASSIGN ONLY ON SUCCESS — see layer 1 on the defer above. `conn, err =
		// connect()` puts a nil in the variable the defer closes over.
		fresh, ferr := connect()
		if ferr != nil {
			return "", false, fmt.Errorf("reconnect to %q after the setup lock timed out: %w",
				adminDatabaseFor(template), ferr)
		}
		conn = fresh
	}
	if locked {
		defer func() {
			unlockCtx, cancelUnlock := context.WithTimeout(context.WithoutCancel(lockCtx), 10*time.Second)
			defer cancelUnlock()
			_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, setupLockKey)
		}()
	}

	dsn, err = withDatabase(base, name)
	if err != nil {
		return "", false, err
	}

	lookupCtx, cancelLookup := context.WithTimeout(context.Background(), budget(t, connectBudget))
	defer cancelLookup()
	var exists bool
	if err := conn.QueryRow(lookupCtx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return "", false, fmt.Errorf("look up %q: %w", name, err)
	}
	if exists {
		return dsn, false, nil
	}

	createCtx, cancelCreate := context.WithTimeout(context.Background(), budget(t, createBudget))
	defer cancelCreate()
	started := time.Now()
	// Identifiers are built by this package from a hash, never from input, but
	// quote them anyway so a future slug change cannot become an injection.
	create := fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`,
		pgx.Identifier{name}.Sanitize(), pgx.Identifier{template}.Sanitize())
	if _, err := conn.Exec(createCtx, create); err != nil {
		// 42P04 (duplicate_database): somebody created it between the lookup above
		// and here — only reachable on the degraded, unserialised path, and it
		// means the database we wanted EXISTS. That is the outcome, so take it
		// rather than reporting a failure.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P04" {
			return dsn, false, nil
		}
		// 55006 (object_in_use): something still holds a connection to the
		// template. Under the setup lock that should be impossible, so say what
		// was seen rather than retrying blind.
		return "", false, fmt.Errorf("create %q from template %q after %s (setup lock held: %t): %w",
			name, template, time.Since(started).Round(time.Millisecond), locked, err)
	}
	t.Logf("dbtest: created %s from template %s in %s", name, template, time.Since(started).Round(time.Millisecond))
	return dsn, true, nil
}

// closeQuietly closes an admin connection, tolerating a nil one.
//
// ⚠ THE NIL CASE IS NOT REACHABLE TODAY. It was, in the original: ensure's
// deferred close captures the connection VARIABLE, the degraded path reassigned
// it from connect() — which returns (nil, err) — and a failed reconnect panicked
// the whole test binary on a nil *pgx.Conn. That is layer 1's history, not its
// present: the reconnect now assigns only on SUCCESS.
//
// 🔴 KEEP THE CHECK ANYWAY — it is layer 2 of a deliberate pair, and it is what
// survives if someone undoes layer 1. But read TestCloseQuietlyToleratesANilConn
// for what it is: an INVARIANT guard against the code as it stands, and a
// regression guard only against the code as it was. The behavioural guard for
// the pair is TestEnsureSurvivesAFailedReconnect.
func closeQuietly(c *pgx.Conn) {
	if c == nil {
		return
	}
	_ = c.Close(context.Background())
}

// adviseIfUnmigrated says once, out loud, that the fast path is not in play. It
// does NOT migrate: importing internal/db here would be an import cycle for
// internal/db's own tests.
//
// It inspects the FRESH COPY rather than the template, because pg_class is
// per-database and cannot be read across one — and connecting to the template to
// look would be the very thing that makes `CREATE DATABASE ... TEMPLATE` fail
// with 55006 for whoever is next in the queue. The copy has the template's
// schema by construction, so it answers the same question.
//
// Best-effort throughout: an error here is advice that could not be given, never
// a test failure.
//
// 🔴 THE THIRD ARGUMENT IS THE BASE DSN, NOT A DATABASE NAME. Both are bare
// strings, so passing the template here compiles and then does nothing at all —
// TemplateName rejects it and this function returns silently. Nothing pins that
// (a mutant swapping them SURVIVES the suite, because the whole function is
// best-effort advice), so the error path SAYS SO rather than vanishing. That is
// the honest position: not covered, but not invisible either.
func adviseIfUnmigrated(t testing.TB, dsn, base string) {
	template, err := TemplateName(base)
	if err != nil {
		t.Logf("dbtest: adviseIfUnmigrated was given %q, which is not a base DSN (%v) — it needs "+
			"the value of %s, not a database name. No advice was given.", base, err, EnvVar)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget(t, 10*time.Second))
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return
	}
	defer func() { closeQuietly(conn) }()
	var migrated bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&migrated); err != nil {
		return
	}
	if migrated {
		return
	}
	t.Logf("dbtest: the template database %q has no schema_migrations, so every package will "+
		"bootstrap its own copy of the schema. Run `DATABASE_URL=$%s go run ./cmd/muster-migrate` "+
		"once to migrate the template and make this a copy instead.", template, EnvVar)
}
