package api

import (
	"encoding/json"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/theme"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uikit"
)

// themeServer is brandingServer with MUSTER_PLUGIN_ORIGINS set to an exact
// host and a wildcard zone (neither is muster's own host).
func themeServer(t *testing.T) (*Server, http.Handler, *memBranding, *http.Cookie) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	mem := &memBranding{}
	origins, err := plugins.ParseOrigins("https://meet.example.org, https://*.plugins.example.test")
	if err != nil {
		t.Fatal(err)
	}
	s.UseExtensions(Extensions{Notes: &pwaNotes{}, Branding: mem, PluginOrigins: origins})
	s.sessionUserOverride = func(r *http.Request) int64 {
		if v := r.Header.Get(testMemberHeader); v != "" {
			uid, _ := strconv.ParseInt(v, 10, 64)
			return uid
		}
		return authz.OperatorUserID
	}
	h := s.Handler()
	return s, h, mem, loginCookie(t, h)
}

func putTheme(h http.Handler, as who, preset, accent string) *httptest.ResponseRecorder {
	form := url.Values{"preset": {preset}, "accent": {accent}}
	return brandingDo(h, http.MethodPut, ThemePath, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", as)
}

func kitGet(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var kitPaths = map[string]string{
	uikit.KitCSSPath:   "text/css; charset=utf-8",
	uikit.ThemeCSSPath: "text/css; charset=utf-8",
	uikit.IconsPath:    "image/svg+xml",
	uikit.KitJSPath:    "text/javascript; charset=utf-8",
}

// TestTheKitIsServedWithTheContractHeaders: each file, with no session, is
// 200 with its type, nosniff, CORP cross-origin, an ETag that is the hash of
// the body; unversioned is no-cache, ?v=<etag> is immutable, a
// stale v is no-cache again, and If-None-Match answers 304.
func TestTheKitIsServedWithTheContractHeaders(t *testing.T) {
	_, h, _, _ := themeServer(t)
	for path, ctype := range kitPaths {
		rec := kitGet(h, path, nil)
		hd := rec.Header()
		if rec.Code != http.StatusOK || hd.Get("Content-Type") != ctype {
			t.Fatalf("%s: %d %q, want 200 %q", path, rec.Code, hd.Get("Content-Type"), ctype)
		}
		etag := theme.ETag(rec.Body.Bytes())
		if hd.Get("ETag") != `"`+etag+`"` {
			t.Errorf("%s: ETag %s, want the body's hash %q", path, hd.Get("ETag"), etag)
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
			t.Errorf("%s: nosniff=%q CORP=%q", path, hd.Get("X-Content-Type-Options"), hd.Get("Cross-Origin-Resource-Policy"))
		}
		if hd.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s unversioned: Cache-Control %q, want no-cache", path, hd.Get("Cache-Control"))
		}
		if cc := kitGet(h, path+"?v="+etag, nil).Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("%s?v=<etag>: Cache-Control %q, want immutable", path, cc)
		}
		if cc := kitGet(h, path+"?v=0123456789abcdef", nil).Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s with a stale v: Cache-Control %q, want no-cache", path, cc)
		}
		if r := kitGet(h, path, map[string]string{"If-None-Match": `"` + etag + `"`}); r.Code != http.StatusNotModified || r.Body.Len() != 0 {
			t.Errorf("%s If-None-Match: %d with %d bytes, want 304 empty", path, r.Code, r.Body.Len())
		}
	}
	if uikit.ThemeCSSPath != ui.ThemeCSSPath {
		t.Errorf("the pages link %s but the kit serves %s", ui.ThemeCSSPath, uikit.ThemeCSSPath)
	}
}

// TestTheKitETagAndBodyMoveWithTheTheme: an admin's theme changes kit.css and
// theme.css — body (the accent's channels) and ETag — and leaves icons.svg
// and kit.js as they were; the old ?v= then reads no-cache.
func TestTheKitETagAndBodyMoveWithTheTheme(t *testing.T) {
	_, h, _, cookie := themeServer(t)
	before := map[string]string{}
	for path := range kitPaths {
		before[path] = kitGet(h, path, nil).Header().Get("ETag")
	}
	if rec := putTheme(h, who{cookie: cookie}, "verdigris", "#d98ae8"); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	for path := range kitPaths {
		rec := kitGet(h, path, nil)
		changed := rec.Header().Get("ETag") != before[path]
		themed := path == uikit.KitCSSPath || path == uikit.ThemeCSSPath
		if changed != themed {
			t.Errorf("%s: ETag changed=%v after a theme change; want %v", path, changed, themed)
		}
		if themed {
			if !strings.Contains(rec.Body.String(), "--mu-accent: 217 138 232;") {
				t.Errorf("%s does not carry the new accent's channels", path)
			}
			old := strings.Trim(before[path], `"`)
			if cc := kitGet(h, path+"?v="+old, nil).Header().Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("%s at the old ?v=: %q, want no-cache", path, cc)
			}
		}
	}
}

