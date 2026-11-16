package db_test

import (
	"strings"
	"testing"
)

// TestMigration0136OfficerDeclared: at 0135 officer_defs has no runtime
// columns; at 0136 an existing def is untouched (runtime NULL: an env
// officer's def, as before). A declared row needs its instance slug and the
// slug a declaration (one without the other is refused), and the slug is
// unique ACROSS OWNERS — user 2 cannot take user 1's. The shapes refuse a host
// on a pod officer, an unknown runtime, max_instances past the cap and a poll
// under the floor. Re-applying the body is harmless, and the header's
// ROLLBACK block runs as written.
func TestMigration0136OfficerDeclared(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0136_officer_declared", 135)
	if _, err := pool.Exec(ctx, `SELECT runtime FROM officer_defs`); err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("control: before 0136 officer_defs.runtime exists: %v", err)
	}
	two := itoa(scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`))
	mustExec(ctx, t, pool, `INSERT INTO officer_types (owner_id, key) VALUES (1, 'scout'), (`+two+`, 'scout')`)
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key) VALUES (1, 'scout:env', 'scout')`)
	applyThroughFrom(ctx, t, pool, 136)
	var runtime *string
	if err := pool.QueryRow(ctx, `SELECT runtime FROM officer_defs WHERE officer = 'scout:env'`).Scan(&runtime); err != nil || runtime != nil {
		t.Fatalf("an existing def after 0136: runtime %v (%v), want NULL", runtime, err)
	}
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'scout:main', 'scout', 'pod', 'scout-main')`)
	for _, bad := range []struct{ why, sql string }{
		{"another owner's row with a taken slug", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (` + two + `, 'scout:main', 'scout', 'pod', 'scout-main')`},
		{"a runtime with no slug", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime) VALUES (1, 'scout:a', 'scout', 'pod')`},
		{"a slug with no runtime", `INSERT INTO officer_defs (owner_id, officer, type_key, instance_slug) VALUES (1, 'scout:b', 'scout', 'scout-b')`},
		{"an unknown runtime", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'scout:c', 'scout', 'vm', 'scout-c')`},
		{"a host on a pod officer", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, host, instance_slug) VALUES (1, 'scout:d', 'scout', 'pod', 'box', 'scout-d')`},
		{"max_instances past the cap", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, max_instances, instance_slug) VALUES (1, 'scout:e', 'scout', 'pod', 9, 'scout-e')`},
		{"a poll under the floor", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, poll_seconds, instance_slug) VALUES (1, 'scout:f', 'scout', 'pod', 5, 'scout-f')`},
		{"a slug that is not a label", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'scout:g', 'scout', 'pod', 'Scout_G')`},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (`+two+`, 'scout:main', 'scout', 'pod', 'scout-main-2')`)
	if _, err := pool.Exec(ctx, readMigration(t, "0136_officer_declared.sql")); err != nil {
		t.Fatalf("re-applying 0136: %v", err)
	}
	if n := countRows(ctx, t, pool, "officer_defs"); n != 3 {
		t.Fatalf("re-applying 0136 left %d defs, want 3", n)
	}
	for _, q := range asksRollbackSQL(t, "0136_officer_declared.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT runtime FROM officer_defs`); err == nil {
		t.Error("after the rollback officer_defs.runtime still exists")
	}
}
