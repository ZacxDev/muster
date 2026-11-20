package api

import (
	"crypto/tls"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/gotrue"
)

// The deployments these tests run as. prefixedAuth is an https public URL
// with the resolved secure-cookie setting on; composeAuth is the Compose
// stack's (MUSTER_SECURE_COOKIES=1 on an http://localhost URL).
const testPrefixedURL = "https://muster.example.test"

// testHostPrefix is spelled here rather than read from hostCookiePrefix, so the
// expectation is not derived from the implementation it tests.
const testHostPrefix = "__Host-"

func prefixedAuth(mode AuthMode) AuthConfig {
	return AuthConfig{Mode: mode, UIPassword: testUIPassword, HookToken: testHookToken,
		SecureCookies: true, PublicURL: testPrefixedURL}
}

func composeAuth(mode AuthMode) AuthConfig {
	return AuthConfig{Mode: mode, UIPassword: testUIPassword, HookToken: testHookToken,
		SecureCookies: true, PublicURL: "http://localhost:8105"}
}

func cookieServer(c AuthConfig) *Server { return New(nil, c, log.New(io.Discard, "", 0)) }

// loopbackPage is a request from a page this machine serves over plain http on
// loopback (loopbackHTTPPage is true for it).
func loopbackPage(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, "http://localhost:8105"+path, body)
	r.Host = "localhost:8105"
	r.Header.Set("Origin", "http://localhost:8105")
	return r
}

// httpsPage is a request from muster's own https page.
func httpsPage(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, testPrefixedURL+path, body)
	r.Host = "muster.example.test"
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("Origin", testPrefixedURL)
	return r
}

var cookieValueRE = regexp.MustCompile(`^([^=]+)=[^;]*`)

// setCookieLines is every Set-Cookie on w, each with its value replaced by V,
// so the rest of the line can be pinned whole.
func setCookieLines(w *httptest.ResponseRecorder) []string {
	var out []string
	for _, l := range w.Result().Header.Values("Set-Cookie") {
		out = append(out, cookieValueRE.ReplaceAllString(l, "${1}=V"))
	}
	return out
}

// sevenLiterals drives each of muster's seven http.Cookie literals (the sets
// and the clears) once, on r, and returns its Set-Cookie lines by literal.
func sevenLiterals(t *testing.T, c AuthConfig, r func() *http.Request) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	run := func(name string, f func(w http.ResponseWriter)) {
		w := httptest.NewRecorder()
		f(w)
		out[name] = setCookieLines(w)
	}
	pw := cookieServer(c)
	gt := c
	gt.Mode = AuthModeGoTrue
	gs := cookieServer(gt)
	run("session set (login.go)", func(w http.ResponseWriter) { pw.setPasswordSessionCookie(w, r()) })
	run("session clear (login.go)", func(w http.ResponseWriter) { pw.handleLogout(w, r()) })
	run("known device (known_device.go)", func(w http.ResponseWriter) { pw.setKnownDeviceCookie(w, r()) })
	run("gotrue set (accounts.go)", func(w http.ResponseWriter) {
		gs.setGoTrueCookies(w, gotrue.Session{AccessToken: "a", RefreshToken: "r"})
	})
	run("gotrue clear (accounts.go)", func(w http.ResponseWriter) { gs.clearGoTrueCookies(w) })
	run("push device set (push_routes.go)", func(w http.ResponseWriter) { pw.setPushDeviceCookie(w, r(), "h") })
	run("push device clear (push_routes.go)", func(w http.ResponseWriter) { pw.clearPushDeviceCookie(w, r()) })
	return out
}

// TestHostPrefixedCookieGolden: on a prefixed deployment every Set-Cookie the
// seven literals emit is named `__Host-…`, Secure, Path=/, no Domain, HttpOnly
// (pinned whole) — on an https request AND on a loopback http one, since a
// `__Host-` cookie without Secure is refused and the per-request exception
// must not drop it there. Red if one literal keeps its old name, gains a
// Domain, or drops Secure.
func TestHostPrefixedCookieGolden(t *testing.T) {
	want := map[string][]string{
		"session set (login.go)":             {"__Host-muster_session=V; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax"},
		"session clear (login.go)":           {"__Host-muster_session=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"},
		"known device (known_device.go)":     {"__Host-muster_device=V; Path=/; Max-Age=34560000; HttpOnly; Secure; SameSite=Lax"},
		"gotrue set (accounts.go)":           {"__Host-muster_at=V; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax", "__Host-muster_rt=V; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax"},
		"gotrue clear (accounts.go)":         {"__Host-muster_at=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax", "__Host-muster_rt=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"},
		"push device set (push_routes.go)":   {"__Host-muster_push_device=V; Path=/; Max-Age=34560000; HttpOnly; Secure; SameSite=Lax"},
		"push device clear (push_routes.go)": {"__Host-muster_push_device=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"},
	}
	for reqName, r := range map[string]func() *http.Request{
		"https":         func() *http.Request { return httpsPage(http.MethodPost, "/x", nil) },
		"loopback http": func() *http.Request { return loopbackPage(http.MethodPost, "/x", nil) },
	} {
		got := sevenLiterals(t, prefixedAuth(""), r)
		if len(got) != len(want) {
			t.Fatalf("%s: drove %d literals, want %d", reqName, len(got), len(want))
		}
		for lit, w := range want {
			if !slices.Equal(got[lit], w) {
				t.Errorf("%s, %s:\n got %q\nwant %q", reqName, lit, got[lit], w)
			}
		}
	}
}

