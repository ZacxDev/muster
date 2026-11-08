package db_test

import (
	"strings"
	"testing"
)

// TestMigration0094NotifyPreferencesIsOffForEveryone: a database at 0093 with
// users in it has no notify_preferences; at 0094 it does, with NO row for any
// existing user (nothing backfilled: no row reads as off) and a row written
// with no value is off (the column default). Then the header's ROLLBACK block
// runs as written and leaves no table.
func TestMigration0094NotifyPreferencesIsOffForEveryone(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0094_notify_prefs", 93)
	mustExec(ctx, t, pool, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM notify_preferences`); err == nil || !strings.Contains(err.Error(), "notify_preferences") {
		t.Fatalf("control: before 0094 notify_preferences exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 94)
	if n := countRows(ctx, t, pool, "notify_preferences"); n != 0 {
		t.Fatalf("0094 backfilled %d row(s); existing users must have none (off)", n)
	}
	mustExec(ctx, t, pool, `INSERT INTO notify_preferences (owner_id) VALUES (2)`)
	var on bool
	if err := pool.QueryRow(ctx, `SELECT tmux_address FROM notify_preferences WHERE owner_id = 2`).Scan(&on); err != nil || on {
		t.Fatalf("a row written with no value: tmux_address=%v err=%v, want false", on, err)
	}
	for _, q := range asksRollbackSQL(t, "0094_notify_preferences.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM notify_preferences`); err == nil {
		t.Error("after the rollback notify_preferences still exists")
	}
}
