package db_test

import (
	"strings"
	"testing"
)

// TestMigration0103TiesTheViewerLinkAndKeepsEveryToken: a plugin token minted at
// version 102 arrives at 0103 unchanged ({messages}, no origin, no entry URL).
// After it, plugins_viewer_link stores a viewer token only WITH an origin and
// an https entry URL, and refuses either on a token without viewer, or one
// without the other; plugin_handoffs refuses a malformed code hash, purpose or
// expiry and stores a well-formed row. Re-applying the file changes nothing.
func TestMigration0103TiesTheViewerLinkAndKeepsEveryToken(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0103_plugin_viewer", 102)
	old := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-viewer-old', 'Example inbox', 'pluginhash-viewer-old', 'example') RETURNING id`)
	var hasTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.plugin_handoffs') IS NOT NULL`).Scan(&hasTable); err != nil || hasTable {
		t.Fatalf("control: at version 102 plugin_handoffs exists=%v (%v); want absent", hasTable, err)
	}

	applyThroughFrom(ctx, t, pool, 103)

	var caps string
	var origin, entry *string
	if err := pool.QueryRow(ctx, `SELECT capabilities::text, origin, entry_url FROM plugins WHERE id = $1`, old).Scan(&caps, &origin, &entry); err != nil ||
		caps != "{messages}" || origin != nil || entry != nil {
		t.Fatalf("the token from before 0103: %s %v %v (%v); want {messages} with no origin or entry URL", caps, origin, entry, err)
	}

	n := 0
	plugin := func(caps string, origin, entry any) error {
		n++
		_, err := pool.Exec(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities, origin, entry_url)
			VALUES (1, 'plugin-v-' || $1::text, 'Example viewer', 'pluginhash-v-' || $1::text, 'none', $2::text[], $3, $4)`,
			strings.Repeat("x", n), caps, origin, entry)
		return err
	}
	const o, e = "https://*.example.net", "https://w.example.net/enter"
	for _, c := range []struct {
		caps          string
		origin, entry any
		constraint    string
	}{
		{"{viewer}", nil, nil, "plugins_viewer_link"},
		{"{viewer}", o, nil, "plugins_viewer_link"},
		{"{viewer}", nil, e, "plugins_viewer_link"},
		{"{messages}", o, e, "plugins_viewer_link"},
		{"{viewer}", o, "http://w.example.net/enter", "plugins_entry_url_check"},
	} {
		if err := plugin(c.caps, c.origin, c.entry); err == nil || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("capabilities %s, origin %v, entry %v: %v; want refused by %s", c.caps, c.origin, c.entry, err, c.constraint)
		}
	}
	for _, c := range []string{"{viewer}", "{messages,viewer}"} {
		if err := plugin(c, o, e); err != nil {
			t.Errorf("capabilities %s with an origin and entry URL: %v; want stored", c, err)
		}
	}

	m := 0
	handoff := func(hash, purpose string, ttl string) error {
		m++
		_, err := pool.Exec(ctx, `INSERT INTO plugin_handoffs (owner_id, plugin_id, code_hash, target, purpose, stepped_up, minted_at, expires_at)
			VALUES (1, $1, $2, '{"mirror":{"host":"h","session":"s","window":"@1"}}', $3, false, now(), now() + $4::interval)`,
			old, hash, purpose, ttl)
		return err
	}
	hash := func() string { return strings.Repeat(string(rune('a'+m)), 64) }
	for _, c := range []struct{ hash, purpose, ttl, constraint string }{
		{"not-a-hash", "view", "1 minute", "plugin_handoffs_code_hash_check"},
		{hash(), "View", "1 minute", "plugin_handoffs_purpose_check"},
		{hash(), "view", "0 seconds", "plugin_handoffs_expiry_check"},
	} {
		if err := handoff(c.hash, c.purpose, c.ttl); err == nil || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("hand-off %q/%q/%s: %v; want refused by %s", c.hash, c.purpose, c.ttl, err, c.constraint)
		}
	}
	if err := handoff(strings.Repeat("f", 64), "view", "1 minute"); err != nil {
		t.Errorf("a well-formed hand-off: %v; want stored", err)
	}

	if _, err := pool.Exec(ctx, readMigration(t, "0103_plugin_viewer_handoffs.sql")); err != nil {
		t.Fatalf("re-applying 0103: %v", err)
	}
	var handoffs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM plugin_handoffs`).Scan(&handoffs); err != nil || handoffs != 1 {
		t.Fatalf("after re-applying 0103: %d hand-offs (%v); want the one stored", handoffs, err)
	}
}
