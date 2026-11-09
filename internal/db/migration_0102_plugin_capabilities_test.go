package db_test

import (
	"strings"
	"testing"
)

// TestMigration0102KeepsEveryTokenItsRoutes: a plugin token minted at version
// 101 — live and revoked alike — arrives at 0102 with exactly {messages}, the
// capability of every route it reached before, and a row inserted without
// naming the column gets it too. The CHECKs refuse an unknown capability, a
// NULL element and the empty set, and admit {viewer} and {messages,viewer}.
// Re-applying the file changes nothing (it is idempotent).
func TestMigration0102KeepsEveryTokenItsRoutes(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0102_plugin_capabilities", 101)
	live := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-caps-live', 'Example inbox', 'pluginhash-caps-live', 'example') RETURNING id`)
	revoked := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, revoked_at)
		VALUES (1, 'plugin-caps-old', 'Example inbox', 'pluginhash-caps-old', 'example', now()) RETURNING id`)
	var hasColumn bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'plugins' AND column_name = 'capabilities')`).Scan(&hasColumn); err != nil || hasColumn {
		t.Fatalf("control: at version 101 plugins.capabilities exists=%v (%v); want absent", hasColumn, err)
	}

	applyThroughFrom(ctx, t, pool, 102)

	caps := func(id int64) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT capabilities::text FROM plugins WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for name, id := range map[string]int64{"live": live, "revoked": revoked} {
		if got := caps(id); got != "{messages}" {
			t.Errorf("the %s token from before 0102: capabilities %s; want {messages}", name, got)
		}
	}
	fresh := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-caps-new', 'Example inbox', 'pluginhash-caps-new', 'example') RETURNING id`)
	if got := caps(fresh); got != "{messages}" {
		t.Errorf("a token inserted after 0102 without capabilities: %s; want the {messages} default", got)
	}

	n := 0
	set := func(value string) error {
		n++
		_, err := pool.Exec(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities)
			VALUES (1, 'plugin-caps-' || $1::text, 'Example inbox', 'pluginhash-caps-' || $1::text, 'example', $2::text[])`,
			strings.Repeat("x", n), value)
		return err
	}
	for value, constraint := range map[string]string{
		"{admin}":          "plugins_capabilities_known",
		"{messages,admin}": "plugins_capabilities_known",
		"{NULL}":           "plugins_capabilities_known",
		"{}":               "plugins_capabilities_nonempty",
	} {
		if err := set(value); err == nil || !strings.Contains(err.Error(), constraint) {
			t.Errorf("capabilities %s: %v; want refused by %s", value, err, constraint)
		}
	}
	for _, value := range []string{"{viewer}", "{messages,viewer}"} {
		if err := set(value); err != nil {
			t.Errorf("capabilities %s: %v; want stored", value, err)
		}
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0102_plugin_capabilities.sql")); err != nil {
		t.Fatalf("re-applying 0102: %v", err)
	}
	if got := caps(live); got != "{messages}" {
		t.Errorf("after re-applying 0102 the live token's capabilities are %s; want {messages}", got)
	}
}
