package api

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/plugins"
)

// The app tab (meetings PR 1b): the hand-off's app target, the sidebar entry
// and its gates, /apps/{id}, its headers and frame, and the alive poll. The
// fixture's origin PATTERN is hoZone (https://*.viewer.example.net) and its
// entry URL's exact origin is appTestOrigin — deliberately different strings,
// so a header naming the pattern cannot pass for one naming the origin.

const appTestOrigin = "https://w.viewer.example.net"

func appMintBody(plugin int64, target string) string {
	return `{"plugin":` + strconv.FormatInt(plugin, 10) + `,"target":` + target + `}`
}

// mustApp mints an app plugin for uid through the /plugins form.
func (f *handoffSeam) mustApp(uid int64, name string, caps ...string) (string, int64) {
	f.t.Helper()
	tok, id, rec := f.create(uid, appForm(name, hoEntry, caps...))
	if tok == "" {
		f.t.Fatalf("minting app plugin %s: %d %s", name, rec.Code, rec.Body.String())
	}
	return tok, id
}

// TestAnAppHandoffTakesItsPurposeFromTheTarget: a mint with {"app": {}} whose
// body also names "purpose": "view" stores `app`: the redeem answers the
// pinned whole body with purpose app and target {"app":{}}, and the metric
// counts it under purpose app.
func TestAnAppHandoffTakesItsPurposeFromTheTarget(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	tok, id := f.mustApp(f.a, "app-purpose")
	minted := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("app", "minted"))
	redeemed := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("app", "redeemed"))
	rec, link := f.mint(f.a, false, `{"plugin":`+strconv.FormatInt(id, 10)+`,"purpose":"view","target":{"app":{}}}`)
	if rec.Code != http.StatusOK || !strings.HasPrefix(link, hoEntry+"?") {
		t.Fatalf("app mint: %d %s; want 200 with the entry URL", rec.Code, rec.Body.String())
	}
	rec = f.redeem(tok, codeOf(t, link))
	want := `{"owner_id":` + strconv.FormatInt(f.a, 10) + `,"target":{"app":{}},"purpose":"app","stepped_up":true,"minted_at":"2030-01-02T03:04:05Z"}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("redeem: %d\n got %s\nwant %s", rec.Code, strings.TrimSpace(rec.Body.String()), want)
	}
	if d := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("app", "minted")) - minted; d != 1 {
		t.Errorf("muster_plugin_handoffs_total{purpose=app,minted} moved by %v; want 1", d)
	}
	if d := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("app", "redeemed")) - redeemed; d != 1 {
		t.Errorf("muster_plugin_handoffs_total{purpose=app,redeemed} moved by %v; want 1", d)
	}
}

// TestAHandoffTargetNeedsItsOwnCapability: an app target for a viewer-only
// plugin and a mirror target for an app-only plugin are both refused 404 and
// mint nothing; a body with both targets, or neither, is 400. An app-only
// token redeems its own app code and gets the one 404 for another app
// plugin's; a viewer-only token still redeems its mirror code (the control
// that the redeem door admits both).
func TestAHandoffTargetNeedsItsOwnCapability(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	appTok, appID := f.mustApp(f.a, "app-only")
	app2Tok, _ := f.mustApp(f.a, "app-two")
	viewTok, viewID, _ := f.create(f.a, viewerForm("viewer-only", hoEntry))
	if viewTok == "" {
		t.Fatal("instrument: the viewer plugin was not minted")
	}
	for name, body := range map[string]string{
		"an app target for a viewer-only plugin": appMintBody(viewID, `{"app":{}}`),
		"a mirror target for an app-only plugin": mintBody(appID, "ws-a", "@3"),
	} {
		if rec, link := f.mint(f.a, false, body); rec.Code != http.StatusNotFound || link != "" ||
			!strings.Contains(rec.Body.String(), PluginHandoffNoPlugin) {
			t.Errorf("%s: %d %s; want 404 %q and no URL", name, rec.Code, rec.Body.String(), PluginHandoffNoPlugin)
		}
	}
	for name, body := range map[string]string{
		"both targets": appMintBody(appID, `{"app":{},"mirror":{"host":"ws-a","window":"@3"}}`),
		"no target":    appMintBody(appID, `{}`),
	} {
		if rec, _ := f.mint(f.a, false, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", name, rec.Code, rec.Body.String())
		}
	}
	_, link := f.mint(f.a, false, appMintBody(appID, `{"app":{}}`))
	code := codeOf(t, link)
	if rec := f.redeem(app2Tok, code); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), PluginHandoffNotFound) {
		t.Errorf("another app plugin redeeming the code: %d %s; want the one 404", rec.Code, rec.Body.String())
	}
	if rec := f.redeem(appTok, code); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"purpose":"app"`) {
		t.Errorf("an app-only token redeeming its own app code: %d %s; want 200 purpose app", rec.Code, rec.Body.String())
	}
	_, link = f.mint(f.a, false, mintBody(viewID, "ws-a", "@3"))
	if rec := f.redeem(viewTok, codeOf(t, link)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"purpose":"view"`) {
		t.Errorf("CONTROL: a viewer-only token redeeming its mirror code: %d %s; want 200 purpose view", rec.Code, rec.Body.String())
	}
}

// TestTheRefHintRidesTheAppTarget: /apps/{id}?ref=abc draws data-ref="abc"
// for the page's mint, and a mint with ref abc redeems to {"app":{"ref":
// "abc"}}; a ref with "/", "..", or 65 characters is dropped (no data-ref,
// and the redeemed target is {"app":{}}), never passed and never refused.
func TestTheRefHintRidesTheAppTarget(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	tok, id := f.mustApp(f.a, "app-ref")
	page := f.do("GET", "/apps/"+strconv.FormatInt(id, 10)+"?ref=abc", "", f.as(f.a, false)).Body.String()
	if !strings.Contains(page, `data-ref="abc"`) {
		t.Errorf("/apps/{id}?ref=abc does not hand the page's mint the ref")
	}
	_, link := f.mint(f.a, false, appMintBody(id, `{"app":{"ref":"abc"}}`))
	if rec := f.redeem(tok, codeOf(t, link)); !strings.Contains(rec.Body.String(), `"target":{"app":{"ref":"abc"}}`) {
		t.Errorf("a ref abc: redeemed %s; want target {\"app\":{\"ref\":\"abc\"}}", rec.Body.String())
	}
	for _, bad := range []string{"a/b", "..", strings.Repeat("r", 65)} {
		p := f.do("GET", "/apps/"+strconv.FormatInt(id, 10)+"?ref="+url.QueryEscape(bad), "", f.as(f.a, false))
		if p.Code != http.StatusOK || strings.Contains(p.Body.String(), "data-ref") {
			t.Errorf("/apps/{id}?ref=%q: %d, data-ref drawn %v; want 200 and no ref", bad, p.Code, strings.Contains(p.Body.String(), "data-ref"))
		}
		rec, link := f.mint(f.a, false, appMintBody(id, `{"app":{"ref":"`+bad+`"}}`))
		if rec.Code != http.StatusOK {
			t.Errorf("a mint with ref %q: %d %s; want 200 (the ref dropped, not refused)", bad, rec.Code, rec.Body.String())
			continue
		}
		if rec := f.redeem(tok, codeOf(t, link)); !strings.Contains(rec.Body.String(), `"target":{"app":{}}`) {
			t.Errorf("a ref %q: redeemed %s; want target {\"app\":{}} (dropped)", bad, rec.Body.String())
		}
	}
}

var appTabRE = regexp.MustCompile(`data-app-tab="(\d+)"`)

// tabsOn is the app tab ids a's /account page draws.
func (f *handoffSeam) tabsOn(uid int64) []string {
	f.t.Helper()
	rec := f.do("GET", "/account", "", f.as(uid, false))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET /account: %d", rec.Code)
	}
	var out []string
	for _, m := range appTabRE.FindAllStringSubmatch(rec.Body.String(), -1) {
		out = append(out, m[1])
	}
	return out
}

// listCounting counts the plugin store's List calls.
type listCounting struct {
	plugins.Store
	n *int
}

func (l listCounting) List(ctx context.Context) ([]plugins.Plugin, error) {
	*l.n++
	return l.Store.List(ctx)
}

// TestTheAppTabsGates: a table over each gate closed alone — plugins off,
// MUSTER_PLUGIN_ORIGINS unset, the token revoked, its origin retired, a
// plugin without `app`, another owner's app plugin — draws no tab, serves
// /apps/{id} 404 and answers the alive poll 404; the all-open row draws
// exactly the one tab (the positive control), labelled and linked, and serves
// both. With plugins off or the origins unset the store's List is not read.
// The owner-disabled row needs a GoTrue session, which this seam has not got:
// it is TestADisabledOwnersAppPageAndPollAreRefused (plugin_app_test.go) —
// no tab, the page sent to sign-in, and the alive poll 401, which the page
// script treats as removal (e2e/app-page.test.mjs).
func TestTheAppTabsGates(t *testing.T) {
	type row struct {
		name    string
		close   func(f *handoffSeam, id int64)
		noList  bool
		viewer  bool // the plugin is a viewer, not an app
		otherOw bool // the plugin is the OTHER owner's
	}
	rows := []row{
		{name: "all open"},
		{name: "plugins off", close: func(f *handoffSeam, _ int64) { f.srv.ext.Plugins = nil }, noList: true},
		{name: "origins unset", close: func(f *handoffSeam, _ int64) { f.srv.ext.PluginOrigins = nil }, noList: true},
		{name: "token revoked", close: func(f *handoffSeam, id int64) {
			if rec := f.do("POST", "/plugins/"+strconv.FormatInt(id, 10)+"/revoke", "", f.as(f.a, false)); rec.Code >= 400 {
				t.Fatalf("revoke: %d", rec.Code)
			}
		}},
		{name: "origin retired", close: func(f *handoffSeam, _ int64) {
			pats, err := plugins.ParseOrigins("https://*.other.example.net")
			if err != nil {
				t.Fatal(err)
			}
			f.srv.ext.PluginOrigins = pats
		}},
		{name: "no app capability", viewer: true},
		{name: "another owner's app", otherOw: true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newHandoffSeam(t, hoZone)
			owner := f.a
			if r.otherOw {
				owner = f.b
			}
			var id int64
			if r.viewer {
				_, id, _ = f.create(owner, viewerForm("viewer-gate", hoEntry))
			} else {
				_, id = f.mustApp(owner, "app-gate")
			}
			if id == 0 {
				t.Fatal("instrument: no plugin minted")
			}
			n := 0
			if f.srv.ext.Plugins != nil {
				f.srv.ext.Plugins = listCounting{Store: f.srv.ext.Plugins, n: &n}
			}
			if r.close != nil {
				r.close(f, id)
			}
			n = 0
			tabs := f.tabsOn(f.a)
			page := f.do("GET", "/apps/"+strconv.FormatInt(id, 10), "", f.as(f.a, false))
			alive := f.do("GET", "/ui/apps/"+strconv.FormatInt(id, 10)+"/alive", "", f.as(f.a, false))
			if r.name == "all open" {
				if len(tabs) != 1 || tabs[0] != strconv.FormatInt(id, 10) {
					t.Fatalf("POSITIVE CONTROL: tabs %v; want exactly [%d]", tabs, id)
				}
				acct := f.do("GET", "/account", "", f.as(f.a, false)).Body.String()
				if !strings.Contains(acct, `href="/apps/`+strconv.FormatInt(id, 10)+`"`) || !strings.Contains(acct, ">Team calls<") {
					t.Errorf("the tab does not link /apps/%d with its label", id)
				}
				if page.Code != http.StatusOK || alive.Code != http.StatusNoContent {
					t.Fatalf("POSITIVE CONTROL: /apps/{id} %d, alive %d; want 200 and 204", page.Code, alive.Code)
				}
				if !regexp.MustCompile(`id="sidebar-app-` + strconv.FormatInt(id, 10) + `"[^>]*aria-current="page"`).MatchString(page.Body.String()) {
					t.Errorf("/apps/{id} does not mark its own tab aria-current")
				}
				return
			}
			if len(tabs) != 0 {
				t.Errorf("tabs %v; want none with %s", tabs, r.name)
			}
			if page.Code != http.StatusNotFound || alive.Code != http.StatusNotFound {
				t.Errorf("/apps/{id} %d, alive %d; want both 404 with %s", page.Code, alive.Code, r.name)
			}
			if r.noList && n != 0 {
				t.Errorf("the plugin store's List was read %d times with %s; want none", n, r.name)
			}
		})
	}
}

// TestTheAppPageIsTheOwnersAlone: another owner's app plugin, a revoked one,
// a viewer (no app), an unknown id and a malformed id are each the one 404,
// with identical bodies, carrying the DEFAULT headers.
func TestTheAppPageIsTheOwnersAlone(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	_, other := f.mustApp(f.b, "app-b")
	_, revoked := f.mustApp(f.a, "app-revoked")
	_, viewer, _ := f.create(f.a, viewerForm("viewer-a", hoEntry))
	_, mine := f.mustApp(f.a, "app-mine")
	f.do("POST", "/plugins/"+strconv.FormatInt(revoked, 10)+"/revoke", "", f.as(f.a, false))
	var first string
	for name, path := range map[string]string{
		"another owner's": "/apps/" + strconv.FormatInt(other, 10),
		"a revoked one":   "/apps/" + strconv.FormatInt(revoked, 10),
		"a viewer":        "/apps/" + strconv.FormatInt(viewer, 10),
		"an unknown id":   "/apps/999999999",
		"a malformed id":  "/apps/0" + strconv.FormatInt(mine, 10),
	} {
		rec := f.do("GET", path, "", f.as(f.a, false))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), AppPageNotFound) {
			t.Errorf("%s: %d; want the one 404", name, rec.Code)
			continue
		}
		if first == "" {
			first = rec.Body.String()
		} else if rec.Body.String() != first {
			t.Errorf("%s: its 404 body differs from another's", name)
		}
		if pp := rec.Header().Get("Permissions-Policy"); pp != "microphone=(self)" {
			t.Errorf("%s: Permissions-Policy %q on the 404; want microphone=(self)", name, pp)
		}
		if strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-src") {
			t.Errorf("%s: the 404's CSP has a frame-src", name)
		}
	}
	// CONTROL: the owner's own is served.
	if rec := f.do("GET", "/apps/"+strconv.FormatInt(mine, 10), "", f.as(f.a, false)); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the owner's own app page: %d; want 200", rec.Code)
	}
}

// appPageCSP is /apps/{id}'s Content-Security-Policy, written out whole.
const appPageCSP = "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https://*:*/brand/icon-192.png http://localhost:*/brand/icon-192.png http://127.0.0.1:*/brand/icon-192.png; " +
	"connect-src 'self'; frame-ancestors 'none'; frame-src https://w.viewer.example.net; base-uri 'self'; form-action 'self'"

// appPagePP is /apps/{id}'s Permissions-Policy, written out whole: `self`
// kept on every feature (spike S5).
const appPagePP = `microphone=(self "https://w.viewer.example.net"), camera=(self "https://w.viewer.example.net"), ` +
	`display-capture=(self "https://w.viewer.example.net"), fullscreen=(self "https://w.viewer.example.net"), ` +
	`autoplay=(self "https://w.viewer.example.net"), clipboard-write=(self "https://w.viewer.example.net")`

// appFrameAllow and appFrameSandbox are the frame's attributes, whole.
const (
	appFrameAllow = "camera https://w.viewer.example.net; microphone https://w.viewer.example.net; " +
		"display-capture https://w.viewer.example.net; fullscreen https://w.viewer.example.net; " +
		"autoplay https://w.viewer.example.net; clipboard-write https://w.viewer.example.net"
	appFrameSandbox = "allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals"
)

var appIframeRE = regexp.MustCompile(`<iframe[^>]*>`)

func appAttrOf(tag, name string) (string, bool) {
	m := regexp.MustCompile(`\s` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// TestTheAppPageHeadersAndFrameExactly: /apps/{id}'s CSP and
// Permissions-Policy are pinned whole strings naming the entry URL's exact
// origin (not the origin pattern, no 'self' in frame-src, `self` kept on every
// feature), frame-ancestors 'none' and X-Frame-Options DENY stay; the page has
// exactly one frame, whose sandbox is the preview panel's and whose allow is
// a pinned whole string naming that origin.
func TestTheAppPageHeadersAndFrameExactly(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	_, id := f.mustApp(f.a, "app-headers")
	lists := 0
	f.srv.ext.Plugins = listCounting{Store: f.srv.ext.Plugins, n: &lists}
	rec := f.do("GET", "/apps/"+strconv.FormatInt(id, 10), "", f.as(f.a, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps/{id}: %d", rec.Code)
	}
	// The page and its sidebar share one read of the plugin list.
	if lists != 1 {
		t.Errorf("GET /apps/{id} listed the plugin store %d times; want 1 (the page's list reused by its sidebar)", lists)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != appPageCSP {
		t.Errorf("CSP\n got %s\nwant %s", got, appPageCSP)
	}
	if got := rec.Header().Get("Permissions-Policy"); got != appPagePP {
		t.Errorf("Permissions-Policy\n got %s\nwant %s", got, appPagePP)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options %q; want DENY", got)
	}
	frames := appIframeRE.FindAllString(rec.Body.String(), -1)
	if len(frames) != 1 {
		t.Fatalf("the page draws %d frames; want 1", len(frames))
	}
	for name, want := range map[string]string{"sandbox": appFrameSandbox, "allow": appFrameAllow, "referrerpolicy": "no-referrer"} {
		if got, ok := appAttrOf(frames[0], name); !ok || got != want {
			t.Errorf("the frame's %s = %q (present %v); want %q", name, got, ok, want)
		}
	}
	if _, ok := appAttrOf(frames[0], "src"); ok {
		t.Errorf("the drawn frame has a src: the script swaps in the minted one")
	}
}

// TestTheAlivePollIsLiveUntilRevoked: GET /ui/apps/{id}/alive is 204 for the
// owner's live app plugin and 404 after it is revoked on /plugins, and 404 for
// another owner asking about it.
func TestTheAlivePollIsLiveUntilRevoked(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	_, id := f.mustApp(f.a, "app-alive")
	path := "/ui/apps/" + strconv.FormatInt(id, 10) + "/alive"
	if rec := f.do("GET", path, "", f.as(f.a, false)); rec.Code != http.StatusNoContent {
		t.Fatalf("live: %d; want 204", rec.Code)
	}
	if rec := f.do("GET", path, "", f.as(f.b, false)); rec.Code != http.StatusNotFound {
		t.Errorf("another owner asking: %d; want 404", rec.Code)
	}
	f.do("POST", "/plugins/"+strconv.FormatInt(id, 10)+"/revoke", "", f.as(f.a, false))
	if rec := f.do("GET", path, "", f.as(f.a, false)); rec.Code != http.StatusNotFound {
		t.Fatalf("after revoke: %d; want 404", rec.Code)
	}
}

// TestTheAppLabelIsEscapedOnThePage: a label with ' and & (which 1a admits)
// appears on the page and in the sidebar only escaped — never raw — in the
// <title>, the header, the sidebar entry, the frame's title and the card.
func TestTheAppLabelIsEscapedOnThePage(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	form := appForm("app-label", hoEntry)
	const label = "Tom's & Jerry's"
	form.Set("app_label", label)
	tok, id, rec := f.create(f.a, form)
	if tok == "" {
		t.Fatalf("minting an app plugin labelled %q: %d", label, rec.Code)
	}
	page := f.do("GET", "/apps/"+strconv.FormatInt(id, 10), "", f.as(f.a, false)).Body.String()
	if strings.Contains(page, label) || strings.Contains(page, "Tom's") || strings.Contains(page, "& Jerry") {
		t.Errorf("the page carries the label raw")
	}
	esc := "Tom&#39;s &amp; Jerry&#39;s"
	for name, want := range map[string]string{
		"<title>":         "<title>" + esc + " · muster</title>",
		"the header":      ">" + esc + "</h1>",
		"the sidebar":     `<span class="min-w-0 truncate">` + esc + "</span>",
		"the frame title": `title="` + esc + `"`,
		"the card":        ">" + esc + " opens in your browser</p>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("%s does not carry the escaped label (%s)", name, want)
		}
	}
}
