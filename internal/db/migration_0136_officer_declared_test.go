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
	mustExec(ctx, t, pool, `INSERT INTO officer_types (owner_id, key) VALUES (1, 'scout-own'), (1, 'scout-other')`)
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'scout:main', 'scout', 'pod', 'scout-main')`)
	// F5: a declared def's type may be its own, keyed by its slug.
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'scout:own', 'scout-own', 'pod', 'scout-own')`)
	for _, bad := range []struct{ why, sql string }{
		{"another owner's row with a taken slug", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (` + two + `, 'scout:main', 'scout', 'pod', 'scout-main')`},
		{"a runtime with no slug", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime) VALUES (1, 'scout:a', 'scout', 'pod')`},
		{"a slug with no runtime", `INSERT INTO officer_defs (owner_id, officer, type_key, instance_slug) VALUES (1, 'scout:b', 'scout', 'scout-b')`},
		{"an unknown runtime", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'scout:c', 'scout', 'vm', 'scout-c')`},
		{"a host on a pod officer", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, host, instance_slug) VALUES (1, 'scout:d', 'scout', 'pod', 'box', 'scout-d')`},
		{"max_instances past the cap", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, max_instances, instance_slug) VALUES (1, 'scout:e', 'scout', 'pod', 9, 'scout-e')`},
		{"a poll under the floor", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, poll_seconds, instance_slug) VALUES (1, 'scout:f', 'scout', 'pod', 5, 'scout-f')`},
		{"a slug that is not a label", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'scout:g', 'scout', 'pod', 'Scout_G')`},
		// F5: a declared def's own type is its slug's, never another key.
		{"a declared def on a type that is neither its id's nor its slug's", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (1, 'scout:h', 'scout-other', 'pod', 'scout-h')`},
		{"an env def (no runtime) on a type that is not its id's", `INSERT INTO officer_defs (owner_id, officer, type_key) VALUES (1, 'scout:i', 'scout-own')`},
		// Round 2: the stored auto placement is an auto officer's alone, a
		// host name, and always has its time.
		{"an auto placement on a pod officer", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug, auto_host, auto_host_at) VALUES (1, 'scout:j', 'scout', 'pod', 'scout-j', 'box', now())`},
		{"an auto placement with no time", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug, auto_host) VALUES (1, 'scout:k', 'scout', 'auto', 'scout-k', 'box')`},
		{"an auto placement time with no host", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug, auto_host_at) VALUES (1, 'scout:l', 'scout', 'auto', 'scout-l', now())`},
		{"an auto placement that is not a host name", `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug, auto_host, auto_host_at) VALUES (1, 'scout:m', 'scout', 'auto', 'scout-m', 'Box_1', now())`},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (`+two+`, 'scout:main', 'scout', 'pod', 'scout-main-2')`)
	// Round 2: an auto officer's placement, and a recorded window's count of
	// missing listings (0 by default, never negative).
	mustExec(ctx, t, pool, `INSERT INTO officer_types (owner_id, key) VALUES (1, 'scout-auto')`)
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug, auto_host, auto_host_at) VALUES (1, 'scout:auto', 'scout-auto', 'auto', 'scout-auto', 'box', now())`)
	mustExec(ctx, t, pool, `INSERT INTO officer_host_spawns (owner_id, officer, instance, host) VALUES (1, 'scout:auto', 'officer-scout-auto-1', 'box')`)
	var missing int
	if err := pool.QueryRow(ctx, `SELECT missing_listings FROM officer_host_spawns`).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("a spawn's missing_listings = %d (%v), want 0", missing, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE officer_host_spawns SET missing_listings = -1`); err == nil {
		t.Error("accepted a negative missing_listings")
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0136_officer_declared.sql")); err != nil {
		t.Fatalf("re-applying 0136: %v", err)
	}
	if n := countRows(ctx, t, pool, "officer_defs"); n != 5 {
		t.Fatalf("re-applying 0136 left %d defs, want 5", n)
	}
	for _, q := range asksRollbackSQL(t, "0136_officer_declared.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT runtime FROM officer_defs`); err == nil {
		t.Error("after the rollback officer_defs.runtime still exists")
	}
	if _, err := pool.Exec(ctx, `SELECT auto_host FROM officer_defs`); err == nil {
		t.Error("after the rollback officer_defs.auto_host still exists")
	}
	if _, err := pool.Exec(ctx, `SELECT missing_listings FROM officer_host_spawns`); err == nil {
		t.Error("after the rollback officer_host_spawns.missing_listings still exists")
	}
}
