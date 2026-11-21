package db_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
)

// the0172Constraints is every constraint 0172 adds, by table.
var the0172Constraints = map[string][]string{
	"plugins":         {"plugins_scope_check", "plugins_scope_owner", "plugins_installed_via_check", "plugins_server_installer", "plugins_server_caps"},
	"plugin_handoffs": {"plugin_handoffs_one_plugin", "plugin_handoffs_server_signin"},
}

const (
	srvOrigin = `'https://*.apps.example.net'`
	srvEntry  = `'https://m.apps.example.net/enter'`
)

// TestMigration0172ServerScope: at 0171 plugins refuses a NULL owner (the
// control). At 0172 a server row is ownerless, holds app and at most tasks,
// stores data_scope none and its installer's way; a person row still needs an
// owner; a code names exactly one of a personal plugin and a server app, and a
// server code its sign-in time. The body re-applies cleanly, the header's
// ROLLBACK block runs as written, and the file applies again after it.
func TestMigration0172ServerScope(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0172_server_scope", 171)
	if _, err := pool.Exec(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope) VALUES (NULL, 'early', 'x', 'h-early', 'none')`); err == nil {
		t.Fatal("control: before 0172 plugins accepted a NULL owner")
	}
	applyThroughFrom(ctx, t, pool, 172)
	const cols = `(owner_id, scope, name, label, token_hash, data_scope, capabilities, origin, entry_url, app_label, app_icon, installed_via, installed_by)`
	n := 0
	insert := func(owner, scope, caps, dataScope, via, by string) error {
		n++
		_, err := pool.Exec(ctx, `INSERT INTO plugins `+cols+` VALUES (`+owner+`, `+scope+`, 'srv-'||$1::text, 'x', 'h-srv-'||$1::text, `+dataScope+`, `+
			caps+`, `+srvOrigin+`, `+srvEntry+`, 'Calls', 'video', `+via+`, `+by+`)`, strings.Repeat("x", n))
		return err
	}
	for why, c := range map[string][6]string{
		"a server row with an owner":         {"1", `'server'`, `'{app}'`, `'none'`, `'page'`, "1"},
		"a person row with no owner":         {"NULL", `'person'`, `'{app}'`, `'none'`, "NULL", "NULL"},
		"a server row with messages":         {"NULL", `'server'`, `'{app,messages}'`, `'none'`, `'cli'`, "NULL"},
		"a server row with viewer":           {"NULL", `'server'`, `'{app,viewer}'`, `'none'`, `'cli'`, "NULL"},
		"a server row without app":           {"NULL", `'server'`, `'{tasks}'`, `'none'`, `'cli'`, "NULL"},
		"a server row with a data scope":     {"NULL", `'server'`, `'{app}'`, `'example'`, `'cli'`, "NULL"},
		"a server row with no installed_via": {"NULL", `'server'`, `'{app}'`, `'none'`, "NULL", "NULL"},
		"a person row with an installed_via": {"1", `'person'`, `'{app}'`, `'none'`, `'page'`, "NULL"},
		"a person row with an installed_by":  {"1", `'person'`, `'{app}'`, `'none'`, "NULL", "1"},
		"an unknown scope":                   {"NULL", `'team'`, `'{app}'`, `'none'`, `'cli'`, "NULL"},
		"an unknown installed_via":           {"NULL", `'server'`, `'{app}'`, `'none'`, `'api'`, "NULL"},
	} {
		if err := insert(c[0], c[1], c[2], c[3], c[4], c[5]); err == nil {
			t.Errorf("accepted %s", why)
		}
	}
	for why, c := range map[string][6]string{
		"a server app from the page":    {"NULL", `'server'`, `'{app,tasks}'`, `'none'`, `'page'`, "1"},
		"a server app from the command": {"NULL", `'server'`, `'{app}'`, `'none'`, `'cli'`, "NULL"},
		"a person's app plugin":         {"1", `'person'`, `'{app}'`, `'none'`, "NULL", "NULL"},
	} {
		if err := insert(c[0], c[1], c[2], c[3], c[4], c[5]); err != nil {
			t.Errorf("refused %s: %v", why, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO plugins `+cols+` VALUES (NULL, 'server', (SELECT name FROM plugins WHERE scope = 'server' LIMIT 1), 'x', 'h-dup', 'none', '{app}', `+srvOrigin+`, `+srvEntry+`, 'Calls', 'video', 'cli', NULL)`); err == nil {
		t.Error("accepted a second live server app with one name (plugins_server_name_live)")
	}
	server := itoa(scanID(ctx, t, pool, `SELECT id FROM plugins WHERE scope = 'server' LIMIT 1`))
	person := itoa(scanID(ctx, t, pool, `SELECT id FROM plugins WHERE scope = 'person' LIMIT 1`))
	code := func(plugin, srv, signin string) error {
		n++
		_, err := pool.Exec(ctx, `INSERT INTO plugin_handoffs (owner_id, plugin_id, server_plugin_id, code_hash, target, purpose, stepped_up, minted_at, expires_at, signed_in_at)
			VALUES (1, `+plugin+`, `+srv+`, md5($1::text) || md5($1::text), '{"app":{}}', 'app', false, now(), now() + interval '1 minute', `+signin+`)`, strings.Repeat("c", n))
		return err
	}
	for why, c := range map[string][3]string{
		"a code naming both":            {person, server, "now()"},
		"a code naming neither":         {"NULL", "NULL", "now()"},
		"a server code with no sign-in": {"NULL", server, "NULL"},
	} {
		if err := code(c[0], c[1], c[2]); err == nil {
			t.Errorf("accepted %s", why)
		}
	}
	for why, c := range map[string][3]string{
		"a personal code":              {person, "NULL", "NULL"},
		"a server code with a sign-in": {"NULL", server, "now()"},
	} {
		if err := code(c[0], c[1], c[2]); err != nil {
			t.Errorf("refused %s: %v", why, err)
		}
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0172_plugin_server_scope.sql")); err != nil {
		t.Fatalf("re-applying 0172: %v", err)
	}
	for _, q := range asksRollbackSQL(t, "0172_plugin_server_scope.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
		AND ((table_name = 'plugins' AND column_name IN ('scope', 'installed_by', 'installed_via'))
		  OR (table_name = 'plugin_handoffs' AND column_name IN ('server_plugin_id', 'signed_in_at')))`).Scan(&left); err != nil || left != 0 {
		t.Errorf("after the rollback %d of 0172's columns remain (%v)", left, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope) VALUES (NULL, 'late', 'x', 'h-late', 'none')`); err == nil {
		t.Error("after the rollback plugins still accepts a NULL owner")
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0172_plugin_server_scope.sql")); err != nil {
		t.Fatalf("0172 after its rollback: %v", err)
	}
}