// TestCookiesAreHostOnlyOnEveryDeployment: no Set-Cookie from the seven
// literals ever carries a Domain, and each is HttpOnly — on a prefixed
// deployment and on the Compose (unprefixed) one, where the names stay as they
// were. An invariant guard: no cookie violates it today; it is what a page on
// a subdomain being unable to READ muster's cookies rests on.
func TestCookiesAreHostOnlyOnEveryDeployment(t *testing.T) {
	for depName, c := range map[string]AuthConfig{"prefixed": prefixedAuth(""), "compose": composeAuth("")} {
		got := sevenLiterals(t, c, func() *http.Request { return loopbackPage(http.MethodPost, "/x", nil) })
		for lit, lines := range got {
			if len(lines) == 0 {
				t.Errorf("%s, %s: no Set-Cookie (the fixture drove nothing)", depName, lit)
			}
			for _, l := range lines {
				ck, err := http.ParseSetCookie(l)
				if err != nil {
					t.Fatalf("%s, %s: %q: %v", depName, lit, l, err)
				}
				if ck.Domain != "" || strings.Contains(strings.ToLower(l), "domain=") {
					t.Errorf("%s, %s: %q carries a Domain", depName, lit, l)
				}
				if !ck.HttpOnly || ck.Path != "/" {
					t.Errorf("%s, %s: %q is not HttpOnly with Path=/", depName, lit, l)
				}
				if prefixed := strings.HasPrefix(ck.Name, testHostPrefix); prefixed != (depName == "prefixed") {
					t.Errorf("%s, %s: name %q (prefixed=%v)", depName, lit, ck.Name, prefixed)
				}
			}
		}
	}
}

// TestAnOldNamedSessionIsSignedOutOnAPrefixedDeployment: a valid, signed
// session under the OLD name is not read on a prefixed deployment — GET /login
// renders the form rather than sending the caller on — and the response
// expires the old name. The control: the same value under the new name signs
// the caller in (303), so the fixture's session is valid.
func TestAnOldNamedSessionIsSignedOutOnAPrefixedDeployment(t *testing.T) {
	s := cookieServer(prefixedAuth(""))
	val := auth.SignSession(s.auth.uiSessionSecret(), time.Now(), time.Hour)
	get := func(name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, testPrefixedURL+loginPath, nil)
		r.Host = "muster.example.test"
		r.TLS = &tls.ConnectionState{}
		r.AddCookie(&http.Cookie{Name: name, Value: val})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := get(testHostPrefix + sessionCookieName); w.Code != http.StatusSeeOther {
		t.Fatalf("CONTROL: the session under its new name answered %d, want 303 (signed in)", w.Code)
	}
	w := get(sessionCookieName)
	if w.Code != http.StatusOK {
		t.Fatalf("an old-named session answered %d, want 200 (the sign-in form: signed out)", w.Code)
	}
	if got, want := setCookieLines(w), []string{"muster_session=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"}; !slices.Equal(got, want) {
		t.Errorf("Set-Cookie %q, want the old name expired: %q", got, want)
	}
}

