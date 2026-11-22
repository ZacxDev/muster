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

// TestTheAppCommandLinksAndUnlinks runs `muster-server app link|unlink` and
// the list's "linked with" column against Postgres: link prints one line,
// "linked <adopter> (app <id>) to <source> (app <id>): <n> people already
// known by <source>", and exits 0; list then names each side of the pair; a
// second link of the same apps exits 1 with the store's words; unlink prints
// the partner and exits 0, and says what it does not undo; a second unlink
// exits 1; a missing or non-numeric id is the usage's 2. Each of link and
// unlink leaves one audit row with no user, via cli.
func TestTheAppCommandLinksAndUnlinks(t *testing.T) {
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
	src, ado := "cli-src-"+stamp, "cli-ado-"+stamp
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM audit_log WHERE object IN (SELECT 'plugin:' || id FROM plugins WHERE name = ANY ($1))`, []string{src, ado})
		_, _ = pool.Exec(bg, `DELETE FROM plugins WHERE name = ANY ($1)`, []string{src, ado})
	})
	env := map[string]string{envDatabase: dsn, envPluginOrigins: "https://*.apps.example.test"}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		rc := runAccountCommand(ctx, append([]string{"app"}, args...), func(k string) string { return env[k] }, &out, &errb)
		return rc, out.String(), errb.String()
	}
	ids := map[string]string{}
	for _, n := range []string{src, ado} {
		if rc, out, errs := run("install", "--name", n, "--label", "Ledger Hall", "--icon", "book",
			"--origin", "https://*.apps.example.test", "--entry", "https://m.apps.example.test/"+n); rc != 0 {
			t.Fatalf("install %s: rc=%d %q %q", n, rc, out, errs)
		}
		var id int64
		if err := pool.QueryRow(sys, `SELECT id FROM plugins WHERE name = $1 AND revoked_at IS NULL`, n).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[n] = strconv.FormatInt(id, 10)
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
	if got := lastCol(listLine(src)); got != "-" {
		t.Errorf("list before the link: the linked-with column is %q; want -", got)
	}

	rc, out, errs := run("link", ids[src], ids[ado])
	want := "linked " + ado + " (app " + ids[ado] + ") to " + src + " (app " + ids[src] + "): 0 people already known by " + src + "\n"
	if rc != 0 || out != want || !strings.Contains(errs, "Nothing already stored was changed") {
		t.Fatalf("link: rc=%d stdout %q stderr %q; want 0 and %q", rc, out, errs, want)
	}
	if got := lastCol(listLine(src)); got != "linked with "+ids[ado]+" (source)" {
		t.Errorf("list: the source's linked-with column is %q", got)
	}
	if got := lastCol(listLine(ado)); got != "linked with "+ids[src]+" (adopter)" {
		t.Errorf("list: the adopter's linked-with column is %q", got)
	}
	if rc, _, errs := run("link", ids[src], ids[ado]); rc != 1 || !strings.Contains(errs, "app "+ids[src]+" is already linked with app "+ids[ado]+"; unlink it first") {
		t.Errorf("a second link: rc=%d %q; want 1 and the already-linked refusal", rc, errs)
	}
	for _, bad := range [][]string{{"link", ids[src]}, {"link", ids[src], "x"}, {"link", "0", ids[ado]}, {"unlink"}, {"unlink", "x"}} {
		if rc, _, _ := run(bad...); rc != 2 {
			t.Errorf("%v: rc=%d; want the usage's 2", bad, rc)
		}
	}

	rc, out, errs = run("unlink", ids[ado])
	if rc != 0 || out != "unlinked app "+ids[ado]+" from app "+ids[src]+".\n" || !strings.Contains(errs, "keep the same subject under both") {
		t.Fatalf("unlink: rc=%d stdout %q stderr %q", rc, out, errs)
	}
	if got := lastCol(listLine(ado)); got != "-" {
		t.Errorf("list after the unlink: the linked-with column is %q; want -", got)
	}
	if rc, _, errs := run("unlink", ids[ado]); rc != 1 || !strings.Contains(errs, "app "+ids[ado]+" is not linked") {
		t.Errorf("a second unlink: rc=%d %q; want 1 and \"is not linked\"", rc, errs)
	}
	for action, object := range map[string]string{"plugin.server.linked": "plugin:" + ids[ado], "plugin.server.unlinked": "plugin:" + ids[ado]} {
		var n int
		if err := pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = $1 AND object = $2 AND user_id IS NULL AND via = 'cli'`, action, object).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s on %s: %d rows (%v); want 1 with no user, via cli", action, object, n, err)
		}
	}
}
