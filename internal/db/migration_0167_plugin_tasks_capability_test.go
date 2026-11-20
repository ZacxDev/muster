package db_test

import (
	"strings"
	"testing"
)

// TestMigration0167PluginTasksCapability: at 0166 plugins refuses the tasks
// capability and a pairing request may not name it (the controls); at 0167 a
// plugin takes tasks alone or beside another capability, a pairing request
// takes tasks alone or beside messages, and both still refuse an unknown
// capability, a pairing request still refuses a set with neither messages nor
// tasks. Re-applying the body is harmless, and the header's ROLLBACK block runs
// as written: the tasks-only plugin is revoked, the capability gone.
func TestMigration0167PluginTasksCapability(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0167_plugin_tasks", 166)
	const plugin = `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities) VALUES (1, `
	pair := func(id, caps string) string {
		return `INSERT INTO plugin_pair_requests (owner_id, public_id, name, label, data_scope, capabilities, code_hash, poll_secret_hash, expires_at)
			VALUES (1, '` + strings.Repeat(id, 22) + `', 'example-pair', 'Example', 'example-pair', '` + caps + `', repeat('a', 64), repeat('b', 64), now() + interval '1 hour')`
	}
	if _, err := pool.Exec(ctx, plugin+`'example-early', 'Example', 'hash-early', 'none', '{tasks}')`); err == nil {
		t.Fatal("control: before 0167 plugins accepted the tasks capability")
	}
	if _, err := pool.Exec(ctx, pair("e", "{tasks}")); err == nil {
		t.Fatal("control: before 0167 a pairing request accepted the tasks capability")
	}
	applyThroughFrom(ctx, t, pool, 167)

	p1 := itoa(scanID(ctx, t, pool, plugin+`'example-filer', 'Example', 'hash-one', 'none', '{tasks}') RETURNING id`))
	mustExec(ctx, t, pool, plugin+`'example-both', 'Example', 'hash-two', 'none', '{chief_extension,tasks}')`)
	mustExec(ctx, t, pool, pair("a", "{tasks}"))
	mustExec(ctx, t, pool, pair("c", "{messages,tasks}"))
	for _, bad := range []struct{ why, sql string }{
		{"an unknown plugin capability", plugin + `'example-bad', 'Example', 'hash-bad', 'none', '{tasks,admin}')`},
		{"a pairing request with an unknown capability", pair("f", "{tasks,viewer}")},
		{"a pairing request with neither messages nor tasks", pair("g", "{draft_checks}")},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0167_plugin_tasks_capability.sql")); err != nil {
		t.Fatalf("re-applying 0167: %v", err)
	}
	if _, err := pool.Exec(ctx, pair("h", "{tasks}")); err != nil {
		t.Fatalf("after re-applying 0167 a tasks pairing request: %v", err)
	}
	for _, q := range asksRollbackSQL(t, "0167_plugin_tasks_capability.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	var caps string
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT capabilities::text, revoked_at IS NOT NULL FROM plugins WHERE id = `+p1).Scan(&caps, &revoked); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(caps, "tasks") || !revoked {
		t.Errorf("after the rollback the tasks-only plugin: caps %s revoked %v; want it revoked, the capability gone", caps, revoked)
	}
	if _, err := pool.Exec(ctx, pair("i", "{tasks}")); err == nil {
		t.Error("after the rollback a pairing request accepted the tasks capability")
	}
}
