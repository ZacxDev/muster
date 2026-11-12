package db_test

import (
	"strings"
	"testing"
)

// TestMigration0117CodenameFirstIsOffForEveryone: a database at 0116 whose
// notify_preferences has a user's row (they turned "Show tmux sessions" on)
// has no codename_first; at 0117 it does, OFF on that existing row (nothing
// backfilled on) and on a row written with no value (the column default),
// with tmux_address untouched. Then the header's ROLLBACK block runs as
// written and leaves the table with no such column, its rows intact.
func TestMigration0117CodenameFirstIsOffForEveryone(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0117_codename_first", 116)
	mustExec(ctx, t, pool, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	mustExec(ctx, t, pool, `INSERT INTO notify_preferences (owner_id, tmux_address) VALUES (2, true)`)
	if _, err := pool.Exec(ctx, `SELECT codename_first FROM notify_preferences`); err == nil || !strings.Contains(err.Error(), "codename_first") {
		t.Fatalf("control: before 0117 codename_first exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 117)
	var first, tmux bool
	if err := pool.QueryRow(ctx, `SELECT codename_first, tmux_address FROM notify_preferences WHERE owner_id = 2`).Scan(&first, &tmux); err != nil || first || !tmux {
		t.Fatalf("the existing row at 0117: codename_first=%v tmux_address=%v err=%v, want false and true", first, tmux, err)
	}
	mustExec(ctx, t, pool, `INSERT INTO notify_preferences (owner_id) VALUES (3)`)
	if err := pool.QueryRow(ctx, `SELECT codename_first FROM notify_preferences WHERE owner_id = 3`).Scan(&first); err != nil || first {
		t.Fatalf("a row written with no value: codename_first=%v err=%v, want false", first, err)
	}
	if n := countRows(ctx, t, pool, "notify_preferences"); n != 2 {
		t.Fatalf("0117 changed the row count to %d, want 2", n)
	}
	rb := asksRollbackSQL(t, "0117_notify_codename_first.sql")
	if len(rb) != 2 {
		t.Fatalf("the ROLLBACK block has %d statement(s), want 2: %q", len(rb), rb)
	}
	for _, q := range rb {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT codename_first FROM notify_preferences`); err == nil {
		t.Error("after the rollback codename_first still exists")
	}
	if n := countRows(ctx, t, pool, "notify_preferences"); n != 2 {
		t.Errorf("the rollback left %d row(s), want the 2 it found", n)
	}
	var v int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v); err != nil || v != 116 {
		t.Errorf("after the rollback schema_migrations' max is %d (err %v), want 116", v, err)
	}
}
