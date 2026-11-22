package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestTheAppCommandInstallsRotatesListsAndUninstalls runs `muster-server app`
// against Postgres: install prints exactly the token on stdout (and it
// authenticates as a server app, installed_via cli, installed_by NULL,
// audited with no user); list names it; rotate prints a NEW token and kills
// the old; uninstall revokes it; a bad origin, a missing MUSTER_PLUGIN_ORIGINS
// and an unknown id are each refused with a non-zero exit.
func TestTheAppCommandInstallsRotatesListsAndUninstalls(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	name := "cli-app-" + strings.ToLower(strings.ReplaceAll(time.Now().Format("150405.000"), ".", ""))
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM audit_log WHERE object IN (SELECT 'plugin:' || id FROM plugins WHERE name = $1)`, name)
		_, _ = pool.Exec(bg, `DELETE FROM plugins WHERE name = $1`, name)
	})
	env := map[string]string{envDatabase: dsn, envPluginOrigins: "https://*.apps.example.test"}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		rc := runAccountCommand(ctx, append([]string{"app"}, args...), func(k string) string { return env[k] }, &out, &errb)
		return rc, out.String(), errb.String()
	}
	install := []string{"install", "--name", name, "--label", "Ledger Hall", "--icon", "notes",
		"--origin", "https://*.apps.example.test", "--entry", "https://m.apps.example.test/" + name, "--tasks"}
	rc, out, errs := run(install...)
	tok := strings.TrimSpace(out)
	if rc != 0 || !plugins.TokenShapeOK(tok) || strings.Count(out, "\n") != 1 {
		t.Fatalf("install: rc=%d stdout %q stderr %q; want exactly the token on stdout", rc, out, errs)
	}
	st := plugins.NewPG(pool)
	p, err := st.Authenticate(sys, tok)
	if err != nil || !p.IsServer() || !p.Has(plugins.CapTasks) {
		t.Fatalf("the printed token: %+v %v", p, err)
	}
	var via string
	var by *int64
	var auditUser *int64
	if err := pool.QueryRow(sys, `SELECT installed_via, installed_by FROM plugins WHERE id = $1`, p.ID).Scan(&via, &by); err != nil || via != "cli" || by != nil {
		t.Errorf("installed_via %q installed_by %v (%v); want cli and NULL", via, by, err)
	}
	if err := pool.QueryRow(sys, `SELECT user_id FROM audit_log WHERE action = 'plugin.server.installed' AND object = $1`, plugins.Object(p.ID)).Scan(&auditUser); err != nil || auditUser != nil {
		t.Errorf("the install's audit row: user %v (%v); want one with no user", auditUser, err)
	}
	if rc, out, _ := run("list"); rc != 0 || !strings.Contains(out, name) || !strings.Contains(out, "Ledger Hall") || !strings.Contains(out, "0 live") {
		t.Errorf("list: rc=%d %q", rc, out)
	}
	_, listed, _ := run("list")
	id := ""
	for _, l := range strings.Split(listed, "\n") {
		if strings.Contains(l, name) {
			id = strings.Split(l, "\t")[0]
		}
	}
	rc, out, errs = run("rotate", id)
	tok2 := strings.TrimSpace(out)
	if rc != 0 || !plugins.TokenShapeOK(tok2) || tok2 == tok || !strings.Contains(errs, "ends until it is restarted") {
		t.Fatalf("rotate: rc=%d stdout %q stderr %q", rc, out, errs)
	}
	if _, err := st.Authenticate(sys, tok); err != plugins.ErrBadToken {
		t.Errorf("the old token after rotate: %v; want ErrBadToken", err)
	}
	if rc, _, errs := run("rotate", "999999999"); rc == 0 || !strings.Contains(errs, "no such installed app") {
		t.Errorf("rotate an unknown id: rc=%d %q", rc, errs)
	}
	if rc, out, errs := run("uninstall", id); rc != 0 || !strings.Contains(out, "uninstalled") || !strings.Contains(errs, "cannot withdraw") {
		t.Fatalf("uninstall: rc=%d %q %q", rc, out, errs)
	}
	if _, err := st.Authenticate(sys, tok2); err != plugins.ErrBadToken {
		t.Errorf("the token after uninstall: %v; want ErrBadToken", err)
	}
	bad := append([]string(nil), install...)
	bad[8] = "https://*.elsewhere.example.test"
	if rc, _, _ := run(bad...); rc == 0 {
		t.Error("an origin outside MUSTER_PLUGIN_ORIGINS installed")
	}
	delete(env, envPluginOrigins)
	if rc, _, errs := run("list"); rc == 0 || !strings.Contains(errs, envPluginOrigins) {
		t.Errorf("no MUSTER_PLUGIN_ORIGINS: rc=%d %q", rc, errs)
	}
	if rc, _, _ := run(); rc != 2 {
		t.Errorf("no subcommand: rc=%d; want the usage's 2", rc)
	}
}

// TestTheAppCommandLinksAtInstallAndUnlinks runs `muster-server app install
// --link-to`, `app unlink` and the list's "linked with" column against
// Postgres: an install --link-to prints exactly the token on stdout, says it
// linked on stderr, and list then names each app's partner; a second install
// --link-to the same app exits 1 with the store's words and installs NOTHING
// (no row by its name); unlink prints the partner, exits 0 and says what it
// does not undo; a second unlink exits 1; a non-numeric --link-to and a
// missing or non-numeric unlink id are the usage's 2. The install's audit row
// records linked_to; the unlink leaves one row with no user, via cli.
func TestTheAppCommandLinksAtInstallAndUnlinks(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	stamp := strings.ToLower(strings.ReplaceAll(time.Now().Format("150405.000"), ".", ""))
	src, ado, dup := "cli-src-"+stamp, "cli-ado-"+stamp, "cli-dup-"+stamp
	names := []string{src, ado, dup}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM audit_log WHERE object IN (SELECT 'plugin:' || id FROM plugins WHERE name = ANY ($1))`, names)
		_, _ = pool.Exec(bg, `DELETE FROM plugins WHERE name = ANY ($1)`, names)
	})
	env := map[string]string{envDatabase: dsn, envPluginOrigins: "https://*.apps.example.test"}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		rc := runAccountCommand(ctx, append([]string{"app"}, args...), func(k string) string { return env[k] }, &out, &errb)
		return rc, out.String(), errb.String()
	}
	install := func(name string, extra ...string) (int, string, string) {
		return run(append([]string{"install", "--name", name, "--label", "Ledger Hall", "--icon", "book",
			"--origin", "https://*.apps.example.test", "--entry", "https://m.apps.example.test/" + name}, extra...)...)
	}
	idOf := func(name string) string {
		t.Helper()
		var id int64
		if err := pool.QueryRow(sys, `SELECT id FROM plugins WHERE name = $1 AND revoked_at IS NULL`, name).Scan(&id); err != nil {
			t.Fatalf("id of %s: %v", name, err)
		}
		return strconv.FormatInt(id, 10)
	}
	listLine := func(name string) string {
		t.Helper()
		_, out, _ := run("list")
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Split(l, "\t"); len(f) > 1 && f[1] == name {
				return l
			}
		}
		t.Fatalf("list has no line for %s: %q", name, out)
		return ""
	}
	lastCol := func(l string) string { f := strings.Split(l, "\t"); return f[len(f)-1] }

	if rc, out, errs := install(src); rc != 0 {
		t.Fatalf("install %s: rc=%d %q %q", src, rc, out, errs)
	}
	srcID := idOf(src)
	if got := lastCol(listLine(src)); got != "-" {
		t.Errorf("list before any link: the linked-with column is %q; want -", got)
	}
	rc, out, errs := install(ado, "--link-to", srcID)
	if tok := strings.TrimSpace(out); rc != 0 || !plugins.TokenShapeOK(tok) || strings.Count(out, "\n") != 1 ||
		!strings.Contains(errs, "Linked with app "+srcID+": the two know each person by one subject") {
		t.Fatalf("install --link-to: rc=%d stdout %q stderr %q; want exactly the token, and the link said", rc, out, errs)
	}
	adoID := idOf(ado)
	if got := lastCol(listLine(src)); got != "linked with "+adoID {
		t.Errorf("list: the source's linked-with column is %q; want linked with %s", got, adoID)
	}
	if got := lastCol(listLine(ado)); got != "linked with "+srcID {
		t.Errorf("list: the new app's linked-with column is %q; want linked with %s", got, srcID)
	}
	var linkedTo string
	if err := pool.QueryRow(sys, `SELECT coalesce(detail->>'linked_to', '') FROM audit_log WHERE action = 'plugin.server.installed' AND object = $1`,
		"plugin:"+adoID).Scan(&linkedTo); err != nil || linkedTo != srcID {
		t.Errorf("the install's audit row: linked_to %q (%v); want %s", linkedTo, err, srcID)
	}
	if rc, out, errs := install(dup, "--link-to", srcID); rc != 1 || out != "" ||
		!strings.Contains(errs, "app "+srcID+" is already linked with app "+adoID+"; unlink it first") {
		t.Errorf("install --link-to an app already paired: rc=%d stdout %q stderr %q; want 1, no token, the refusal", rc, out, errs)
	}
	var n int
	if err := pool.QueryRow(sys, `SELECT count(*) FROM plugins WHERE name = $1`, dup).Scan(&n); err != nil || n != 0 {
		t.Errorf("install --link-to is atomic: the refused install left %d row(s) (%v)", n, err)
	}
	for _, bad := range [][]string{{"unlink"}, {"unlink", "x"}} {
		if rc, _, _ := run(bad...); rc != 2 {
			t.Errorf("%v: rc=%d; want the usage's 2", bad, rc)
		}
	}
	if rc, _, _ := install(dup, "--link-to", "x"); rc != 2 {
		t.Errorf("--link-to x: rc=%d; want the usage's 2", rc)
	}

	rc, out, errs = run("unlink", adoID)
	if rc != 0 || out != "unlinked app "+adoID+" from app "+srcID+".\n" || !strings.Contains(errs, "keep the same subject under both") {
		t.Fatalf("unlink: rc=%d stdout %q stderr %q", rc, out, errs)
	}
	if a, b := lastCol(listLine(ado)), lastCol(listLine(src)); a != "-" || b != "-" {
		t.Errorf("list after the unlink: %q and %q; want - for both", a, b)
	}
	if rc, _, errs := run("unlink", adoID); rc != 1 || !strings.Contains(errs, "app "+adoID+" is not linked") {
		t.Errorf("a second unlink: rc=%d %q; want 1 and \"is not linked\"", rc, errs)
	}
	if err := pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = 'plugin.server.unlinked' AND object = $1 AND user_id IS NULL AND via = 'cli'`,
		"plugin:"+adoID).Scan(&n); err != nil || n != 1 {
		t.Errorf("plugin.server.unlinked: %d rows (%v); want 1 with no user, via cli", n, err)
	}
}
