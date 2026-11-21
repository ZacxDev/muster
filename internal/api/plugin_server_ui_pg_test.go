package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// A SERVER APP'S SURFACES (core PR 1h.2; docs/design/meetings.md "The tab for
// every person", "Who installs, and where", "The Android path"): the tab and
// the page for every person, /admin/apps, Account → Apps.

// justSignedIn is a sign-in a moment ago: fresh for every step-up.
func justSignedIn() time.Time { return time.Now().Add(-2 * time.Second) }

// staleSignIn is a sign-in past the step-up window, still a live session.
func staleSignIn() time.Time { return time.Now().Add(-stepup.Window() - time.Minute) }

func (f *srvFix) get(uid int64, path string) *httptest.ResponseRecorder {
	return f.do("GET", path, "", f.browser(uid, justSignedIn()))
}

// form posts a same-origin form as uid, signed in at signin.
func (f *srvFix) form(uid int64, signin time.Time, path string, v url.Values) *httptest.ResponseRecorder {
	return f.do("POST", path, v.Encode(), func(r *http.Request) {
		f.browser(uid, signin)(r)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	})
}

// tabIDs is uid's sidebar app tabs, in order, as plugin ids: every personal
// one, and the server apps THIS fixture installed (the package's tests share
// one database, and a server app is everyone's).
func (f *srvFix) tabIDs(uid int64) []int64 {
	var out []int64
	for _, p := range f.srv.liveAppPlugins(srvAs(uid)) {
		if !p.IsServer() || slices.Contains(f.appIDs, p.ID) {
			out = append(out, p.ID)
		}
	}
	return out
}

// TestTheTabGatesForAServerApp is the tab table with server rows: the
// all-open row draws exactly the server tab and the person's own, IN THAT
// ORDER; hidden, a team's principal, uninstalled and a retired origin each
// draw no server tab — and the page and its alive poll answer 404 exactly
// where no tab is drawn (one predicate, three consumers).
func TestTheTabGatesForAServerApp(t *testing.T) {
	f := newSrvFix(t)
	seam := &handoffSeam{t: t, srv: f.srv, h: f.h, hdr: f.hdr}
	_, mine, rec := seam.create(f.a, appForm("srv-tab-own-"+f.suffix, "https://w5.viewer.example.net/enter"))
	if mine == 0 {
		t.Fatalf("minting A's own app plugin: %d %s", rec.Code, rec.Body.String())
	}
	page := func(uid, id int64) (int, int) {
		p := f.get(uid, "/apps/"+strconv.FormatInt(id, 10))
		a := f.get(uid, "/ui/apps/"+strconv.FormatInt(id, 10)+"/alive")
		return p.Code, a.Code
	}
	if got := f.tabIDs(f.a); len(got) != 2 || got[0] != f.app.ID || got[1] != mine {
		t.Fatalf("all open: A's tabs %v; want [server %d, own %d]", got, f.app.ID, mine)
	}
	if got := f.tabIDs(f.b); len(got) != 1 || got[0] != f.app.ID {
		t.Errorf("all open: B's tabs %v; want the server app alone (B never did anything)", got)
	}
	if p, a := page(f.b, f.app.ID); p != http.StatusOK || a != http.StatusNoContent {
		t.Errorf("all open: B's page %d, poll %d; want 200 and 204", p, a)
	}
	team := f.person("Tab Team", "member", "team")
	if got := f.srv.liveAppPlugins(teamAs(team)); len(got) != 0 {
		t.Errorf("a team's acting principal draws %d app tabs; want none", len(got))
	}
	if _, err := f.store.SetServerAppHidden(srvAs(f.b), f.app.ID, true, f.now); err != nil {
		t.Fatal(err)
	}
	if got := f.tabIDs(f.b); len(got) != 0 {
		t.Errorf("hidden: B's tabs %v; want none", got)
	}
	if p, a := page(f.b, f.app.ID); p != http.StatusNotFound || a != http.StatusNotFound {
		t.Errorf("hidden: B's page %d, poll %d; want 404 and 404", p, a)
	}
	if got := f.tabIDs(f.a); len(got) != 2 {
		t.Errorf("B's hide changed A's tabs: %v", got)
	}
	other, _ := plugins.ParseOrigins("https://*.elsewhere.example.test")
	f.srv.ext.PluginOrigins = other
	if got := f.tabIDs(f.a); len(got) != 0 {
		t.Errorf("retired origin: A's tabs %v; want none", got)
	}
	f.srv.ext.PluginOrigins = f.pats
	if _, err := f.srv.uninstallServerApp(httptestRequest(), f.app.ID, users.Actor{UserID: f.admin, Via: authz.ViaSession}); err != nil {
		t.Fatal(err)
	}
	if got := f.tabIDs(f.a); len(got) != 1 || got[0] != mine {
		t.Errorf("uninstalled: A's tabs %v; want A's own alone", got)
	}
	if p, a := page(f.a, f.app.ID); p != http.StatusNotFound || a != http.StatusNotFound {
		t.Errorf("uninstalled: A's page %d, poll %d; want 404 and 404", p, a)
	}
}

