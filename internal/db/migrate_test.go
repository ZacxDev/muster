package db_test

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// 🔴 THIS IS AN EXTERNAL TEST PACKAGE (`package db_test`) SO IT CAN IMPORT
// internal/dbtest. internal/dbtest deliberately does NOT import internal/db —
// that would be an import cycle for any internal test of this package — and the
// only reason it can advise about `schema_migrations` at all is that it names
// the table rather than calling the runner.

// tablesThisSchemaMustHave is the set 0001 exists to create.
//
// 🔴 A LEDGER, NOT A SPOT CHECK. It fails when the set GROWS or SHRINKS, so a
// table quietly dropped from 0001 reddens here and a table added without a
// decision does too. A "does `notes` exist" check would pass against a
// migration that created one table and lost twelve.
var tablesThisSchemaMustHave = []string{
	"agent_privileges",
	"agents",
	"chat_messages",
	"chat_sessions",
	"github_connection",
	"note_attachments",
	"note_comments",
	"notes",
	"privilege_profiles",
	"privilege_requests",
	"runbook_runs",
	"runbooks",
	"task_sessions",
}

func migrated(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := dbtest.DSN(t) // skips, or fails, per MUSTER_TEST_REQUIRE_DB
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Quiet: a passing package's log is discarded anyway, and a failing one is
	// easier to read without the per-migration lines.
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, pool
}

// TestMigrateCreatesEveryTableTheSchemaDeclares.
func TestMigrateCreatesEveryTableTheSchemaDeclares(t *testing.T) {
	ctx, pool := migrated(t)

	rows, err := pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		   AND table_name <> 'schema_migrations'
		 ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the catalogue query returned no tables at all; it is not observing the database")
	}
	if !equal(got, tablesThisSchemaMustHave) {
		t.Fatalf("the public schema changed.\n got: %v\nwant: %v", got, tablesThisSchemaMustHave)
	}
}

// TestEveryIdentityColumnIsGeneratedALWAYS.
//
// 🔴 THIS IS THE ONE PROPERTY OF 0001 WHOSE VIOLATION IS SILENT FOR MONTHS. A
// column weakened from ALWAYS to BY DEFAULT behaves identically for every write
// muster makes; it only shows up when an importer hands in an explicit id and
// succeeds where it should have been refused, or when someone concludes from
// `BY DEFAULT` that a hand-written INSERT migrator is fine and writes one.
//
// It asserts the RELATIONSHIP — "every identity column in the schema is ALWAYS"
// — rather than naming columns, so a table added later is covered without
// anybody remembering to extend a list. The COUNT is checked too: zero identity
// columns would satisfy an "all of them are ALWAYS" test vacuously.
func TestEveryIdentityColumnIsGeneratedALWAYS(t *testing.T) {
	ctx, pool := migrated(t)

	rows, err := pool.Query(ctx, `
		SELECT table_name, column_name, is_identity, identity_generation
		  FROM information_schema.columns
		 WHERE table_schema = 'public' AND is_identity = 'YES'
		 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("list identity columns: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var table, column, isIdentity string
		var generation *string
		if err := rows.Scan(&table, &column, &isIdentity, &generation); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if generation == nil || *generation != "ALWAYS" {
			got := "<null>"
			if generation != nil {
				got = *generation
			}
			t.Errorf("%s.%s is GENERATED %s AS IDENTITY, want ALWAYS.\n\n"+
				"BY DEFAULT lets a caller supply an id, which means a hand-written INSERT "+
				"migrator appears to work and a `pg_dump`-based one is no longer the only "+
				"supported path. The restriction is what makes the sequence the single "+
				"authority on ids.", table, column, got)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// Positive control: an empty result would make the loop above assert nothing.
	// 11 of the 13 tables have one; task_sessions is keyed by a composite and
	// github_connection is a pinned singleton.
	if want := len(tablesThisSchemaMustHave) - 2; n != want {
		t.Fatalf("found %d identity column(s), want %d — the query is not seeing the schema this "+
			"test believes it is checking", n, want)
	}
}

// TestMigrateIsIdempotent — a second run must be a no-op, not an error.
func TestMigrateIsIdempotent(t *testing.T) {
	ctx, pool := migrated(t)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var versions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if versions == 0 {
		t.Fatal("schema_migrations is empty after two migrate runs; nothing was recorded")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
