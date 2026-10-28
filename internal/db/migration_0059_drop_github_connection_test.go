package db_test

import (
	"context"
	"io"
	"log"
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

// TestMigration0059ThenRLSOffAndOn: the rollback tooling still works on the
// schema 0059 leaves. rollback/rls_off.sql and 0007's body (which db.RLSOn
// re-runs) both name github_connection; against a schema without it each
// must skip it, not fail.
func TestMigration0059ThenRLSOffAndOn(t *testing.T) {
	ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, "ms_0059_rls", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatalf("RLSOff after 0059: %v", err)
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatalf("RLSOn after 0059: %v", err)
	}
	// RLSOn re-ran 0007's body: it must not have brought the table back.
	assertGitHubConnectionGone(ctx, t, pool)
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
