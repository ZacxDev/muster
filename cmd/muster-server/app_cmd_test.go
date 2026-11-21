package main

import (
	"bytes"
	"context"
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