// TestServerAppConstraintsSurviveRLSCycle: with a server row, a server code
// and a grant (and a ledgered task) present, db.RLSOff then db.RLSOn — which
// re-apply 0015's, 0104's, 0141's and 0172's bodies — leaves every 0172
// constraint present and its definition unchanged, and the rows intact.
// Red if 0172's constraints are written unguarded and a re-applied body
// narrows one, or an older body re-adds a NOT NULL a server row breaks.
func TestServerAppConstraintsSurviveRLSCycle(t *testing.T) {
	f := newRLSFixture(t, "ms_0172_rls_cycle")
	ctx := f.mig
	server := itoa(scanID(ctx, t, f.pool, `INSERT INTO plugins (owner_id, scope, name, label, token_hash, data_scope, capabilities, origin, entry_url,
		app_label, app_icon, installed_via) VALUES (NULL, 'server', 'srv-cycle', 'x', 'h-cycle', 'none', '{app,tasks}', `+srvOrigin+`, `+srvEntry+`,
		'Calls', 'video', 'cli') RETURNING id`))
	if _, err := f.pool.Exec(ctx, `INSERT INTO plugin_handoffs (owner_id, server_plugin_id, code_hash, target, purpose, stepped_up, minted_at, expires_at, signed_in_at)
		VALUES (1, `+server+`, repeat('a', 64), '{"app":{}}', 'app', false, now(), now() + interval '1 minute', now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO plugin_server_users (plugin_id, owner_id, subject, signin_at, granted_at, act_until)
		VALUES (`+server+`, 1, 'AAECAwQFBgcICQoLDA0ODw', now(), now(), now() + interval '8 hours')`); err != nil {
		t.Fatal(err)
	}
	note := itoa(scanID(ctx, t, f.pool, `INSERT INTO notes (owner_id, title) VALUES (1, 'cycle') RETURNING id`))
	if _, err := f.pool.Exec(ctx, `INSERT INTO plugin_server_tasks (plugin_id, owner_id, key, task_id) VALUES (`+server+`, 1, 'meetings:cycle', `+note+`)`); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]string {
		out := map[string]string{}
		for table, names := range the0172Constraints {
			for _, name := range names {
				var def string
				_ = f.pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1 AND conrelid = ('public.' || $2)::regclass`,
					name, table).Scan(&def)
				out[name] = def
			}
		}
		var nullable string
		_ = f.pool.QueryRow(ctx, `SELECT string_agg(table_name || '.' || column_name || '=' || is_nullable, ',' ORDER BY table_name)
			FROM information_schema.columns WHERE table_schema = 'public'
			 AND ((table_name = 'plugins' AND column_name = 'owner_id') OR (table_name = 'plugin_handoffs' AND column_name = 'plugin_id'))`).Scan(&nullable)
		out["nullability"] = nullable
		return out
	}
	before := read()
	for name, def := range before {
		if def == "" {
			t.Fatalf("control: %s is absent before the cycle", name)
		}
	}
	if err := db.RLSOff(ctx, f.pool); err != nil {
		t.Fatalf("RLSOff: %v", err)
	}
	if err := db.RLSOn(ctx, f.pool); err != nil {
		t.Fatalf("RLSOn with a server row, code and grant present: %v", err)
	}
	after := read()
	for name, def := range before {
		if after[name] != def {
			t.Errorf("after RLSOff and RLSOn %s is %q; it was %q", name, after[name], def)
		}
	}
	var rows int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM plugin_server_users WHERE plugin_id = `+server+`)
		+ (SELECT count(*) FROM plugin_server_tasks WHERE plugin_id = `+server+`)
		+ (SELECT count(*) FROM plugin_handoffs WHERE server_plugin_id = `+server+`)`).Scan(&rows); err != nil || rows != 3 {
		t.Errorf("the server app's rows after the cycle: %d (%v); want 3", rows, err)
	}
}

// TestAServerRowIsInvisibleToATeamMembersPrincipal (an INVARIANT guard: no
// policy admits a NULL owner today, so it pins the property rather than a
// regression): a person whose principal CARRIES team memberships — so 0127's
// team_read arm, owner_id = ANY (muster_team_ids(...)), is exercised, not only
// the muster.uid arm — reads no server row, and none of another member's
// plugin_server_users or plugin_server_tasks rows. The positive control: the
// same principal reads its own grant row.
func TestAServerRowIsInvisibleToATeamMembersPrincipal(t *testing.T) {
	f := newRLSFixture(t, "ms_0172_null_owner")
	ctx := f.mig
	team := scanID(ctx, t, f.pool, `INSERT INTO users (role, display_name, kind) VALUES ('member', 'Null Owner Team', 'team') RETURNING id`)
	other := scanID(ctx, t, f.pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	for _, m := range []int64{f.two, other} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES ($1, $2, 'operator', 1)`, team, m); err != nil {
			t.Fatal(err)
		}
	}
	server := itoa(scanID(ctx, t, f.pool, `INSERT INTO plugins (owner_id, scope, name, label, token_hash, data_scope, capabilities, origin, entry_url,
		app_label, app_icon, installed_via) VALUES (NULL, 'server', 'srv-null', 'x', 'h-null', 'none', '{app}', `+srvOrigin+`, `+srvEntry+`,
		'Calls', 'video', 'cli') RETURNING id`))
	note := itoa(scanID(ctx, t, f.pool, `INSERT INTO notes (owner_id, title) VALUES ($1, 'theirs') RETURNING id`, other))
	for _, q := range []string{
		`INSERT INTO plugin_server_users (plugin_id, owner_id, subject, signin_at, act_until) VALUES (` + server + `, $1, 'b3RoZXItbWVtYmVyLXN1Yg', now(), now() + interval '1 hour')`,
		`INSERT INTO plugin_server_tasks (plugin_id, owner_id, key, task_id) VALUES (` + server + `, $1, 'meetings:theirs', ` + note + `)`,
	} {
		if _, err := f.pool.Exec(ctx, q, other); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO plugin_server_users (plugin_id, owner_id, subject, signin_at, act_until) VALUES (`+server+`, $1, 'bWluZS1vd24tc3ViamVjdA', now(), now() + interval '1 hour')`, f.two); err != nil {
		t.Fatal(err)
	}
	teams, err := authz.NewMemberships([]authz.Membership{{Team: team, Role: authz.TeamOperator}})
	if err != nil {
		t.Fatal(err)
	}
	me := authz.WithPrincipal(f.base, authz.Principal{UserID: f.two, Role: authz.RoleMember, Via: authz.ViaSession, Teams: teams})
	count := func(q string) int {
		var n int
		if err := f.pool.QueryRow(me, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM plugins WHERE id = ` + server); n != 0 {
		t.Errorf("a team member's principal reads the server row (%d)", n)
	}
	if n := count(`SELECT count(*) FROM plugin_server_users WHERE owner_id <> ` + itoa(f.two)); n != 0 {
		t.Errorf("a team member reads %d of another member's grant rows", n)
	}
	if n := count(`SELECT count(*) FROM plugin_server_tasks`); n != 0 {
		t.Errorf("a team member reads %d of another member's task ledger rows", n)
	}
	if n := count(`SELECT count(*) FROM plugin_server_users WHERE owner_id = ` + itoa(f.two)); n != 1 {
		t.Errorf("CONTROL the member's own grant row: %d; want 1", n)
	}
}
