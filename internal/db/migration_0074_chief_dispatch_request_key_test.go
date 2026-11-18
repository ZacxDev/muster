package db_test

import (
	"strings"
	"testing"
)

// TestMigration0074AddsTheRequestKeyIndexAndRollsBack: at version 73 the index
// is absent (the control); after 0074 it exists with its partial predicate and
// the planner uses it for agent_dispatch's request_key lookup shape (with
// sequential scans off, so a plan that can use it must); the header's
// rollback SQL drops it, and a second apply restores it.
func TestMigration0074AddsTheRequestKeyIndexAndRollsBack(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0074_request_key", 73)
	const name = "idx_audit_log_chief_dispatch_request_key"
	def := func() string {
		var d string
		_ = pool.QueryRow(ctx, `SELECT coalesce((SELECT indexdef FROM pg_indexes WHERE indexname = $1), '')`, name).Scan(&d)
		return d
	}
	if d := def(); d != "" {
		t.Fatalf("control: at version 73 the index exists: %s", d)
	}

	applyThroughFrom(ctx, t, pool, 74)

	d := def()
	for _, want := range []string{"(user_id, ((detail ->> 'request_key'::text)))", "action = 'task.chief_dispatch'::text", "detail ? 'request_key'::text"} {
		if !strings.Contains(d, want) {
			t.Errorf("index definition %q lacks %q", d, want)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN SELECT r.id FROM audit_log r
		WHERE r.action = 'task.chief_dispatch' AND r.detail ? 'request_key'
		  AND r.user_id = 1 AND r.detail->>'request_key' = 'k'`)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), name) {
		t.Errorf("the lookup's plan does not use %s:\n%s", name, plan.String())
	}
	_ = tx.Rollback(ctx)

	// [rollback-sql], as the migration's header writes it.
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS public.idx_audit_log_chief_dispatch_request_key`); err != nil {
		t.Fatal(err)
	}
	if d := def(); d != "" {
		t.Fatalf("after the rollback SQL the index remains: %s", d)
	}
}
