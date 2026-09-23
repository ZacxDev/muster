package runbooks_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/runbooks"
)

// TestPGStoreRunbooks verifies the runbooks registry + the dispatch-run audit
// trail round-trip, including the JSONB spec and params. It needs a real
// Postgres (the store is Postgres-backed). Skipped unless
// MUSTER_TEST_DATABASE_URL points at a disposable database.
func TestPGStoreRunbooks(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := runbooks.NewPG(pool)
	name := "bump-dep-" + time.Now().Format("150405.000000")
	rb, err := store.CreateRunbook(ctx, runbooks.Runbook{
		Name:        name,
		DisplayName: "Bump a dependency",
		Description: "open a PR bumping a dep",
		Spec: runbooks.Spec{
			DefaultRepo:   "owner/app",
			Model:         "anthropic/claude-sonnet-4",
			BodyTemplate:  "Bump {{dep}} to {{version}} and open a PR.",
			Params:        []runbooks.ParamDef{{Name: "dep", Required: true}, {Name: "version", Default: "latest", Enum: []string{"latest", "pinned"}}},
			Steps:         []runbooks.Step{{Title: "Open PR", RequiresApproval: true}},
			GrantProfiles: []string{"k8s-read"},
		},
	})
	if err != nil {
		t.Fatalf("create runbook: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runbooks WHERE id=$1`, rb.ID) })
	if rb.ID == 0 {
		t.Fatal("created runbook has no id")
	}

	// Spec JSONB round-trips (params, enum, steps, grants).
	rt, err := store.GetRunbookByName(ctx, name)
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if rt.Spec.BodyTemplate == "" || len(rt.Spec.Params) != 2 ||
		rt.Spec.Params[1].Default != "latest" || len(rt.Spec.Params[1].Enum) != 2 ||
		len(rt.Spec.Steps) != 1 || !rt.Spec.Steps[0].RequiresApproval ||
		len(rt.Spec.GrantProfiles) != 1 {
		t.Fatalf("spec did not round-trip: %+v", rt.Spec)
	}

	// Render uses the round-tripped spec.
	body, err := rt.Render(map[string]string{"dep": "pgx"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "Bump pgx to latest") || !strings.Contains(body, "## Steps") {
		t.Fatalf("rendered body wrong:\n%s", body)
	}

	// ListRunbooks includes it.
	all, err := store.ListRunbooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !containsRunbook(all, rb.ID) {
		t.Fatalf("created runbook %d not in list (%d entries)", rb.ID, len(all))
	}

	// A run records and round-trips its params snapshot.
	noteID := int64(0)
	run, err := store.CreateRun(ctx, runbooks.Run{
		RunbookID: &rb.ID, RunbookName: name, AgentID: 4242, NoteID: &noteID,
		Params: map[string]string{"dep": "pgx", "version": "latest"}, RenderedBody: body,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.ID == 0 {
		t.Fatal("created run has no id")
	}
	runs, err := store.ListRunsForRunbook(ctx, rb.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Params["dep"] != "pgx" || runs[0].AgentID != 4242 {
		t.Fatalf("runs did not round-trip: %+v", runs)
	}

	// RunStatsForRunbooks: batched count + max(created_at) for the card badge.
	// Record a second run so the count is 2 and last-run advances.
	run2, err := store.CreateRun(ctx, runbooks.Run{
		RunbookID: &rb.ID, RunbookName: name, AgentID: 4243,
		Params: map[string]string{"dep": "pgx"}, RenderedBody: body,
	})
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}
	stats, err := store.RunStatsForRunbooks(ctx, []int64{rb.ID})
	if err != nil {
		t.Fatalf("run stats: %v", err)
	}
	st, ok := stats[rb.ID]
	if !ok || st.Count != 2 {
		t.Fatalf("run stats = %+v (ok=%v), want count 2", st, ok)
	}
	if st.LastRunAt.Before(run.CreatedAt) || st.LastRunAt.IsZero() {
		t.Fatalf("last run time = %v, want >= first run %v", st.LastRunAt, run.CreatedAt)
	}
	// An empty id list short-circuits to an empty map.
	if m, err := store.RunStatsForRunbooks(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("empty stats = %v, err %v; want empty map", m, err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runbook_runs WHERE id=$1`, run2.ID) })

	// LatestRunForAgent: agent 4242 had exactly one run (run); agent 4243 had run2.
	if got, ok, err := store.LatestRunForAgent(ctx, 4242); err != nil || !ok {
		t.Fatalf("latest run for 4242: ok=%v err=%v", ok, err)
	} else if got.ID != run.ID || got.RunbookName != name || got.Params["dep"] != "pgx" {
		t.Fatalf("latest run for 4242 = %+v, want run %d", got, run.ID)
	}
	if got, ok, err := store.LatestRunForAgent(ctx, 4243); err != nil || !ok || got.ID != run2.ID {
		t.Fatalf("latest run for 4243 = %+v ok=%v err=%v, want run2 %d", got, ok, err, run2.ID)
	}
	// An agent with no run → bool false, no error.
	if _, ok, err := store.LatestRunForAgent(ctx, 999999); err != nil || ok {
		t.Fatalf("latest run for unknown agent: ok=%v err=%v, want false/nil", ok, err)
	}

	// DeleteRunsOlderThan: a past cutoff deletes none of these fresh runs.
	if n, err := store.DeleteRunsOlderThan(ctx, run.CreatedAt.Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("delete with past cutoff = %d, err %v; want 0", n, err)
	}

	// Deleting the runbook nulls the run's runbook_id (history survives).
	if err := store.DeleteRunbook(ctx, rb.ID); err != nil {
		t.Fatalf("delete runbook: %v", err)
	}
	var orphanRunbookID *int64
	if err := pool.QueryRow(ctx, `SELECT runbook_id FROM runbook_runs WHERE id=$1`, run.ID).Scan(&orphanRunbookID); err != nil {
		t.Fatalf("read orphaned run: %v", err)
	}
	if orphanRunbookID != nil {
		t.Fatalf("run.runbook_id should be NULL after runbook delete, got %d", *orphanRunbookID)
	}

	// A future cutoff sweeps the remaining run rows (verifies the delete path).
	if n, err := store.DeleteRunsOlderThan(ctx, time.Now().Add(time.Hour)); err != nil || n < 1 {
		t.Fatalf("delete with future cutoff = %d, err %v; want >= 1", n, err)
	}
}

func containsRunbook(list []runbooks.Runbook, id int64) bool {
	for _, rb := range list {
		if rb.ID == id {
			return true
		}
	}
	return false
}