// TestEveryOldNameIsExpiredOnce: a request carrying each old-named cookie gets
// each expired, on a prefixed deployment only; one carrying none gets none.
func TestEveryOldNameIsExpiredOnce(t *testing.T) {
	req := func(names ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, testPrefixedURL+"/health", nil)
		r.Host = "muster.example.test"
		for _, n := range names {
			r.AddCookie(&http.Cookie{Name: n, Value: "x"})
		}
		return r
	}
	serve := func(c AuthConfig, r *http.Request) []string {
		w := httptest.NewRecorder()
		cookieServer(c).Handler().ServeHTTP(w, r)
		return setCookieLines(w)
	}
	got := serve(prefixedAuth(""), req("muster_session", "muster_at", "muster_rt", "muster_device", "muster_push_device", "theme"))
	sort.Strings(got)
	want := []string{
		"muster_at=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax",
		"muster_device=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax",
		"muster_push_device=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax",
		"muster_rt=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax",
		"muster_session=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax",
	}
	if !slices.Equal(got, want) {
		t.Errorf("prefixed: Set-Cookie %q\nwant %q", got, want)
	}
	if got := serve(prefixedAuth(""), req("__Host-muster_session", "theme")); len(got) != 0 {
		t.Errorf("prefixed, no old names: Set-Cookie %q, want none", got)
	}
	if got := serve(composeAuth(""), req("muster_session", "muster_device")); len(got) != 0 {
		t.Errorf("unprefixed: Set-Cookie %q, want none (those ARE its names)", got)
	}
}

// TestTheCookieNameIsPerDeploymentNotPerRequest (Y1): for each deployment, a
// sign-in POST and the following GET with NO Origin and NO Referer (what
// Referrer-Policy: no-referrer makes a page load) use the SAME name, and the
// GET is signed in. Red if the name is derived from loopbackHTTPPage(r): on
// the Compose row the POST (loopback Origin) and the GET (none) would disagree,
// and the GET would land signed out — the sign-in loop.
func TestTheCookieNameIsPerDeploymentNotPerRequest(t *testing.T) {
	for _, c := range []struct {
		name     string
		cfg      AuthConfig
		https    bool // the sign-in is from muster's https page (a prefixed deployment refuses a loopback one)
		wantName string
	}{
		{"https URL, secure on", prefixedAuth(""), true, "__Host-muster_session"},
		{"https URL, secure off", AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, PublicURL: testPrefixedURL}, true, "muster_session"},
		{"compose: secure on, http://localhost URL", composeAuth(""), false, "muster_session"},
		{"plain http, secure off", AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, PublicURL: "http://muster.example.test"}, false, "muster_session"},
		{"no public URL, secure on", AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, SecureCookies: true}, false, "muster_session"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := cookieServer(c.cfg)
			body := strings.NewReader(url.Values{"password": {testUIPassword}}.Encode())
			var post *http.Request
			if c.https {
				post = httpsPage(http.MethodPost, loginPath, body)
			} else {
				post = loopbackPage(http.MethodPost, loginPath, body)
			}
			post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, post)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("sign-in answered %d: %s", w.Code, w.Body.String())
			}
			var session *http.Cookie
			for _, ck := range w.Result().Cookies() {
				if strings.HasSuffix(ck.Name, sessionCookieName) {
					session = ck
				}
			}
			if session == nil || session.Name != c.wantName {
				t.Fatalf("sign-in set %v, want a %s cookie", session, c.wantName)
			}
			get := httptest.NewRequest(http.MethodGet, post.URL.String(), nil)
			get.Host, get.TLS = post.Host, post.TLS
			get.AddCookie(&http.Cookie{Name: session.Name, Value: session.Value})
			if !s.hasValidSession(get) {
				t.Errorf("the next GET (no Origin, no Referer) is signed out: the name it reads is not %q", session.Name)
			}
		})
	}
}

