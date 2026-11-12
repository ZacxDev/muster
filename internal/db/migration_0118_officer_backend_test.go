package db_test

import (
	"strings"
	"testing"
)

// TestMigration0118OfficerBackend: a database at 0117 has none of the
// built-in officer backend's tables; at 0118 it has all four, empty. Their
// constraints refuse what the store never writes — an officer whose type is
// not its id's, a claimed item with no holder, a finished item with no
// outcome (or an open one with one), a second LIVE item for one subject — and
// the composite keys refuse a row naming ANOTHER owner's type or officer. A
// finished item frees its subject for a new one. Re-applying the body
// (db.RLSOn) is harmless, and the header's ROLLBACK block runs as written.
func TestMigration0118OfficerBackend(t *testing.T) {
	tables := []string{"officer_types", "officer_defs", "officer_items", "officer_schedules"}
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0118_officer_backend", 117)
	for _, tbl := range tables {
		if _, err := pool.Exec(ctx, `SELECT 1 FROM `+tbl); err == nil || !strings.Contains(err.Error(), tbl) {
			t.Fatalf("control: before 0118 %s exists: %v", tbl, err)
		}
	}
	applyThroughFrom(ctx, t, pool, 118)
	for _, tbl := range tables {
		if n := countRows(ctx, t, pool, tbl); n != 0 {
			t.Fatalf("0118 backfilled %d row(s) into %s", n, tbl)
		}
	}
	two := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	mustExec(ctx, t, pool, `INSERT INTO officer_types (owner_id, key, charter) VALUES (1, 'marketing', 'post nothing unapproved')`)
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key) VALUES (1, 'marketing:example', 'marketing')`)
	item := scanID(ctx, t, pool, `INSERT INTO officer_items (owner_id, officer, kind, subject_key) VALUES (1, 'marketing:example', 'draft', 'launch') RETURNING id`)
	u2 := itoa(two)
	for _, bad := range []struct{ why, sql string }{
		{"an officer whose type is not its id's", `INSERT INTO officer_defs (owner_id, officer, type_key) VALUES (1, 'other:example', 'marketing')`},
		{"an officer of a type that does not exist", `INSERT INTO officer_defs (owner_id, officer, type_key) VALUES (1, 'triage:example', 'triage')`},
		{"user 2's officer of user 1's type", `INSERT INTO officer_defs (owner_id, officer, type_key) VALUES (` + u2 + `, 'marketing:example', 'marketing')`},
		{"an officer id that is not <type>:<area>", `INSERT INTO officer_defs (owner_id, officer, type_key) VALUES (1, 'marketing', 'marketing')`},
		{"an unknown mode", `INSERT INTO officer_defs (owner_id, officer, type_key, mode) VALUES (1, 'marketing:two', 'marketing', 'loud')`},
		{"an item of an undeclared officer", `INSERT INTO officer_items (owner_id, officer, kind, subject_key) VALUES (1, 'marketing:nobody', 'k', 's')`},
		{"user 2's item of user 1's officer", `INSERT INTO officer_items (owner_id, officer, kind, subject_key) VALUES (` + u2 + `, 'marketing:example', 'k', 's')`},
		{"a second live item for one subject", `INSERT INTO officer_items (owner_id, officer, kind, subject_key) VALUES (1, 'marketing:example', 'draft', 'launch')`},
		{"a claimed item with no holder", `INSERT INTO officer_items (owner_id, officer, kind, subject_key, status, lease_expires_at) VALUES (1, 'marketing:example', 'k', 's1', 'claimed', now())`},
		{"a done item with no outcome", `INSERT INTO officer_items (owner_id, officer, kind, subject_key, status) VALUES (1, 'marketing:example', 'k', 's2', 'done')`},
		{"an open item with an outcome", `INSERT INTO officer_items (owner_id, officer, kind, subject_key, outcome) VALUES (1, 'marketing:example', 'k', 's3', 'done')`},
		{"an escalated item whose outcome is done", `INSERT INTO officer_items (owner_id, officer, kind, subject_key, status, outcome) VALUES (1, 'marketing:example', 'k', 's4', 'escalated', 'done')`},
		{"an unknown tier", `INSERT INTO officer_items (owner_id, officer, kind, subject_key, tier) VALUES (1, 'marketing:example', 'k', 's5', 'cheap')`},
		{"a cost with no source", `INSERT INTO officer_items (owner_id, officer, kind, subject_key, cost_usd) VALUES (1, 'marketing:example', 'k', 's6', 1)`},
		{"eleven links", `INSERT INTO officer_items (owner_id, officer, kind, subject_key, links) VALUES (1, 'marketing:example', 'k', 's7', array_fill('x'::text, ARRAY[11]))`},
		{"a cron that is not five fields", `INSERT INTO officer_schedules (owner_id, officer, name, cron, kind, subject_key) VALUES (1, 'marketing:example', 'n', '0 6 * *', 'k', 's')`},
		{"user 2's schedule of user 1's officer", `INSERT INTO officer_schedules (owner_id, officer, name, cron, kind, subject_key) VALUES (` + u2 + `, 'marketing:example', 'n', '0 6 * * *', 'k', 's')`},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	// a finished item frees its subject (the live-subject index is partial)
	mustExec(ctx, t, pool, `UPDATE officer_items SET status = 'done', outcome = 'done' WHERE id = $1`, item)
	mustExec(ctx, t, pool, `INSERT INTO officer_items (owner_id, officer, kind, subject_key) VALUES (1, 'marketing:example', 'draft', 'launch')`)
	mustExec(ctx, t, pool, `INSERT INTO officer_schedules (owner_id, officer, name, cron, kind, subject_key) VALUES (1, 'marketing:example', 'nightly', '0 6 * * *', 'draft', 'nightly')`)
	if _, err := pool.Exec(ctx, readMigration(t, "0118_officer_backend.sql")); err != nil {
		t.Fatalf("re-applying 0118: %v", err)
	}
	for tbl, want := range map[string]int{"officer_types": 1, "officer_defs": 1, "officer_items": 2, "officer_schedules": 1} {
		if n := countRows(ctx, t, pool, tbl); n != want {
			t.Fatalf("re-applying 0118 left %d rows in %s, want %d", n, tbl, want)
		}
	}
	for _, q := range asksRollbackSQL(t, "0118_officer_backend.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	for _, tbl := range tables {
		if _, err := pool.Exec(ctx, `SELECT 1 FROM `+tbl); err == nil {
			t.Errorf("after the rollback %s still exists", tbl)
		}
	}
}