// TestAServerAppsPageHasNoNewTabAndItsOwnCards: a server app's /apps/{id}
// renders no data-app-newtab button and no personal card, but marks itself
// data-app-server and carries the open-external and copy-link cards; a
// personal app's page still has New tab and its card (the positive control).
func TestAServerAppsPageHasNoNewTabAndItsOwnCards(t *testing.T) {
	f := newSrvFix(t)
	seam := &handoffSeam{t: t, srv: f.srv, h: f.h, hdr: f.hdr}
	_, mine, rec := seam.create(f.a, appForm("srv-page-own-"+f.suffix, "https://w6.viewer.example.net/enter"))
	if mine == 0 {
		t.Fatalf("minting A's own app plugin: %d %s", rec.Code, rec.Body.String())
	}
	srv := f.get(f.a, "/apps/"+strconv.FormatInt(f.app.ID, 10)).Body.String()
	own := f.get(f.a, "/apps/"+strconv.FormatInt(mine, 10)).Body.String()
	for _, want := range []string{"data-app-server", "data-app-card-external", "data-app-card-copy", "data-app-external", "data-app-copy",
		"Basalt Room opens in your browser", "Open this page in your browser to use Basalt Room:"} {
		if !strings.Contains(srv, want) {
			t.Errorf("the server app's page lacks %q", want)
		}
	}
	for _, not := range []string{"data-app-newtab", "data-app-open", `data-app-card=""`} {
		if strings.Contains(srv, not) {
			t.Errorf("the server app's page carries %q", not)
		}
	}
	for _, want := range []string{"data-app-newtab", "data-app-open"} {
		if !strings.Contains(own, want) {
			t.Errorf("CONTROL the personal app's page lacks %q", want)
		}
	}
	if strings.Contains(own, "data-app-server") {
		t.Errorf("the personal app's page is marked a server app's")
	}
}

var serverTokenRE = regexp.MustCompile(`id="minted-server-app-token"[^>]*value="(mpl_[A-Za-z0-9_-]+)"`)

func installForm(name string) url.Values {
	return url.Values{"name": {name}, "app_label": {"Quarry Hall"}, "app_icon": {"mic"}, "origin": {hoZone}, "entry_url": {"https://q.viewer.example.net/" + name}, "tasks": {"1"}}
}

