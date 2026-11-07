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

	"github.com/ZacxDev/muster/internal/db"
)

// asksMigration is the asks migration's file name and version, found by name
// (it may be renumbered at merge), and the version of the file before it.
func asksMigration(t *testing.T) (name string, v, prev int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for i, f := range files {
		if b := filepath.Base(f); len(b) != len("0000_asks.sql") || !strings.HasSuffix(b, "_asks.sql") {
			continue
		}
		if name != "" || i == 0 {
			t.Fatalf("the asks migration: %v", files)
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
		t.Fatal("no *_asks.sql migration")
	}
	return name, v, prev
}

// asksRollbackSQL is the statements of the migration header's ROLLBACK block.
func asksRollbackSQL(t *testing.T, name string) []string {
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

// TestMigrationAsksAppliesAndRollsBack: before the migration there is no asks
// table (the control); after it the table takes a row of every kind, refuses
// each value outside its vocabulary, a purged row holding text and options
// that are not an array; a source ref is unique per owner and kind while ”
// (plain text) repeats; an episode's deletion leaves the ask, unlinked; it is
// under FORCED row-level security; RLSOn re-applies the file RLSOff undid;
// and the header's rollback SQL — which deletes only its own schema_migrations
// row — removes it, after which the file applies again.
func TestMigrationAsksAppliesAndRollsBack(t *testing.T) {
	ctx := ctx0006(t)
	name, v, prev := asksMigration(t)
	if v != db.AsksMigrationVersionForTest {
		t.Fatalf("rollback.go re-applies version %d for asks, but the file is %s: renumber them together", db.AsksMigrationVersionForTest, name)
	}
	pool := freshAt(ctx, t, "ms_asks", prev)
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return ""
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM asks`); code(err) != "42P01" {
		t.Fatalf("control: before %s asks exists: %v", name, err)
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	applyThroughFrom(ctx, t, pool, v)

	for _, k := range []string{"plain_text", "ask_user_question", "attention_ask", "opencode_question", "opencode_permission"} {
		mustExec(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, source_ref, question) VALUES (1, $1, 'claude-code', $1, 'ok?')`, k)
	}
	mustExec(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, question, answered_by, outcome, answered_at) VALUES (1, 'plain_text', 'agent', 'a', 'auto:12', 'answered', now())`)
	mustExec(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, answered_by, outcome) VALUES (1, 'plain_text', 'agent', 'other', 'answered')`)
	mustExec(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, outcome) VALUES (1, 'plain_text', 'agent', 'unknown')`)
	mustExec(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, text_purged_at) VALUES (1, 'plain_text', 'agent', now())`)
	mustExec(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, question) VALUES (1, 'plain_text', 'opencode', 'same')`)
	mustExec(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, question) VALUES (1, 'plain_text', 'opencode', 'same')`)
	for _, bad := range []struct{ sql, why string }{
		{`INSERT INTO asks (owner_id, kind, runtime) VALUES (1, 'permission_prompt', 'claude-code')`, "a later kind"},
		{`INSERT INTO asks (owner_id, kind, runtime) VALUES (1, 'plain_text', 'hermes')`, "an unknown runtime"},
		{`INSERT INTO asks (owner_id, kind, runtime, outcome) VALUES (1, 'plain_text', 'agent', 'ignored')`, "an unknown outcome"},
		{`INSERT INTO asks (owner_id, kind, runtime, answered_by) VALUES (1, 'plain_text', 'agent', 'auto:0')`, "auto with no rule id"},
		{`INSERT INTO asks (owner_id, kind, runtime, answered_by) VALUES (1, 'plain_text', 'agent', 'operator')`, "attention's word, not the log's"},
		{`INSERT INTO asks (owner_id, kind, runtime, answer_provenance) VALUES (1, 'plain_text', 'agent', 'typed')`, "an unknown provenance"},
		{`INSERT INTO asks (owner_id, kind, runtime, question, text_purged_at) VALUES (1, 'plain_text', 'agent', 'kept?', now())`, "a purged row holding text"},
		{`INSERT INTO asks (owner_id, kind, runtime, options, text_purged_at) VALUES (1, 'plain_text', 'agent', '["kept"]', now())`, "a purged row holding options"},
		{`INSERT INTO asks (owner_id, kind, runtime, options) VALUES (1, 'plain_text', 'agent', '{"a":1}')`, "options that are not an array"},
		{`INSERT INTO asks (owner_id, kind, runtime, question) VALUES (1, 'plain_text', 'agent', repeat('x', 4001))`, "a question past its bound"},
		{`INSERT INTO asks (owner_id, kind, runtime, jev_label) VALUES (1, 'plain_text', 'agent', 'maybe')`, "an unknown classifier label"},
	} {
		if _, err := pool.Exec(ctx, bad.sql); code(err) != "23514" {
			t.Errorf("%s: %v, want a CHECK violation", bad.why, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO asks (owner_id, kind, runtime, source_ref) VALUES (1, 'attention_ask', 'agent', 'attention_ask')`); code(err) != "23505" {
		t.Errorf("a second row for one source: %v, want a unique violation", err)
	}
	// The same ref under another kind is another source.
	mustExec(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, source_ref) VALUES (1, 'plain_text', 'agent', 'attention_ask')`)
	// Another owner's episode cannot be linked (the composite key).
	mustExec(ctx, t, pool, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (77, 'member') ON CONFLICT (id) DO NOTHING`)
	theirs := scanID(ctx, t, pool, `INSERT INTO session_episodes (owner_id, session_kind, session_key, seq, state) VALUES (77, 'mirror', 'sess-y', 1, 'stalled') RETURNING id`)
	if _, err := pool.Exec(ctx, `INSERT INTO asks (owner_id, kind, runtime, episode_id) VALUES (1, 'plain_text', 'claude-code', $1)`, theirs); code(err) != "23503" {
		t.Errorf("an ask naming another owner's episode: %v, want a foreign-key violation", err)
	}
	// An episode deleted leaves its asks, unlinked.
	ep := scanID(ctx, t, pool, `INSERT INTO session_episodes (owner_id, session_kind, session_key, seq, state) VALUES (1, 'mirror', 'sess-x', 1, 'stalled') RETURNING id`)
	ask := scanID(ctx, t, pool, `INSERT INTO asks (owner_id, kind, runtime, episode_id) VALUES (1, 'plain_text', 'claude-code', $1) RETURNING id`, ep)
	mustExec(ctx, t, pool, `DELETE FROM session_episodes WHERE id = $1`, ep)
	var linked *int64
	if err := pool.QueryRow(ctx, `SELECT episode_id FROM asks WHERE id = $1`, ask).Scan(&linked); err != nil || linked != nil {
		t.Fatalf("after its episode's deletion the ask links %v (%v), want NULL", linked, err)
	}
	forced := func() (bool, bool) {
		var enabled, forced bool
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.asks'::regclass`).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		return enabled, forced
	}
	if e, f := forced(); !e || !f {
		t.Fatalf("row-level security enabled %v forced %v, want both", e, f)
	}
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if e, f := forced(); e || f {
		t.Fatalf("after RLSOff: enabled %v forced %v, want neither", e, f)
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if e, f := forced(); !e || !f {
		t.Fatalf("after RLSOn: enabled %v forced %v, want both", e, f)
	}

	rb := asksRollbackSQL(t, name)
	if len(rb) != 3 || rb[2] != fmt.Sprintf("DELETE FROM schema_migrations WHERE version = %d", v) {
		t.Fatalf("the header's rollback SQL is %q; want the two DROPs and the delete of version %d only", rb, v)
	}
	for _, q := range rb {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM asks`); code(err) != "42P01" {
		t.Fatalf("after the rollback asks exists: %v", err)
	}
	var idx *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.session_episodes_id_owner_key')::text`).Scan(&idx); err != nil || idx != nil {
		t.Fatalf("after the rollback the episode key index is %v (%v), want gone", idx, err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&after); err != nil || after != before {
		t.Fatalf("after the rollback schema_migrations has %d rows (%v), want the %d from before", after, err, before)
	}
	applyThroughFrom(ctx, t, pool, v)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM asks`); err != nil {
		t.Fatalf("re-applied: %v", err)
	}
}
