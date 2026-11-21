package plugins

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// TestAServerRowCannotHoldMessages: plugins_server_caps refuses a server row
// with `messages` (so auto-start, which needs it, is unreachable for a server
// app), and the install itself offers app and tasks only; the install is
// ownerless, scope server, data scope none, and audited as its actor.
func TestAServerRowCannotHoldMessages(t *testing.T) {
	s, pool, _ := pgStore(t)
	sys := authz.System(context.Background())
	if _, err := pool.Exec(sys, `INSERT INTO plugins (owner_id, scope, name, label, token_hash, data_scope, capabilities, origin, entry_url,
		app_label, app_icon, installed_via) VALUES (NULL, 'server', 'srv-msgs', 'x', 'h-srv-msgs', 'none', '{app,messages}',
		'https://*.apps.example.net', 'https://m.apps.example.net/enter', 'Calls', 'video', 'cli')`); err == nil {
		t.Fatal("a server row with messages was stored")
	}
	pats, _ := ParseOrigins("https://*.apps.example.net")
	tok, p, err := s.InstallServerApp(sys, ServerAppIn{Name: "srv-ok", AppLabel: "Slate Hall", AppIcon: "video",
		Origin: "https://*.apps.example.net", EntryURL: "https://m.apps.example.net/enter", Origins: pats, Tasks: true}, users.Actor{Via: "cli"})
	if err != nil || !TokenShapeOK(tok) {
		t.Fatalf("install: %v", err)
	}
	if _, ok := p.PersonalOwner(); ok || !p.IsServer() || p.DataScope != ViewerOnlyDataScope || len(p.Capabilities) != 2 {
		t.Errorf("the installed row: %+v; want ownerless, server, data scope none, app and tasks", p)
	}
	var via string
	var by *int64
	if err := pool.QueryRow(sys, `SELECT installed_via, installed_by FROM plugins WHERE id = $1`, p.ID).Scan(&via, &by); err != nil || via != "cli" || by != nil {
		t.Errorf("installed_via %q installed_by %v (%v); want cli and NULL", via, by, err)
	}
	var audits int
	if err := pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = 'plugin.server.installed' AND object = $1 AND user_id IS NULL`, Object(p.ID)).Scan(&audits); err != nil || audits != 1 {
		t.Errorf("the CLI install's audit rows: %d (%v); want 1 with no user", audits, err)
	}
	got, err := s.Authenticate(sys, tok)
	if err != nil || got.ID != p.ID || !got.IsServer() {
		t.Errorf("Authenticate the server token: %+v %v", got, err)
	}
	if _, err := s.UninstallServerApp(sys, p.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(sys, tok); err != ErrBadToken {
		t.Errorf("Authenticate after the uninstall: %v; want ErrBadToken", err)
	}
	if _, err := s.SetServerAppHidden(session(2), p.ID, true, time.Now()); err != ErrNotFound {
		t.Errorf("hiding an uninstalled app: %v; want ErrNotFound", err)
	}
}

// TestAtMostFiveServerApps: the sixth live install is refused; an uninstall
// frees a slot.
func TestAtMostFiveServerApps(t *testing.T) {
	s, _, _ := pgStore(t)
	sys := authz.System(context.Background())
	pats, _ := ParseOrigins("https://*.apps.example.net")
	install := func(name string) (Plugin, error) {
		_, p, err := s.InstallServerApp(sys, ServerAppIn{Name: name, AppLabel: "Slate Hall", AppIcon: "video",
			Origin: "https://*.apps.example.net", EntryURL: "https://m.apps.example.net/" + name, Origins: pats}, users.Actor{Via: "cli"})
		return p, err
	}
	var first Plugin
	for i, name := range []string{"srv-a", "srv-b", "srv-c", "srv-d", "srv-e"} {
		p, err := install(name)
		if err != nil {
			t.Fatalf("install %d: %v", i+1, err)
		}
		if i == 0 {
			first = p
		}
	}
	if _, err := install("srv-f"); err == nil {
		t.Error("a sixth live server app was installed")
	}
	if _, err := s.UninstallServerApp(sys, first.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := install("srv-f"); err != nil {
		t.Errorf("after an uninstall, the fifth: %v", err)
	}
}