// TestAPrefixedDeploymentRefusesALoopbackHTTPSignIn: on a prefixed deployment
// every sign-in and step-up POST from a loopback plain-http page is answered
// with the "sign in at MUSTER_PUBLIC_URL" page and NO Set-Cookie. Controls: the
// same sign-in over https signs in; the same loopback sign-in on the Compose
// deployment signs in (unchanged).
func TestAPrefixedDeploymentRefusesALoopbackHTTPSignIn(t *testing.T) {
	form := func() io.Reader { return strings.NewReader(url.Values{"password": {testUIPassword}}.Encode()) }
	do := func(s *Server, r *http.Request) *httptest.ResponseRecorder {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	paths := []string{loginPath, InvitePath, ResetPath, ClaimPath, passkeySignInVerifyPath, stepUpConfirmPath, stepUpConfirmPath + "/passkey/verify"}
	for _, mode := range []AuthMode{"", AuthModeGoTrue} {
		s := cookieServer(prefixedAuth(mode))
		for _, p := range paths {
			w := do(s, loopbackPage(http.MethodPost, p, form()))
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "This muster is set up for "+testPrefixedURL+". Sign in there.") {
				t.Errorf("mode %q, POST %s from a loopback http page: %d %q; want 403 and the pointer", mode, p, w.Code, w.Body.String())
			}
			if sc := w.Result().Header.Values("Set-Cookie"); len(sc) != 0 {
				t.Errorf("mode %q, POST %s refused but set %q", mode, p, sc)
			}
		}
	}
	if w := do(cookieServer(prefixedAuth("")), httpsPage(http.MethodPost, loginPath, form())); w.Code != http.StatusSeeOther ||
		!slices.ContainsFunc(w.Result().Cookies(), func(c *http.Cookie) bool { return c.Name == "__Host-muster_session" }) {
		t.Errorf("CONTROL: the same sign-in over https: %d %v; want 303 and a __Host-muster_session", w.Code, w.Result().Cookies())
	}
	if w := do(cookieServer(composeAuth("")), loopbackPage(http.MethodPost, loginPath, form())); w.Code != http.StatusSeeOther ||
		!slices.ContainsFunc(w.Result().Cookies(), func(c *http.Cookie) bool { return c.Name == "muster_session" }) {
		t.Errorf("CONTROL: the Compose deployment's loopback sign-in: %d %v; want 303 and a muster_session", w.Code, w.Result().Cookies())
	}
	// A GET of the sign-in page from the same loopback page is not refused.
	if w := do(cookieServer(prefixedAuth("")), loopbackPage(http.MethodGet, loginPath, nil)); w.Code != http.StatusOK {
		t.Errorf("GET /login from a loopback page answered %d, want 200 (only the POST is refused)", w.Code)
	}
}

// TestCookieLiteralsAndSignInCallersAreLedgered pins two relationships by
// parsing this package's source:
//
//   - every http.Cookie literal, by enclosing function, and that each one's
//     Name goes through cookieName — except expireLegacyCookies, which writes
//     the old names on purpose. A new literal fails here until it is named.
//   - every function that calls a sign-in cookie setter (setGoTrueCookies,
//     setPasswordSessionCookie). Each is reached through a cookieSignInPaths
//     route, or is a refresh of a cookie the request already carries; a new
//     one fails here until someone decides which, so a sign-in path cannot
//     escape the loopback refusal silently.
//
// Fails when either set grows OR shrinks.
func TestCookieLiteralsAndSignInCallersAreLedgered(t *testing.T) {
	wantLiterals := []string{
		"clearGoTrueCookies", "clearPushDeviceCookie", "expireLegacyCookies", "handleLogout",
		"setGoTrueCookies", "setKnownDeviceCookie", "setPasswordSessionCookie", "setPushDeviceCookie",
	}
	wantCallers := []string{
		"confirmGoTrue",             // POST /auth/confirm
		"confirmOperatorPassword",   // POST /auth/confirm
		"handleClaimSubmit",         // POST /auth/claim
		"handleGoTrueLoginPage",     // GET /login: refreshes a session the request carries
		"handleGoTrueLoginSubmit",   // POST /login
		"handleLoginSubmit",         // POST /login
		"handlePasskeySignInVerify", // POST /auth/passkey/verify
		"handleStepUpPasskeyVerify", // POST /auth/confirm/passkey/verify
		"requireGoTrueSession",      // every page: refreshes a session the request carries
		"signInAfterCutoff",         // POST /invite, /auth/reset
		"signInNewLogin",            // POST /invite, /auth/reset
	}
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	var literals, callers []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cookie" {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" {
							literals = append(literals, fn.Name.Name)
							if fn.Name.Name != "expireLegacyCookies" && !nameViaCookieName(x) {
								t.Errorf("%s: an http.Cookie whose Name does not go through s.auth.cookieName", fset.Position(x.Pos()))
							}
						}
					}
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "setGoTrueCookies" || sel.Sel.Name == "setPasswordSessionCookie") {
						callers = append(callers, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(literals)
	literals = slices.Compact(literals)
	sort.Strings(callers)
	callers = slices.Compact(callers)
	if !slices.Equal(literals, wantLiterals) {
		t.Errorf("functions holding an http.Cookie literal:\n got %q\nwant %q", literals, wantLiterals)
	}
	if !slices.Equal(callers, wantCallers) {
		t.Errorf("functions that set a sign-in cookie:\n got %q\nwant %q\n(a new one: route it through cookieSignInPaths, or argue it out here)", callers, wantCallers)
	}
}

// nameViaCookieName reports whether a Cookie literal's Name field is a call to
// a method named cookieName.
func nameViaCookieName(lit *ast.CompositeLit) bool {
	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Name" {
			continue
		}
		call, ok := kv.Value.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "cookieName"
	}
	return false
}
