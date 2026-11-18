package db_test

import (
	"strings"
	"testing"
)

// TestMigration0110KeysOfficerLLMUsageByPool: a database at 0109 holding a
// routine usage row arrives at 0110 with that row read as 'routine'; from then
// on the judgement pool's instance 1 and the routine pool's r1 each hold a row
// for one officer and day, a second row for the same (pool, instance, day) is
// still refused, and an unknown pool is refused. The header's rollback runs as
// written and restores the old key.
func TestMigration0110KeysOfficerLLMUsageByPool(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0110_llm_pool", 109)
	mustExec(ctx, t, pool, `INSERT INTO officer_llm_usage (owner_id, officer, instance, day, requests, cost_usd)
		VALUES (1, 'triage:example-web', 1, '2026-01-02', 3, 0.25)`)
	applyThroughFrom(ctx, t, pool, 110)

	var p string
	if err := pool.QueryRow(ctx, `SELECT pool FROM officer_llm_usage WHERE officer = 'triage:example-web'`).Scan(&p); err != nil || p != "routine" {
		t.Fatalf("the existing row's pool = %q (%v), want routine", p, err)
	}
	mustExec(ctx, t, pool, `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day, requests, cost_usd)
		VALUES (1, 'triage:example-web', 'judgement', 1, '2026-01-02', 1, 0.5)`)
	if n := countRows(ctx, t, pool, "officer_llm_usage"); n != 2 {
		t.Fatalf("rows = %d, want 2 (one per pool)", n)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day)
		VALUES (1, 'triage:example-web', 'judgement', 1, '2026-01-02')`); err == nil || !strings.Contains(err.Error(), "officer_llm_usage_one_pool_row") {
		t.Fatalf("a second judgement row for one instance and day: %v, want the unique refusal", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day)
		VALUES (1, 'triage:example-web', 'other', 2, '2026-01-02')`); err == nil || !strings.Contains(err.Error(), "officer_llm_usage_pool_check") {
		t.Fatalf("an unknown pool: %v, want the pool check", err)
	}
	// The proxy's own upsert shape books onto the pool's row.
	mustExec(ctx, t, pool, `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day, requests, cost_usd)
		VALUES (1, 'triage:example-web', 'judgement', 1, '2026-01-02', 1, 0.5)
		ON CONFLICT (owner_id, officer, pool, instance, day) DO UPDATE SET cost_usd = officer_llm_usage.cost_usd + EXCLUDED.cost_usd`)
	var usd float64
	if err := pool.QueryRow(ctx, `SELECT cost_usd::float8 FROM officer_llm_usage WHERE pool = 'judgement'`).Scan(&usd); err != nil || usd != 1.0 {
		t.Fatalf("judgement spend = %v (%v), want 1.0", usd, err)
	}

	for _, q := range asksRollbackSQL(t, "0110_officer_llm_usage_pool.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if n := countRows(ctx, t, pool, "officer_llm_usage"); n != 1 {
		t.Fatalf("after the rollback rows = %d, want the routine row only", n)
	}
	// An older image's booking works again.
	mustExec(ctx, t, pool, `INSERT INTO officer_llm_usage (owner_id, officer, instance, day, requests, cost_usd)
		VALUES (1, 'triage:example-web', 1, '2026-01-02', 1, 0.1)
		ON CONFLICT (owner_id, officer, instance, day) DO UPDATE SET requests = officer_llm_usage.requests + 1`)
}
