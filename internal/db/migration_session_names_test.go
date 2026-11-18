package db_test

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sessionNamesMigration is the session-names migration's file name, version
// and predecessor's version, found by name (a sibling branch may take its
// number first and renumber it at merge; the test follows the file).
func sessionNamesMigration(t *testing.T) (name string, v, prev int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for i, f := range files {
		if !strings.HasSuffix(f, "_session_names.sql") {
			continue
		}
		name = filepath.Base(f)
		if _, err := fmt.Sscanf(name, "%04d_", &v); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Sscanf(filepath.Base(files[i-1]), "%04d_", &prev); err != nil {
			t.Fatal(err)
		}
	}
	if name == "" {
		t.Fatal("no *_session_names.sql migration")
	}
	return name, v, prev
}

// TestMigrationSessionNamesBackfillsWhoSetEachDisplayName: before the
// migration agents have no display_name_source (the control); after it each
// existing row is classified by the header's stated rule — a chief slug
// ("chief", "chief-<id>") system, the slug copied default, "#<id> · …"
// dispatch, "officer <id> #<n>" system, anything else manual — and agent_focus carries the name columns with their bounds: an
// agent row refuses a manual name, and an over-long one is refused. Applying
// it twice changes nothing.
func TestMigrationSessionNamesBackfillsWhoSetEachDisplayName(t *testing.T) {
	name, v, prev := sessionNamesMigration(t)
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_session_names", prev)
	rows := map[string]string{
		"slug-copied":  "slug-copied",
		"from-task":    "#42 · Fix the login bug",
		"from-task-id": "#7",
		"an-officer":   "officer triage #2",
		// The REAL chief slugs (agents.ChiefName, agents.ChiefNameFor): the
		// operator's chief is slug "chief" with display name "chief" (slug
		// and display name EQUAL), a member's is "chief-<id>".
		"chief":        "chief",
		"chief-7":      "chief",
		"a-renamed-ch": "chief",
		"renamed":      "My billing work",
		// "#42 fix" is not dispatchDisplayName's shape: a person typed it.
		"hash-typed": "#42 fix",
	}
	// One chief per owner (agents_one_chief_per_owner): each "chief"-named
	// row has an owner of its own.
	mustExec(ctx, t, pool, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (7, 'member'), (8, 'member') ON CONFLICT (id) DO NOTHING`)
	owners := map[string]int{"chief-7": 7, "a-renamed-ch": 8}
	for slug, display := range rows {
		owner := owners[slug]
		if owner == 0 {
			owner = 1
		}
		mustExec(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status, display_name) VALUES ($3, $1, 'ns-'||$1, 'stopped', $2)`, slug, display, owner)
	}
	if _, err := pool.Exec(ctx, `SELECT display_name_source FROM agents`); err == nil {
		t.Fatalf("control: before %s agents has display_name_source", name)
	}

	applyThroughFrom(ctx, t, pool, v)

	want := map[string]string{"slug-copied": "default", "from-task": "dispatch", "from-task-id": "dispatch", "an-officer": "system",
		"chief": "system", "chief-7": "system", "a-renamed-ch": "manual", "renamed": "manual", "hash-typed": "manual"}
	check := func(when string) {
		t.Helper()
		for slug, w := range want {
			var got string
			if err := pool.QueryRow(ctx, `SELECT display_name_source FROM agents WHERE name = $1`, slug).Scan(&got); err != nil || got != w {
				t.Errorf("%s: %s: source %q (%v), want %q", when, slug, got, err, w)
			}
		}
	}
	check("after " + name)
	mustExec(ctx, t, pool, readMigration(t, name))
	check("after re-applying " + name)

	if _, err := pool.Exec(ctx, `UPDATE agents SET display_name_source = 'robot' WHERE name = 'renamed'`); err == nil ||
		!strings.Contains(err.Error(), "agents_display_name_source_check") {
		t.Errorf("an unknown source: %v; want refused", err)
	}
	var agentID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM agents WHERE name = 'renamed'`).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, agent_id, title) VALUES (1, 'agent', $1::bigint::text, $1, 'Work')`, agentID)
	mustExec(ctx, t, pool, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, title) VALUES (1, 'mirror', 'sess-names-1', 'Work')`)
	var auto, manual string
	if err := pool.QueryRow(ctx, `SELECT name_auto, name_manual FROM agent_focus WHERE subject_key = 'sess-names-1'`).Scan(&auto, &manual); err != nil || auto != "" || manual != "" {
		t.Fatalf("a new row's names: %q %q (%v), want both empty", auto, manual, err)
	}
	for _, c := range []struct{ sql, constraint string }{
		{`UPDATE agent_focus SET name_manual = 'Mine' WHERE subject_kind = 'agent'`, "agent_focus_name_manual_mirror_only"},
		{`UPDATE agent_focus SET name_manual = repeat('x', 81) WHERE subject_kind = 'mirror'`, "agent_focus_name_manual_cap"},
		{`UPDATE agent_focus SET name_auto = repeat('x', 81) WHERE subject_kind = 'mirror'`, "agent_focus_name_auto_cap"},
	} {
		if _, err := pool.Exec(ctx, c.sql); err == nil || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("%s: %v; want refused by %s", c.sql, err, c.constraint)
		}
	}
	mustExec(ctx, t, pool, `UPDATE agent_focus SET name_manual = 'Mine' WHERE subject_kind = 'mirror'`)
}
