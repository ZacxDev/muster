package db_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// sessionTagsMigration is the session_tags migration's file name and version,
// found by name rather than spelled: the number may be renumbered at merge (a
// sibling branch can take it first), and the test must follow the file.
//
// prev is the version of the file before it: the schema the migration applies
// to. (It is v-1 on a contiguous tree; it is read, not computed, so the
// fixture is still the old schema on a branch where the predecessor has not
// merged yet — which TestEmbeddedMigrationsAreContiguousFrom0001 reports.)
func sessionTagsMigration(t *testing.T) (name string, v, prev int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for i, f := range files {
		if !strings.HasSuffix(f, "_session_tags.sql") {
			continue
		}
		if name != "" || i == 0 {
			t.Fatalf("the session_tags migration: %v", files)
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
		t.Fatal("no *_session_tags.sql migration")
	}
	return name, v, prev
}

// sessionTagsRollbackSQL is the statements of the migration header's ROLLBACK
// block (the "--   " lines under it), split on ';'.
func sessionTagsRollbackSQL(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	var sql []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "-- ROLLBACK") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "--") {
			break
		}
		if strings.HasPrefix(line, "--   ") {
			sql = append(sql, strings.TrimSpace(strings.TrimPrefix(line, "--")))
		}
	}
	var out []string
	for _, q := range strings.Split(strings.Join(sql, " "), ";") {
		if q = strings.TrimSpace(q); q != "" {
			out = append(out, q)
		}
	}
	return out
}

// TestMigrationSessionTagsAppliesAndRollsBack: before the migration there is no
// session_tags (the control); after it the table takes an agent row and a
// mirror row, refuses a row naming two subjects, a window-less mirror row
// without a session, and an upper-case tag; it is under FORCED row-level
// security; and the header's rollback SQL — which deletes only its OWN
// schema_migrations row — removes it, after which the file applies again.
func TestMigrationSessionTagsAppliesAndRollsBack(t *testing.T) {
	ctx := ctx0006(t)
	name, v, prev := sessionTagsMigration(t)
	pool := freshAt(ctx, t, "ms_session_tags", prev)
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return ""
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM session_tags`); code(err) != "42P01" {
		t.Fatalf("control: before %s session_tags exists: %v", name, err)
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	applyThroughFrom(ctx, t, pool, v)

	agent := scanID(ctx, t, pool, `INSERT INTO agents (name, namespace) VALUES ('synthetic-tagged', 'synthetic-ns') RETURNING id`)
	mustExec(ctx, t, pool, `INSERT INTO session_tags (owner_id, kind, agent_id, tag) VALUES (1, 'agent', $1, 'one')`, agent)
	mustExec(ctx, t, pool, `INSERT INTO session_tags (owner_id, kind, mirror_host, mirror_session, tag) VALUES (1, 'mirror', 'laptop', 'scratch2', 'one')`)
	for _, bad := range []struct{ sql, why string }{
		{`INSERT INTO session_tags (owner_id, kind, agent_id, mirror_host, mirror_session, tag) VALUES (1, 'agent', ` + fmt.Sprint(agent) + `, 'laptop', 's', 'x')`, "two subjects"},
		{`INSERT INTO session_tags (owner_id, kind, mirror_host, tag) VALUES (1, 'mirror', 'laptop', 'x')`, "a mirror row with no session"},
		{`INSERT INTO session_tags (owner_id, kind, mirror_host, mirror_session, tag) VALUES (1, 'mirror', 'laptop', 's', 'Upper')`, "an upper-case tag"},
		{`INSERT INTO session_tags (owner_id, kind, mirror_host, mirror_session, tag) VALUES (1, 'mirror', 'laptop', 's', '')`, "an empty tag"},
		{`INSERT INTO session_tags (owner_id, kind, mirror_host, mirror_session, tag) VALUES (1, 'window', 'laptop', 's', 'x')`, "an unknown kind"},
	} {
		if _, err := pool.Exec(ctx, bad.sql); code(err) != "23514" {
			t.Errorf("%s: %v, want a CHECK violation", bad.why, err)
		}
	}
	// The same tag on the same subject twice is the unique index's.
	if _, err := pool.Exec(ctx, `INSERT INTO session_tags (owner_id, kind, mirror_host, mirror_session, tag) VALUES (1, 'mirror', 'laptop', 'scratch2', 'one')`); code(err) != "23505" {
		t.Errorf("a duplicate tag: %v, want a unique violation", err)
	}
	var enabled, forced bool
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.session_tags'::regclass`).Scan(&enabled, &forced); err != nil || !enabled || !forced {
		t.Fatalf("row-level security enabled %v forced %v (%v), want both", enabled, forced, err)
	}
	// The cascade from agents.
	mustExec(ctx, t, pool, `DELETE FROM agents WHERE id = $1`, agent)
	if n := countRows(ctx, t, pool, `session_tags WHERE kind = 'agent'`); n != 0 {
		t.Fatalf("%d agent rows survive the agent", n)
	}

	rb := sessionTagsRollbackSQL(t, name)
	if len(rb) != 2 || rb[1] != fmt.Sprintf("DELETE FROM schema_migrations WHERE version = %d", v) {
		t.Fatalf("the header's rollback SQL is %q; want the DROP and the delete of version %d only", rb, v)
	}
	for _, q := range rb {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM session_tags`); code(err) != "42P01" {
		t.Fatalf("after the rollback session_tags exists: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&after); err != nil || after != before {
		t.Fatalf("after the rollback schema_migrations has %d rows (%v), want the %d from before", after, err, before)
	}
	applyThroughFrom(ctx, t, pool, v)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM session_tags`); err != nil {
		t.Fatalf("re-applied: %v", err)
	}
}
