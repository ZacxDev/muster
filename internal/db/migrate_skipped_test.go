package db_test

import (
	"bytes"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// TestMigrateRefusesToSkipAMigrationBelowTheAppliedVersion (round-2 audit on
// #69): the runner applies only versions above max(version), so a migration
// that reaches the image AFTER a higher one was applied would be skipped
// forever. It must refuse instead, naming the file.
//
//   - The merge-order case itself: a database at {…,0009,0011} with an
//     embedded 0010 (#67's, stood in for here) is refused, naming 0010.
//   - End to end through Migrate: a database missing an embedded version below
//     its max does not boot, and nothing is applied or recorded.
//   - The control: an ordinary database (every embedded version recorded)
//     migrates exactly as before.
func TestMigrateRefusesToSkipAMigrationBelowTheAppliedVersion(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_skipped_migration")

	// The control first: a normal, fully migrated database boots, and the
	// check alone passes over the embedded set.
	var logs bytes.Buffer
	if err := db.Migrate(ctx, pool, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("control: a normal database refused to migrate: %v", err)
	}
	if !strings.Contains(logs.String(), "nothing to apply") {
		t.Fatalf("control: a re-run did something: %s", logs.String())
	}
	if err := db.SkippedMigrationCheckForTest(ctx, pool, nil); err != nil {
		t.Fatalf("control: the check refused a normal database: %v", err)
	}

	// The merge-order case: this database records 0011 and never saw a 0010.
	var max int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&max); err != nil || max != 11 {
		t.Fatalf("fixture: max version %d (%v); want 11", max, err)
	}
	if n := countRows(ctx, t, pool, "schema_migrations WHERE version = 10"); n != 0 {
		t.Fatalf("fixture: version 10 is already recorded")
	}
	err := db.SkippedMigrationCheckForTest(ctx, pool, map[int]string{10: "0010_sessions_not_before.sql"})
	if !errors.Is(err, db.ErrSkippedMigration) || !strings.Contains(err.Error(), "0010_sessions_not_before.sql") {
		t.Fatalf("an embedded 0010 below an applied 0011: %v; want ErrSkippedMigration naming the file", err)
	}

	// End to end: forget an embedded version below the max; Migrate refuses.
	mustExec(ctx, t, pool, `DELETE FROM schema_migrations WHERE version = 9`)
	before := countRows(ctx, t, pool, "schema_migrations")
	err = db.Migrate(ctx, pool, log.New(io.Discard, "", 0))
	if !errors.Is(err, db.ErrSkippedMigration) || !strings.Contains(err.Error(), "0009_per_user_scoping.sql") {
		t.Fatalf("Migrate over a database missing 0009 below 0011: %v; want ErrSkippedMigration naming 0009", err)
	}
	if after := countRows(ctx, t, pool, "schema_migrations"); after != before {
		t.Fatalf("a refused Migrate recorded %d migration(s)", after-before)
	}
}
