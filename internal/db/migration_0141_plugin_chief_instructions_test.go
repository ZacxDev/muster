package db_test

import (
	"strings"
	"testing"
)

// TestMigration0141PluginChiefInstructions: at 0140 plugins refuses the
// chief_extension capability and there is no plugin_chief_instructions (the
// control); at 0141 a plugin takes it, and its one section is held to the
// schema's rules — the sha IS the text's, an approval names the current sha
// or nothing and has its time, the text is 1..4096 bytes, one row per plugin,
// and a row cannot name another owner's plugin. The plugin row's delete takes
// the section with it. Re-applying the body is harmless, and the header's
// ROLLBACK block runs as written.
func TestMigration0141PluginChiefInstructions(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0141_chief_instr", 140)
	if _, err := pool.Exec(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities)
		VALUES (1, 'example-early', 'Example', 'hash-early', 'none', '{chief_extension}')`); err == nil {
		t.Fatal("control: before 0141 plugins accepted the chief_extension capability")
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM plugin_chief_instructions`); err == nil {
		t.Fatal("control: before 0141 plugin_chief_instructions exists")
	}
	two := itoa(scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`))
	applyThroughFrom(ctx, t, pool, 141)

	p1 := itoa(scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities)
		VALUES (1, 'example-guide', 'Example', 'hash-one', 'none', '{chief_extension}') RETURNING id`))
	p2 := itoa(scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities)
		VALUES (`+two+`, 'example-other', 'Example', 'hash-two', 'example-other', '{messages,chief_extension}') RETURNING id`))
	const hashOf = `encode(sha256(convert_to('first section text', 'UTF8')), 'hex')`
	mustExec(ctx, t, pool, `INSERT INTO plugin_chief_instructions (owner_id, plugin_id, text, sha) VALUES (1, `+p1+`, 'first section text', `+hashOf+`)`)
	mustExec(ctx, t, pool, `UPDATE plugin_chief_instructions SET approved_sha = sha, approved_at = now() WHERE plugin_id = `+p1)
	for _, bad := range []struct{ why, sql string }{
		{"a second row for one plugin", `INSERT INTO plugin_chief_instructions (owner_id, plugin_id, text, sha) VALUES (1, ` + p1 + `, 'other text', encode(sha256(convert_to('other text', 'UTF8')), 'hex'))`},
		{"a sha that is not the text's", `INSERT INTO plugin_chief_instructions (owner_id, plugin_id, text, sha) VALUES (` + two + `, ` + p2 + `, 'second text', repeat('a', 64))`},
		{"another owner's plugin", `INSERT INTO plugin_chief_instructions (owner_id, plugin_id, text, sha) VALUES (1, ` + p2 + `, 'second text', encode(sha256(convert_to('second text', 'UTF8')), 'hex'))`},
		{"an empty text", `INSERT INTO plugin_chief_instructions (owner_id, plugin_id, text, sha) VALUES (` + two + `, ` + p2 + `, '', encode(sha256(convert_to('', 'UTF8')), 'hex'))`},
		{"a text over 4096 bytes", `INSERT INTO plugin_chief_instructions (owner_id, plugin_id, text, sha) VALUES (` + two + `, ` + p2 + `, repeat('x', 4097), encode(sha256(convert_to(repeat('x', 4097), 'UTF8')), 'hex'))`},
		{"new text that keeps the old approval", `UPDATE plugin_chief_instructions SET text = 'changed text', sha = encode(sha256(convert_to('changed text', 'UTF8')), 'hex') WHERE plugin_id = ` + p1},
		{"an approval of another sha", `UPDATE plugin_chief_instructions SET approved_sha = repeat('b', 64) WHERE plugin_id = ` + p1},
		{"an approval with no time", `UPDATE plugin_chief_instructions SET approved_at = NULL WHERE plugin_id = ` + p1},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	// The boundary that IS allowed: exactly 4096 bytes.
	mustExec(ctx, t, pool, `INSERT INTO plugin_chief_instructions (owner_id, plugin_id, text, sha) VALUES (`+two+`, `+p2+`, repeat('y', 4096), encode(sha256(convert_to(repeat('y', 4096), 'UTF8')), 'hex'))`)
	if _, err := pool.Exec(ctx, readMigration(t, "0141_plugin_chief_instructions.sql")); err != nil {
		t.Fatalf("re-applying 0141: %v", err)
	}
	if n := countRows(ctx, t, pool, "plugin_chief_instructions"); n != 2 {
		t.Fatalf("re-applying 0141 left %d rows, want 2", n)
	}
	mustExec(ctx, t, pool, `DELETE FROM plugins WHERE id = `+p2)
	if n := countRows(ctx, t, pool, "plugin_chief_instructions"); n != 1 {
		t.Fatalf("after deleting a plugin: %d sections, want 1 (its own went with it)", n)
	}
	for _, q := range asksRollbackSQL(t, "0141_plugin_chief_instructions.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM plugin_chief_instructions`); err == nil {
		t.Error("after the rollback plugin_chief_instructions still exists")
	}
	var caps string
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT capabilities::text, revoked_at IS NOT NULL FROM plugins WHERE id = `+p1).Scan(&caps, &revoked); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(caps, "chief_extension") || !revoked {
		t.Errorf("after the rollback the chief_extension-only plugin: caps %s revoked %v; want it revoked, the capability gone", caps, revoked)
	}
}
