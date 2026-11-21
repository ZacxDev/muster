package api

import (
	"encoding/json"
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
// 200 with its type, nosniff, CORP cross-origin, Vary: Origin, an ETag that is
// the hash of the body; unversioned is no-cache, ?v=<etag> is immutable, a
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
		if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Cross-Origin-Resource-Policy") != "cross-origin" || hd.Get("Vary") != "Origin" {
			t.Errorf("%s: nosniff=%q CORP=%q Vary=%q", path, hd.Get("X-Content-Type-Options"), hd.Get("Cross-Origin-Resource-Policy"), hd.Get("Vary"))
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

// TestTheKitCORSIsTheirsAlone: an https origin under MUSTER_PLUGIN_ORIGINS is
// echoed; muster's own origin, another site, http, a port, a path, the
// wildcard's own zone and a look-alike suffix are not. With no
// MUSTER_PLUGIN_ORIGINS nobody is.
func TestTheKitCORSIsTheirsAlone(t *testing.T) {
	_, h, _, _ := themeServer(t)
	for origin, allowed := range map[string]bool{
		"https://meet.example.org":             true,
		"https://a.plugins.example.test":       true,
		"https://b.c.plugins.example.test":     true,
		"https://plugins.example.test":         false, // the zone itself
		"https://evilplugins.example.test":     false,
		"https://meet.example.org.attacker.io": false,
		"http://meet.example.org":              false,
		"https://meet.example.org:8443":        false,
		"https://meet.example.org/x":           false,
		"https://other.example.net":            false,
		"http://example.com":                   false, // muster's own (httptest's host)
		"null":                                 false,
	} {
		for path := range kitPaths {
			got := kitGet(h, path, map[string]string{"Origin": origin}).Header().Get("Access-Control-Allow-Origin")
			if allowed && got != origin {
				t.Errorf("%s from %s: ACAO %q, want the origin echoed", path, origin, got)
			}
			if !allowed && got != "" {
				t.Errorf("%s from %s: ACAO %q, want none", path, origin, got)
			}
		}
	}
	_, h2, _, _ := brandingServer(t) // no MUSTER_PLUGIN_ORIGINS
	if got := kitGet(h2, uikit.IconsPath, map[string]string{"Origin": "https://meet.example.org"}).Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("with no plugin origins: ACAO %q, want none", got)
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
			for k := range heads[0] {
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
	if got := rec.Header().Get("HX-Redirect"); got != "/account?theme_error=theme_contrast#theme" {
		t.Errorf("htmx refusal redirect = %q", got)
	}
	page := get(h, "/account?theme_error=theme_contrast", cookie, true).Body.String()
	if !strings.Contains(page, themeErrorText["theme_contrast"]) {
		t.Error("the Account page does not say why the theme was refused")
	}
	if page := get(h, "/account?theme_error=%3Cscript%3E", cookie, true).Body.String(); strings.Contains(page, "<script>") && !strings.Contains(page, "The theme could not be saved.") {
		t.Error("an unknown refusal code is drawn from the query")
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
	req := httptest.NewRequest(http.MethodGet, ThemePreviewPath+"?preset=harbor", nil)
	req.AddCookie(cookie)
	req.Header.Set(testMemberHeader, "2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a member's preview: %d, want 403", rec.Code)
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
// its form are an admin's only; the preview endpoint draws the chosen accent
// and, for an unreadable one, names the failing pairs.
func TestTheAdminsAccountPageHasTheThemeSectionAndALivePreview(t *testing.T) {
	_, h, _, cookie := themeServer(t)
	admin := get(h, "/account", cookie, true).Body.String()
	for _, want := range []string{`data-testid="theme"`, `hx-put="` + ThemePath + `"`, `hx-get="` + ThemePreviewPath + `"`, `data-testid="theme-preview"`, `<option value="verdigris"`} {
		if !strings.Contains(admin, want) {
			t.Errorf("the admin's Account page lacks %s", want)
		}
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
	prev := func(q string) string {
		req := httptest.NewRequest(http.MethodGet, ThemePreviewPath+"?"+q, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview %s: %d", q, rec.Code)
		}
		return rec.Body.String()
	}
	if p := prev("preset=brass&accent=%237fd0ff"); !strings.Contains(p, `data-theme-ok="true"`) || !strings.Contains(p, "--mu-accent: 127 208 255;") {
		t.Errorf("a readable accent's preview: %s", p)
	}
	if p := prev("preset=brass&accent=%233a2f6b"); !strings.Contains(p, `data-theme-ok="false"`) || !strings.Contains(p, "accent #3A2F6B on bg") {
		t.Errorf("an unreadable accent's preview does not name the failing pair: %s", p)
	}
}
