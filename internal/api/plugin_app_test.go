package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// appListedPlugins is a plugin store whose List answers rows as they are,
// whoever asks: it lets each clause of liveAppPlugins be reached on its own,
// including rows the database's own CHECKs and owner filter would never hand
// back (a label without `app`, another owner's row).
type appListedPlugins struct {
	plugins.Store
	rows []plugins.Plugin
}

func (l appListedPlugins) List(context.Context) ([]plugins.Plugin, error) { return l.rows, nil }

func appRow(id, owner int64, mut func(*plugins.Plugin)) plugins.Plugin {
	label, icon, origin, entry := "Calls", "video", "https://*.meet.example.test", "https://w.meet.example.test/enter"
	p := plugins.Plugin{ID: id, Capabilities: []plugins.Capability{plugins.CapApp},
		AppLabel: &label, AppIcon: &icon, Origin: &origin, EntryURL: &entry}.OwnedBy(owner)
	if mut != nil {
		mut(&p)
	}
	return p
}

// TestLiveAppPluginsHoldsEveryClauseOnItsOwn: with one live app row as the
// positive control, each other row breaks exactly one clause — revoked, no
// `app` (but a label), another owner's, a team's, no label, an origin the
// current list no longer allows — and is not a tab; the result is oldest
// first (the store lists newest first).
func TestLiveAppPluginsHoldsEveryClauseOnItsOwn(t *testing.T) {
	at := time.Unix(1_900_000_000, 0)
	retired := "https://*.gone.example.test"
	rows := []plugins.Plugin{ // newest first, as the store lists
		appRow(9, 2, nil),
		appRow(8, 2, func(p *plugins.Plugin) { p.RevokedAt = &at }),
		appRow(7, 2, func(p *plugins.Plugin) { p.Capabilities = []plugins.Capability{plugins.CapViewer} }),
		appRow(6, 3, nil),
		appRow(5, 2, func(p *plugins.Plugin) { p.OwnerTeam = true }),
		appRow(4, 2, func(p *plugins.Plugin) { p.AppLabel = nil }),
		appRow(3, 2, func(p *plugins.Plugin) { p.Origin, p.EntryURL = &retired, strPtr("https://w.gone.example.test/enter") }),
		appRow(2, 2, nil),
	}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	pats, err := plugins.ParseOrigins("https://*.meet.example.test")
	if err != nil {
		t.Fatal(err)
	}
	s.UseExtensions(Extensions{Plugins: appListedPlugins{rows: rows}, PluginOrigins: pats})
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	var ids []string
	for _, p := range s.liveAppPlugins(ctx) {
		ids = append(ids, strconv.FormatInt(p.ID, 10))
	}
	if got := strings.Join(ids, ","); got != "2,9" {
		t.Fatalf("live app plugins %q; want 2,9 (oldest first, every other row breaking one clause)", got)
	}
	tabs := s.appTabs(ctx)
	if len(tabs) != 2 || tabs[0].Label != "Calls" || tabs[0].Icon != "📹" || tabs[0].ID != 2 {
		t.Errorf("app tabs %+v; want the two, labelled, with the video icon's emoji", tabs)
	}
}

// TestADisabledOwnersAppPageAndPollAreRefused is the tab gates' owner-disabled
// row, on a GoTrue session (the only door that can disable a signed-in
// owner): before the disable, the owner's page draws the app tab, /apps/{id}
// is 200 and the alive poll 204 (the positive control); after it, the page
// draws nothing (a document is sent to sign-in), /apps/{id} is not served and
// the alive poll is 401 — which the open page's script treats as removal
// (e2e/app-page.test.mjs).
func TestADisabledOwnersAppPageAndPollAreRefused(t *testing.T) {
	pats, err := plugins.ParseOrigins("https://*.meet.example.test")
	if err != nil {
		t.Fatal(err)
	}
	f := newGoTrueFixtureWith(t, &Extensions{Plugins: appListedPlugins{rows: []plugins.Plugin{appRow(4, 7, nil)}}, PluginOrigins: pats})
	sub := f.member("owner@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(sub)
	acct := do(f.edge, "GET", "/account", nil, withCookies(at, rt))
	page := do(f.edge, "GET", "/apps/4", nil, withCookies(at, rt), asNavigation)
	alive := do(f.edge, "GET", "/ui/apps/4/alive", nil, withCookies(at, rt), asXHR)
	if acct.Code != http.StatusOK || !strings.Contains(acct.Body.String(), `data-app-tab="4"`) ||
		page.Code != http.StatusOK || alive.Code != http.StatusNoContent {
		t.Fatalf("POSITIVE CONTROL: account %d (tab drawn %v), page %d, alive %d; want 200 with the tab, 200, 204",
			acct.Code, strings.Contains(acct.Body.String(), `data-app-tab="4"`), page.Code, alive.Code)
	}
	if err := f.s.DisableUser(context.Background(), 7, true); err != nil {
		t.Fatal(err)
	}
	acct = do(f.edge, "GET", "/account", nil, withCookies(at, rt), asNavigation)
	page = do(f.edge, "GET", "/apps/4", nil, withCookies(at, rt), asNavigation)
	alive = do(f.edge, "GET", "/ui/apps/4/alive", nil, withCookies(at, rt), asXHR)
	if strings.Contains(acct.Body.String(), "data-app-tab") {
		t.Errorf("a disabled owner's page draws the app tab")
	}
	if page.Code == http.StatusOK {
		t.Errorf("/apps/{id} for a disabled owner: 200; want it refused")
	}
	if alive.Code != http.StatusUnauthorized {
		t.Errorf("the alive poll for a disabled owner: %d; want 401 (the page script removes the frame on it)", alive.Code)
	}
}
