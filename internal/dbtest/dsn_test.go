package dbtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestDSNHandsOutADatabaseThatIsNOTTheTemplate closes the one gap the static
// guards in ledger_test.go structurally cannot see.
//
// 🔴 WITHOUT THIS, THE WHOLE SUITE IS GREEN FOR THE WRONG REASON. Those guards
// prove that every package ROUTES through DSN and that the names DSN would
// compute are distinct. Neither of them ever calls DSN, so an `ensure` that
// quietly returned the caller's base DSN unchanged — every package back on ONE
// shared database, which is the exact defect this package exists to remove —
// passes all of them, and passes every other test in the module too, because
// sharing a database is not something a single package can notice. The mutant
// on the original was run (`ensure` made to hand the caller's base DSN straight
// back) and only this test failed.
//
// 🔴 BOUNDED, DELIBERATELY. It goes through Base(t), so it skips without the
// DSN and FAILS when the requirement is on. With a database, it performs exactly
// what any other package performs — one CREATE DATABASE through the same
// serialised path — plus one connect and one query, each clamped against the
// running test's own deadline by budget(). It creates no throwaway database of
// its own and drops nothing.
func TestDSNHandsOutADatabaseThatIsNOTTheTemplate(t *testing.T) {
	// Note: os.Getenv(EnvVar) below uses the CONSTANT, so this file contains no
	// literal spelling of either variable and the sole-reader ledger is
	// unaffected. That is also that guard's known limitation, written down where
	// it matters: it catches the realistic regression (a fixture copy-pasted back
	// to os.Getenv("...")), not a caller that deliberately goes through the
	// exported constant.
	dsn := DSN(t) // skips, or fails, when the variable is unset

	base := os.Getenv(EnvVar)
	template, err := TemplateName(base)
	if err != nil {
		t.Fatalf("TemplateName(base): %v", err)
	}
	mine, err := TemplateName(dsn)
	if err != nil {
		t.Fatalf("TemplateName(returned dsn): %v", err)
	}
	if mine == template {
		t.Fatalf("DSN returned the TEMPLATE database %q. Every package would be sharing it again, "+
			"and no test anywhere else in this module can tell — cross-package contamination is "+
			"invisible from inside one package", template)
	}
	if want := DatabaseName(callerIdentity(0)); mine != want {
		t.Fatalf("DSN returned database %q, want %q for this package's identity", mine, want)
	}

	// And it is a REAL database carrying the template's schema — a DSN naming a
	// database that does not exist would satisfy every string comparison above.
	ctx, cancel := context.WithTimeout(context.Background(), budget(t, 30*time.Second))
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the private database %q: %v", mine, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var current string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&current); err != nil {
		t.Fatalf("current_database(): %v", err)
	}
	if current != mine {
		t.Fatalf("connected to %q but the DSN names %q", current, mine)
	}
	var migrated bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&migrated); err != nil {
		t.Fatalf("check schema_migrations: %v", err)
	}
	if !migrated {
		// 🔴 NOT FATAL, AND THAT IS A KNOWN, MEASURED HOLE — do not read this
		// branch as coverage. Mutating the CREATE to drop `TEMPLATE` (private
		// database, but EMPTY) SURVIVES the whole suite: every package then
		// bootstraps its own schema and still goes green, just slower — a
		// performance regression, not an isolation one, so the property this file
		// pins is untouched by it.
		//
		// It stays non-fatal because the two cases are indistinguishable from
		// here: an unmigrated template is LEGITIMATE when someone runs the suite
		// without `go run ./cmd/muster-migrate` first. Separating them would mean
		// connecting to the template to look, and a session on the template makes
		// a CONCURRENT `CREATE DATABASE ... TEMPLATE` fail with 55006 — trading a
		// performance blind spot for a new way to redden the gate.
		t.Logf("the private database %q has no schema_migrations, so the template %q was not "+
			"migrated before the tests ran. In CI that means the migrate step and %s have "+
			"drifted apart and every package is bootstrapping its own schema.", mine, template, EnvVar)
	}
}
