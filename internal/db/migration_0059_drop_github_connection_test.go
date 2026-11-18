package db_test

import (
	"context"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
)

// Migration 0059 drops github_connection, the per-user GitHub OAuth connection
// #315 stopped reading and writing. In a SCRATCH database, as 0005's tests: the
// package's own database is already at the newest version, so it cannot show a
// database that holds a connection when 0059 arrives.

// TestMigration0059DropsGitHubConnectionFromAPopulatedDatabase is the deploy
// case: a database at 0058 holding a stored connection. 0059 must apply and
// leave no table and no policy behind, and every other owned table intact.
func TestMigration0059DropsGitHubConnectionFromAPopulatedDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, "ms_0059_populated", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	applyThrough(ctx, t, pool, 58)
	mustExec(ctx, t, pool, `INSERT INTO github_connection (owner_id, login, scopes, token_ciphertext) VALUES (1, 'octo', 'repo', '\xdeadbeef')`)
	if n := countRows(ctx, t, pool, "github_connection"); n != 1 {
		t.Fatalf("fixture: github_connection holds %d rows at 0058, want 1", n)
	}

	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrating a populated 0058 database failed: %v", err)
	}
	assertGitHubConnectionGone(ctx, t, pool)
}

// TestMigration0059HeaderRecreateThenRLSOn is the rollback recipe in 0059's
// header, run as written: re-create github_connection with the header's own
// CREATE TABLE (read from the file, so the header cannot drift from what is
// tested), run db.RLSOn, and the table must come back under row-level
// security — enabled AND forced, with exactly its one owner_isolation policy.
// That is what the header promises an older image gets back.
func TestMigration0059HeaderRecreateThenRLSOn(t *testing.T) {
	ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, "ms_0059_recreate", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertGitHubConnectionGone(ctx, t, pool)

	mustExec(ctx, t, pool, headerCreateTable(t))
	if !tableExists(ctx, t, pool, "github_connection") {
		t.Fatal("the header's CREATE TABLE did not create github_connection")
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatalf("RLSOn after re-creating github_connection from the 0059 header: %v", err)
	}
	var enabled, forced bool
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class
		WHERE oid = 'public.github_connection'::regclass`).Scan(&enabled, &forced); err != nil {
		t.Fatal(err)
	}
	if !enabled || !forced {
		t.Errorf("re-created github_connection after RLSOn: rls enabled=%v forced=%v; want both true", enabled, forced)
	}
	var policies []string
	rows, err := pool.Query(ctx, `SELECT policyname::text FROM pg_policies
		WHERE schemaname = 'public' AND tablename = 'github_connection' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(policies, []string{"owner_isolation"}) {
		t.Errorf("policies on re-created github_connection after RLSOn = %v; want [owner_isolation]", policies)
	}
}

// TestRLSToolsStillFailOnAnotherMissingOwnedTable: the guards in 0007 and
// rollback/rls_off.sql skip github_connection ONLY. Any other owned table in
// their loop that is missing is a broken schema, and both tools must say so.
// connector_audit is in the same loop and nothing re-run by RLSOn names it.
func TestRLSToolsStillFailOnAnotherMissingOwnedTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, "ms_0059_narrow", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mustExec(ctx, t, pool, `DROP TABLE public.connector_audit`)
	const missing = `relation "public.connector_audit" does not exist`

	err = db.RLSOn(ctx, pool)
	if err == nil {
		t.Error("RLSOn succeeded with connector_audit missing; the github_connection guard must not skip other tables")
	} else if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "0007") {
		t.Errorf("RLSOn with connector_audit missing: %v; want 0007 failing with %q", err, missing)
	}
	err = db.RLSOff(ctx, pool)
	if err == nil {
		t.Error("RLSOff succeeded with connector_audit missing; the github_connection guard must not skip other tables")
	} else if !strings.Contains(err.Error(), missing) {
		t.Errorf("RLSOff with connector_audit missing: %v; want %q", err, missing)
	}
}

// headerCreateTable returns the CREATE TABLE statement from 0059's header
// comment, uncommented: the lines from "CREATE TABLE public.github_connection ("
// through the first one ending ");".
func headerCreateTable(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/0059_drop_github_connection.sql")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "--") {
			if out != nil {
				break
			}
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, "--"))
		if out == nil && !strings.HasPrefix(body, "CREATE TABLE public.github_connection (") {
			continue
		}
		out = append(out, body)
		if strings.HasSuffix(body, ");") {
			return strings.Join(out, "\n")
		}
	}
	t.Fatal("no complete CREATE TABLE public.github_connection statement in 0059's header")
	return ""
}

func assertGitHubConnectionGone(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// Positive control for the probes: an owned table 0059 leaves, with its
	// policy, must be observed — or the zeros below prove nothing.
	if !tableExists(ctx, t, pool, "connectors") || policiesOn(ctx, t, pool, "connectors") != 1 {
		t.Fatal("connectors / its owner_isolation policy not found — the probes are not observing this database")
	}
	if tableExists(ctx, t, pool, "github_connection") {
		t.Error("table github_connection still exists after migrating")
	}
	if n := policiesOn(ctx, t, pool, "github_connection"); n != 0 {
		t.Errorf("%d policies on github_connection remain after migrating, want 0", n)
	}
}

func policiesOn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE schemaname='public' AND tablename=$1`, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