// TestAdminAppsIsAnAdminsAndInstallAndRotateNeedAFreshSignIn: a member is
// refused; an admin's install with a stale sign-in is a 403 asking for a
// step-up and installs nothing; with a fresh one it installs, shows the token
// once (it authenticates as a server app) and lists the app; rotate is gated
// the same way and kills the old token; uninstall needs no fresh sign-in and
// removes the app; a sixth app is refused. The page lists the live-grant
// count, never a name.
func TestAdminAppsIsAnAdminsAndInstallAndRotateNeedAFreshSignIn(t *testing.T) {
	f := newSrvFix(t)
	name := "srv-admin-" + f.suffix
	if rec := f.get(f.a, adminAppsPath); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("a member's GET /admin/apps: %d; want refused", rec.Code)
	}
	if rec := f.form(f.a, justSignedIn(), adminAppsPath, installForm(name)); rec.Code < 400 {
		t.Errorf("a member's install: %d; want refused", rec.Code)
	}
	stale := f.form(f.admin, staleSignIn(), adminAppsPath, installForm(name))
	if stale.Code != http.StatusForbidden || stale.Header().Get("X-Muster-Step-Up") != "required" {
		t.Errorf("a stale install: %d step-up %q; want 403 required", stale.Code, stale.Header().Get("X-Muster-Step-Up"))
	}
	if n := f.count(`SELECT count(*) FROM plugins WHERE name = $1`, name); n != 0 {
		t.Fatalf("a stale install stored %d rows", n)
	}
	rec := f.form(f.admin, justSignedIn(), adminAppsPath, installForm(name))
	m := serverTokenRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("a fresh install: %d, token shown %v", rec.Code, m != nil)
	}
	var id int64
	if err := f.pool.QueryRow(f.sys(), `SELECT id FROM plugins WHERE name = $1 AND scope = 'server' AND installed_via = 'page' AND installed_by = $2`,
		name, f.admin).Scan(&id); err != nil {
		t.Fatalf("the installed row: %v", err)
	}
	f.appIDs = append(f.appIDs, id)
	if p, err := f.store.Authenticate(f.sys(), m[1]); err != nil || p.ID != id || !p.IsServer() {
		t.Fatalf("the shown token: %+v %v", p, err)
	}
	list := f.get(f.admin, adminAppsPath).Body.String()
	// The two gated forms confirm IN PLACE: each is marked data-step-up-form
	// and the page draws the confirm dialog (stepUpFormsHTML).
	for _, want := range []string{`action="/admin/apps" class="`, `/rotate" data-step-up-form`, "musterStepUpForms", "window.musterStepUp"} {
		if !strings.Contains(list, want) {
			t.Errorf("/admin/apps lacks %q (its gated forms cannot confirm in place)", want)
		}
	}
	if !regexp.MustCompile(`<form method="POST" action="/admin/apps" class="[^"]*" data-step-up-form`).MatchString(list) {
		t.Errorf("the install form is not marked data-step-up-form")
	}
	if !strings.Contains(list, `data-server-app="`+strconv.FormatInt(id, 10)+`"`) || strings.Contains(list, "minted-server-app-token") {
		t.Errorf("the list after the install: the app listed %v, the token shown again %v",
			strings.Contains(list, `data-server-app="`+strconv.FormatInt(id, 10)+`"`), strings.Contains(list, "minted-server-app-token"))
	}
	// The fixture's own app has one live grant (A opened it).
	f.open(f.a, f.app.ID, f.tok)
	if !strings.Contains(f.get(f.admin, adminAppsPath).Body.String(), `data-live-grants="1"`) {
		t.Errorf("the page does not count A's live grant")
	}
	if strings.Contains(f.get(f.admin, adminAppsPath).Body.String(), "Ada Quill") {
		t.Errorf("the page names a person who holds a grant")
	}
	rot := "/admin/apps/" + strconv.FormatInt(id, 10) + "/rotate"
	if rec := f.form(f.admin, staleSignIn(), rot, url.Values{}); rec.Code != http.StatusForbidden {
		t.Errorf("a stale rotate: %d; want 403", rec.Code)
	}
	rec = f.form(f.admin, justSignedIn(), rot, url.Values{})
	m2 := serverTokenRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m2 == nil || m2[1] == m[1] {
		t.Fatalf("a fresh rotate: %d, a new token %v", rec.Code, m2 != nil && m2[1] != m[1])
	}
	if _, err := f.store.Authenticate(f.sys(), m[1]); err != plugins.ErrBadToken {
		t.Errorf("the old token after a rotate: %v; want ErrBadToken", err)
	}
	// Fill the server to five, so the sixth is refused.
	for i := 0; ; i++ {
		var n int
		_ = f.pool.QueryRow(f.sys(), `SELECT count(*) FROM plugins WHERE scope = 'server' AND revoked_at IS NULL`).Scan(&n)
		if n >= plugins.MaxServerApps {
			break
		}
		f.install("srv-fill-"+strconv.Itoa(i), false)
	}
	if !strings.Contains(f.get(f.admin, adminAppsPath).Body.String(), `id="admin-apps-full"`) {
		t.Errorf("a full server still offers the install form")
	}
	if rec := f.form(f.admin, justSignedIn(), adminAppsPath, installForm(name+"-6")); rec.Code != http.StatusBadRequest {
		t.Errorf("a sixth install: %d; want 400", rec.Code)
	}
	// Uninstall: a stale sign-in is enough.
	if rec := f.form(f.admin, staleSignIn(), "/admin/apps/"+strconv.FormatInt(id, 10)+"/uninstall", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Errorf("an uninstall with a stale sign-in: %d; want 303", rec.Code)
	}
	if n := f.count(`SELECT count(*) FROM plugins WHERE id = $1 AND revoked_at IS NULL`, id); n != 0 {
		t.Errorf("the app is still live after the uninstall")
	}
}

