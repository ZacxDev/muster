package db_test

import (
	"strings"
	"testing"
)

// TestMigration0166PluginAppCapability: at 0165 plugins refuses `app` (the
// control); at 0166 an `app` row needs an origin, an entry URL, a tab label
// and an icon, and only an `app` row may carry a label or icon. A viewer row
// from before 0166 is untouched and still valid, the label is 1..24
// characters and the icon a lower-case key. Re-applying the body is harmless,
// and the header's ROLLBACK block runs as written.
func TestMigration0166PluginAppCapability(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0166_plugin_app", 165)
	const cols = `(owner_id, name, label, token_hash, data_scope, capabilities, origin, entry_url`
	const o, e = `'https://*.apps.example.net'`, `'https://m.apps.example.net/enter'`
	if _, err := pool.Exec(ctx, `INSERT INTO plugins `+cols+`) VALUES (1, 'app-early', 'x', 'h-early', 'none', '{app}', `+o+`, `+e+`)`); err == nil {
		t.Fatal("control: before 0166 plugins accepted the app capability")
	}
	viewer := itoa(scanID(ctx, t, pool, `INSERT INTO plugins `+cols+`) VALUES (1, 'viewer-old', 'x', 'h-viewer', 'none', '{viewer}', `+o+`, `+e+`) RETURNING id`))
	applyThroughFrom(ctx, t, pool, 166)

	var label, icon *string
	if err := pool.QueryRow(ctx, `SELECT app_label, app_icon FROM plugins WHERE id = `+viewer).Scan(&label, &icon); err != nil || label != nil || icon != nil {
		t.Fatalf("the viewer row from before 0166: label %v icon %v (%v); want both NULL", label, icon, err)
	}
	n := 0
	insert := func(caps, origin, entry, label, icon string) error {
		n++
		_, err := pool.Exec(ctx, `INSERT INTO plugins `+cols+`, app_label, app_icon) VALUES (1, 'app-'||$1::text, 'x', 'h-app-'||$1::text, 'none', `+
			caps+`, `+origin+`, `+entry+`, `+label+`, `+icon+`)`, strings.Repeat("x", n))
		return err
	}
	for why, c := range map[string][5]string{
		"app without an origin or entry URL": {`'{app}'`, "NULL", "NULL", `'Calls'`, `'video'`},
		"app with an origin, no entry URL":   {`'{app}'`, o, "NULL", `'Calls'`, `'video'`},
		"app without a label or icon":        {`'{app}'`, o, e, "NULL", "NULL"},
		"app without an icon":                {`'{app}'`, o, e, `'Calls'`, "NULL"},
		"app without a label":                {`'{app}'`, o, e, "NULL", `'video'`},
		"a label on a viewer row":            {`'{viewer}'`, o, e, `'Calls'`, `'video'`},
		"a label on a messages row":          {`'{messages}'`, "NULL", "NULL", `'Calls'`, `'video'`},
		"a 25-character label":               {`'{app}'`, o, e, `repeat('m', 25)`, `'video'`},
		"an empty label":                     {`'{app}'`, o, e, `''`, `'video'`},
		"an emoji for an icon":               {`'{app}'`, o, e, `'Calls'`, `'📹'`},
		"an origin on a messages row":        {`'{messages}'`, o, e, "NULL", "NULL"},
		"an unknown capability":              {`'{apps}'`, o, e, "NULL", "NULL"},
	} {
		if err := insert(c[0], c[1], c[2], c[3], c[4]); err == nil {
			t.Errorf("accepted %s", why)
		}
	}
	for why, c := range map[string][5]string{
		"app with everything":      {`'{app}'`, o, e, `repeat('m', 24)`, `'video'`},
		"messages, viewer and app": {`'{messages,viewer,app}'`, o, e, `'Calls'`, `'clipboard'`},
		"a viewer row, no tab":     {`'{viewer}'`, o, e, "NULL", "NULL"},
	} {
		if err := insert(c[0], c[1], c[2], c[3], c[4]); err != nil {
			t.Errorf("refused %s: %v", why, err)
		}
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0166_plugin_app_capability.sql")); err != nil {
		t.Fatalf("re-applying 0166: %v", err)
	}
	appOnly := itoa(scanID(ctx, t, pool, `SELECT id FROM plugins WHERE capabilities = '{app}' LIMIT 1`))
	for _, q := range asksRollbackSQL(t, "0166_plugin_app_capability.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	var caps string
	var revoked, hasOrigin bool
	if err := pool.QueryRow(ctx, `SELECT capabilities::text, revoked_at IS NOT NULL, origin IS NOT NULL FROM plugins WHERE id = `+appOnly).Scan(&caps, &revoked, &hasOrigin); err != nil {
		t.Fatal(err)
	}
	if caps != "{messages}" || !revoked || hasOrigin {
		t.Errorf("after the rollback the app-only plugin: caps %s revoked %v origin %v; want {messages}, revoked, no origin", caps, revoked, hasOrigin)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO plugins `+cols+`) VALUES (1, 'app-late', 'x', 'h-late', 'none', '{app}', `+o+`, `+e+`)`); err == nil {
		t.Error("after the rollback plugins still accepts app")
	}
}
