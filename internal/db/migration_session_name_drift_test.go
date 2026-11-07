package db_test

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestMigrationSessionNameDriftAddsABoundedDismissal: before the migration
// agent_focus has no drift_dismissed (the control); after it a row from before
// reads '' (nothing dismissed), a title is stored, and one past the title's
// bound is refused by agent_focus_drift_dismissed_cap. Found by name, as the
// number may be renumbered at merge.
func TestMigrationSessionNameDriftAddsABoundedDismissal(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var v, prev int
	for i, f := range files {
		if strings.HasSuffix(f, "_session_name_drift.sql") {
			if _, err := fmt.Sscanf(filepath.Base(f), "%04d_", &v); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Sscanf(filepath.Base(files[i-1]), "%04d_", &prev); err != nil {
				t.Fatal(err)
			}
		}
	}
	if v == 0 {
		t.Fatal("no *_session_name_drift.sql migration")
	}
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_session_name_drift", prev)
	mustExec(ctx, t, pool, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, title) VALUES (1, 'mirror', 'drift-old', 'A focus')`)
	mustExec(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status, created_at) VALUES (1, 'drift-agent', 'ns-drift-agent', 'stopped', '2000-01-02T03:04:05Z')`)
	if _, err := pool.Exec(ctx, `SELECT drift_dismissed FROM agent_focus`); err == nil {
		t.Fatal("control: agent_focus has drift_dismissed before the migration")
	}

	applyThroughFrom(ctx, t, pool, v)

	var got string
	if err := pool.QueryRow(ctx, `SELECT drift_dismissed FROM agent_focus WHERE subject_key = 'drift-old'`).Scan(&got); err != nil || got != "" {
		t.Fatalf("a row from before: %q (%v), want ''", got, err)
	}
	mustExec(ctx, t, pool, `UPDATE agent_focus SET drift_dismissed = 'Fixing the webhook' WHERE subject_key = 'drift-old'`)
	// An agent from before reads its creation as when its name took effect.
	var same bool
	if err := pool.QueryRow(ctx, `SELECT display_name_at = created_at FROM agents WHERE name = 'drift-agent'`).Scan(&same); err != nil || !same {
		t.Fatalf("an agent from before: display_name_at = created_at is %v (%v), want true", same, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_focus SET drift_dismissed = repeat('x', 81)`); err == nil ||
		!strings.Contains(err.Error(), "agent_focus_drift_dismissed_cap") {
		t.Fatalf("an over-long title: %v; want refused by agent_focus_drift_dismissed_cap", err)
	}
}