// TestAdminAppsIsServedOnlyWithPluginOrigins: with MUSTER_PLUGIN_ORIGINS
// unset /admin/apps is the feature's 404 and the admin tab bar does not link
// it; set, it is linked.
func TestAdminAppsIsServedOnlyWithPluginOrigins(t *testing.T) {
	f := newSrvFix(t)
	if !strings.Contains(f.get(f.admin, adminAppsPath).Body.String(), `href="/admin/apps"`) {
		t.Errorf("CONTROL the admin tab bar does not link Apps")
	}
	f.srv.ext.PluginOrigins = nil
	if rec := f.get(f.admin, adminAppsPath); rec.Code != http.StatusNotFound {
		t.Errorf("/admin/apps with no plugin origins: %d; want 404", rec.Code)
	}
	if rec := f.get(f.admin, accountAppsPath); rec.Code != http.StatusNotFound {
		t.Errorf("/account/apps with no plugin origins: %d; want 404", rec.Code)
	}
}

// TestAccountAppsHideAndShow: Account → Apps lists the app with what it may
// do and A's live grant; Hide (a same-origin form, no fresh sign-in) removes
// the tab and ends the grant at once; Show brings the tab back and no grant;
// a cross-origin Hide is refused and changes nothing.
func TestAccountAppsHideAndShow(t *testing.T) {
	f := newSrvFix(t)
	sub := f.open(f.a, f.app.ID, f.tok)
	page := f.get(f.a, accountAppsPath).Body.String()
	for _, want := range []string{"Basalt Room", "can file tasks for you", "it learns your display name when you open it", "Can act for you until"} {
		if !strings.Contains(page, want) {
			t.Errorf("Account → Apps lacks %q", want)
		}
	}
	id := strconv.FormatInt(f.app.ID, 10)
	cross := f.do("POST", accountAppsPath+"/"+id+"/hide", "", func(r *http.Request) {
		f.browser(f.a, f.signin)(r)
		r.Header.Set("Origin", "https://evil.example.test")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if cross.Code < 400 {
		t.Errorf("a cross-origin hide: %d; want refused", cross.Code)
	}
	if len(f.tabIDs(f.a)) != 1 {
		t.Errorf("a refused hide changed A's tabs")
	}
	if rec := f.form(f.a, f.signin, accountAppsPath+"/"+id+"/hide", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("hide: %d %s", rec.Code, rec.Body.String())
	}
	if len(f.tabIDs(f.a)) != 0 {
		t.Errorf("after Hide A still has the tab")
	}
	if rec := f.knock(f.tok, sub, "acct-1"); rec.Code != http.StatusNotFound {
		t.Errorf("act-for after Hide: %d; want 404", rec.Code)
	}
	if !strings.Contains(f.get(f.a, accountAppsPath).Body.String(), `data-app-state="hidden"`) {
		t.Errorf("Account → Apps does not show the app hidden")
	}
	if rec := f.form(f.a, f.signin, accountAppsPath+"/"+id+"/show", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("show: %d", rec.Code)
	}
	if len(f.tabIDs(f.a)) != 1 {
		t.Errorf("after Show A has no tab")
	}
	if rec := f.knock(f.tok, sub, "acct-2"); rec.Code != http.StatusNotFound {
		t.Errorf("act-for after Show with no redeem: %d; want 404", rec.Code)
	}
	if strings.Contains(f.get(f.a, accountAppsPath).Body.String(), "Can act for you until") {
		t.Errorf("Account → Apps says the app may act for A after Show with no redeem")
	}
}