// TestTheKitSendsNoCORSToAnyone: the kit needs no CORS (kit.js carries the
// sprite), so no origin is granted a read — a plugin origin under
// MUSTER_PLUGIN_ORIGINS, another site, or muster's own — and nothing varies by
// Origin. (A plugin's icons do not depend on being in MUSTER_PLUGIN_ORIGINS:
// internal/uikit TestKitJSCarriesTheSprite, and the parity e2e loads kit.js on
// a page of another origin and draws an icon.)
func TestTheKitSendsNoCORSToAnyone(t *testing.T) {
	_, h, _, _ := themeServer(t)
	for _, origin := range []string{"https://meet.example.org", "https://a.plugins.example.test", "https://other.example.net", "http://example.com", "null", ""} {
		for path := range kitPaths {
			hd := kitGet(h, path, map[string]string{"Origin": origin}).Header()
			if got := hd.Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("%s from %q: Access-Control-Allow-Origin %q, want none", path, origin, got)
			}
			if got := hd.Get("Vary"); strings.Contains(got, "Origin") {
				t.Errorf("%s from %q: Vary %q; nothing varies by Origin", path, origin, got)
			}
		}
	}
}

// TestTheKitCarriesNoSessionOrUserData: every kit file is the same bytes and
// the same headers (but Date) for nobody, the admin and a member; none sets a
// cookie; none names a user. A control first: the same comparison over the
// Account page must DIFFER between admin and member, or it can see nothing.
func TestTheKitCarriesNoSessionOrUserData(t *testing.T) {
	_, h, _, cookie := themeServer(t)
	putTheme(h, who{cookie: cookie}, "ember", "")
	acct := func(member bool) string {
		req := httptest.NewRequest(http.MethodGet, "/account", nil)
		req.AddCookie(cookie)
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		if member {
			req.Header.Set(testMemberHeader, "2")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if acct(false) == acct(true) {
		t.Fatal("control: the Account page reads the same for admin and member; the comparison cannot see a per-user difference")
	}
	for path := range kitPaths {
		var bodies []string
		var heads []http.Header
		for _, as := range []struct {
			cookie bool
			member bool
		}{{false, false}, {true, false}, {true, true}} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if as.cookie {
				req.AddCookie(cookie)
			}
			if as.member {
				req.Header.Set(testMemberHeader, "2")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d", path, rec.Code)
			}
			if sc := rec.Header().Values("Set-Cookie"); len(sc) != 0 {
				t.Errorf("%s sets a cookie: %v", path, sc)
			}
			hd := rec.Header().Clone()
			hd.Del("Date")
			bodies = append(bodies, rec.Body.String())
			heads = append(heads, hd)
		}
		for i := 1; i < len(bodies); i++ {
			if bodies[i] != bodies[0] {
				t.Errorf("%s: the body differs by caller (%d)", path, i)
			}
			// Both ways: a header only one caller gets is a difference too.
			keys := map[string]bool{}
			for k := range heads[0] {
				keys[k] = true
			}
			for k := range heads[i] {
				keys[k] = true
			}
			for k := range keys {
				if heads[i].Get(k) != heads[0].Get(k) {
					t.Errorf("%s: header %s differs by caller: %q vs %q", path, k, heads[i].Get(k), heads[0].Get(k))
				}
			}
		}
		for _, leak := range []string{"user", "session", "operator", "csrf", "password"} {
			if path != uikit.KitJSPath && strings.Contains(strings.ToLower(bodies[0]), leak) {
				t.Errorf("%s mentions %q", path, leak)
			}
		}
	}
}

// TestANonCompliantAccentIsRefusedAtSave: #3A2F6B (no preset's) fails the
// contrast table — 400 theme_contrast with the failing pairs, nothing written;
// the htmx form is sent back to the Account page with the code, which draws a
// fixed sentence. A malformed accent and an unknown preset are refused by
// their own codes. A compliant accent is then saved (the control).
func TestANonCompliantAccentIsRefusedAtSave(t *testing.T) {
	_, h, mem, cookie := themeServer(t)
	rec := putTheme(h, who{cookie: cookie}, "brass", "#3a2f6b")
	var body struct {
		Code     string   `json:"code"`
		Failures []string `json:"failures"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusBadRequest || body.Code != "theme_contrast" || len(body.Failures) == 0 ||
		!strings.Contains(strings.Join(body.Failures, "\n"), "accent #3A2F6B on bg") {
		t.Fatalf("a dark accent: %d %s; want 400 theme_contrast naming `accent #3A2F6B on bg`", rec.Code, rec.Body.String())
	}
	if mem.writeCount() != 0 {
		t.Fatalf("a refused theme was written (%d writes)", mem.writeCount())
	}
	rec = putTheme(h, who{cookie: cookie, htmx: true}, "brass", "#3a2f6b")
	const redirect = "/account?theme_error=theme_contrast&theme_try_preset=brass&theme_try_accent=%233a2f6b#theme"
	if got := rec.Header().Get("HX-Redirect"); got != redirect {
		t.Errorf("htmx refusal redirect = %q, want %q", got, redirect)
	}
	page := get(h, strings.TrimSuffix(redirect, "#theme"), cookie, true).Body.String()
	if !strings.Contains(page, html.EscapeString(themeErrorText["theme_contrast"])) {
		t.Error("the Account page does not say why the theme was refused")
	}
	// It names the failing pairs, recomputed from the tried values.
	if !strings.Contains(page, `data-testid="theme-failures"`) || !strings.Contains(page, "accent #3A2F6B on bg") {
		t.Error("the Account page does not name the failing pairs of the refused theme")
	}
	// Tried values that do not parse, or do not fail, draw no list and none of
	// the query's text.
	for _, q := range []string{"theme_try_accent=%3Cb%3Ezzq", "theme_try_preset=zzq", "theme_try_accent=%237fd0ff"} {
		p := get(h, "/account?theme_error=theme_contrast&"+q, cookie, true).Body.String()
		if strings.Contains(p, `data-testid="theme-failures"`) || strings.Contains(p, "zzq") {
			t.Errorf("?%s: a failure list, or the query's text, was drawn", q)
		}
	}
	// An unknown code draws the generic sentence, never the query's text.
	if page := get(h, "/account?theme_error=%3Cb%3Ezzq%3C/b%3E", cookie, true).Body.String(); !strings.Contains(page, "The theme could not be saved.") || strings.Contains(page, "zzq") {
		t.Error("an unknown refusal code is not answered with the generic sentence, or the query's text is drawn")
	}
	for _, c := range []struct{ preset, accent, code string }{
		{"brass", "red", "theme_accent_invalid"},
		{"neon", "", "theme_preset_unknown"},
	} {
		if rec := putTheme(h, who{cookie: cookie}, c.preset, c.accent); rec.Code != http.StatusBadRequest || errCode(t, rec) != c.code {
			t.Errorf("%+v: %d %s, want 400 %s", c, rec.Code, rec.Body.String(), c.code)
		}
	}
	if mem.writeCount() != 0 {
		t.Fatalf("a refused theme was written")
	}
	if rec := putTheme(h, who{cookie: cookie}, "brass", "#7fd0ff"); rec.Code != http.StatusOK || mem.writeCount() != 1 {
		t.Fatalf("control: a readable accent: %d %s, writes %d", rec.Code, rec.Body.String(), mem.writeCount())
	}
}

// TestOnlyAnAdminSavesATheme: a member is 403, no session 401, another
// origin is refused by the central check, and none writes; the preview is an
// admin's too.
func TestOnlyAnAdminSavesATheme(t *testing.T) {
	_, h, mem, cookie := themeServer(t)
	if rec := putTheme(h, who{cookie: cookie, member: true}, "harbor", ""); rec.Code != http.StatusForbidden {
		t.Errorf("a member: %d, want 403", rec.Code)
	}
	if rec := putTheme(h, who{}, "harbor", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d, want 401", rec.Code)
	}
	if rec := putTheme(h, who{cookie: cookie, origin: "https://meet.example.org"}, "harbor", ""); rec.Code != http.StatusForbidden {
		t.Errorf("a plugin origin: %d, want 403 (the central Origin check)", rec.Code)
	}
	if mem.callCount() != 0 || mem.writeCount() != 0 {
		t.Errorf("a refused caller reached the store (%d calls)", mem.callCount())
	}
	if rec := putTheme(h, who{cookie: cookie}, "harbor", ""); rec.Code != http.StatusOK || mem.writeCount() != 1 {
		t.Errorf("control: the admin: %d, writes %d", rec.Code, mem.writeCount())
	}
}

// TestASavedThemeReachesTheInstanceDocumentAndThePages: with the default
// theme the instance document says brass and the pages link no theme.css;
// after a save it names the preset, the accent and theme.css's versioned URL,
// and the shell, the Account page and the sign-in page link exactly that URL.
func TestASavedThemeReachesTheInstanceDocumentAndThePages(t *testing.T) {
	_, h, _, cookie := themeServer(t)
	doc := func() InstanceDoc {
		var d InstanceDoc
		if err := json.Unmarshal(kitGet(h, InstancePath, nil).Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	pages := func() map[string]string {
		return map[string]string{
			"shell":   get(h, "/tasks", cookie, true).Body.String(),
			"account": get(h, "/account", cookie, true).Body.String(),
			"sign-in": get(h, "/login", nil, true).Body.String(),
		}
	}
	d := doc()
	if d.Theme == nil || d.Theme.Preset != "brass" || d.Theme.Accent != nil {
		t.Fatalf("default instance theme = %+v", d.Theme)
	}
	for name, p := range pages() {
		if strings.Contains(p, uikit.ThemeCSSPath) {
			t.Errorf("%s links the theme stylesheet with the default theme", name)
		}
	}
	if rec := putTheme(h, who{cookie: cookie}, "harbor", "#5aa9f0"); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	d = doc()
	themeETag := strings.Trim(kitGet(h, uikit.ThemeCSSPath, nil).Header().Get("ETag"), `"`)
	if d.Theme == nil || d.Theme.Preset != "harbor" || d.Theme.Accent == nil || *d.Theme.Accent != "#5AA9F0" ||
		d.Theme.ETag != themeETag || d.Theme.CSS != uikit.ThemeCSSPath+"?v="+themeETag {
		t.Fatalf("instance theme = %+v (theme.css ETag %s)", d.Theme, themeETag)
	}
	link := `<link rel="stylesheet" data-theme-css="" href="` + uikit.ThemeCSSPath + `?v=` + themeETag + `">`
	for name, p := range pages() {
		if strings.Count(p, link) != 1 {
			t.Errorf("%s links the theme %d times as %s (%d bytes)", name, strings.Count(p, link), link, len(p))
		}
	}
}

// TestTheAdminsAccountPageHasTheThemeSectionAndALivePreview: the section and
// its form are an admin's only; each preset option carries its resolved
// accent, label and focus channels for the browser preview (harbor's, pinned
// here as literals), and the preview script derives a typed accent with
// theme.Derive's own factors. There is no server preview route any more.
func TestTheAdminsAccountPageHasTheThemeSectionAndALivePreview(t *testing.T) {
	_, h, _, cookie := themeServer(t)
	admin := get(h, "/account", cookie, true).Body.String()
	for _, want := range []string{
		`data-testid="theme"`, `hx-put="` + ThemePath + `"`, `data-testid="theme-preview"`,
		`<option value="harbor" data-accent="110 176 245" data-on-accent="14 23 32" data-focus="154 200 248">`,
		"var DARKEN = 0.87, LIGHTEN = 0.3;",
	} {
		if !strings.Contains(admin, want) {
			t.Errorf("the admin's Account page lacks %s", want)
		}
	}
	if strings.Contains(admin, "/api/instance/theme/preview") {
		t.Error("the page still points at a server preview")
	}
	if rec := get(h, "/api/instance/theme/preview?preset=harbor", cookie, false); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/instance/theme/preview: %d, want it gone", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(cookie)
	req.Header.Set(testMemberHeader, "2")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `data-testid="theme"`) {
		t.Errorf("a member's Account page (%d) has the theme section", rec.Code)
	}
}
