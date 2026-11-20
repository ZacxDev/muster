package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// testNet2 is the nth address of TEST-NET-2, derived from the block's name so
// no test spells an address (tests/leakscan.py).
func testNet2(n int) string {
	a := netip.MustParsePrefix("198.51.100.0/24").Addr()
	for i := 0; i < n; i++ {
		a = a.Next()
	}
	return a.String()
}

// The READER half of the `__Host-` rename, on a prefixed deployment: each
// cookie is written and then read back under the new name, and the old name
// alone is not read. A reader left on the unprefixed name is a sign-in loop
// (set as __Host-x, read as x), which the writer goldens cannot see.

// TestGoTrueCookiesAreWrittenAndReadPrefixed: a gotrue deployment with an
// https public URL (prefixed whatever MUSTER_SECURE_COOKIES says: the fixture
// leaves it off, as a gotrue deployment may) signs in under __Host- names, an
// authenticated request with those names is served, an expired access token is
// refreshed from the __Host- refresh cookie, and the old names alone sign
// nobody in.
func TestGoTrueCookiesAreWrittenAndReadPrefixed(t *testing.T) {
	f := newGoTrueFixture(t)
	if f.s.auth.SecureCookies || !f.s.auth.HostPrefixedCookies() {
		t.Fatalf("fixture: SecureCookies=%v prefixed=%v; want a gotrue deployment with the flag OFF that is prefixed",
			f.s.auth.SecureCookies, f.s.auth.HostPrefixedCookies())
	}
	sub := f.member("m@example.com", 7, authz.RoleMember)

	in := do(f.edge, "POST", "/login", url.Values{"email": {"m@example.com"}, "password": {"correct horse battery"}})
	raw := map[string]string{}
	for _, c := range in.Result().Cookies() {
		raw[c.Name] = c.Value
	}
	at, rt := raw["__Host-muster_at"], raw["__Host-muster_rt"]
	if in.Code != http.StatusSeeOther || at == "" || rt == "" {
		t.Fatalf("sign-in: %d, cookies %v; want 303 and __Host-muster_at/__Host-muster_rt", in.Code, raw)
	}
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt)); w.Code != http.StatusOK {
		t.Fatalf("an authenticated request under the __Host- names: %d", w.Code)
	}
	if w := do(f.edge, "GET", "/account", nil, withLegacyCookies(at, rt), asXHR); w.Code != http.StatusUnauthorized {
		t.Errorf("the same tokens under the OLD names: %d, want 401 (not read)", w.Code)
	}

	// Refresh: the access token is past its expiry; only the refresh cookie
	// can recover the session, so this reads __Host-muster_rt.
	f.fake.SetAccessTTL(60 * time.Second)
	at2, rt2, _ := f.fake.Mint(sub)
	f.fake.Advance(95 * time.Second)
	w := do(f.edge, "GET", "/account", nil, withCookies(at2, rt2))
	if w.Code != http.StatusOK {
		t.Fatalf("expired access + __Host- refresh: %d", w.Code)
	}
	var names []string
	for _, c := range w.Result().Cookies() {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "__Host-muster_at,__Host-muster_rt" {
		t.Errorf("the refresh set %q, want __Host-muster_at,__Host-muster_rt", got)
	}
}

// signInPrefixed signs in to a prefixed password server over https and
// returns the cookies it set, by name.
func signInPrefixed(t *testing.T, s *Server, ip string) map[string]*http.Cookie {
	t.Helper()
	r := httpsPage(http.MethodPost, loginPath, strings.NewReader(url.Values{"password": {testUIPassword}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("sign-in: %d", w.Code)
	}
	out := map[string]*http.Cookie{}
	for _, c := range w.Result().Cookies() {
		out[c.Name] = c
	}
	return out
}

// postLogin posts a password from muster's own https page at ip, carrying cks.
func postLogin(s *Server, ip, password, origin string, cks ...*http.Cookie) int {
	r := httpsPage(http.MethodPost, loginPath, strings.NewReader(url.Values{"password": {password}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin == "" {
		r.Header.Del("Origin")
	} else {
		r.Header.Set("Origin", origin)
	}
	r.RemoteAddr = ip + ":40000"
	for _, c := range cks {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code
}

// trip spends ip's wrong-password budget.
func trip(t *testing.T, s *Server, ip string) {
	t.Helper()
	for i := 0; i <= loginRateBurst; i++ {
		postLogin(s, ip, "wrong", testPrefixedURL)
	}
	if code := postLogin(s, ip, testUIPassword, testPrefixedURL); code != http.StatusTooManyRequests {
		t.Fatalf("CONTROL: an unknown device past the limit answered %d, want 429", code)
	}
}

// TestTheKnownDeviceCookieIsReadPrefixed: on a prefixed deployment the device
// cookie a sign-in sets (__Host-muster_device) is what exempts that browser
// from a tripped limit; the same value under the old name does not.
func TestTheKnownDeviceCookieIsReadPrefixed(t *testing.T) {
	s := cookieServer(prefixedAuth(""))
	ip := testNet2(7)
	dev := signInPrefixed(t, s, ip)["__Host-muster_device"]
	if dev == nil {
		t.Fatal("sign-in set no __Host-muster_device")
	}
	trip(t, s, ip)
	if code := postLogin(s, ip, testUIPassword, testPrefixedURL, dev); code != http.StatusSeeOther {
		t.Errorf("the known device under its __Host- name: %d, want 303", code)
	}
	if code := postLogin(s, ip, testUIPassword, testPrefixedURL, &http.Cookie{Name: knownDeviceCookieName, Value: dev.Value}); code != http.StatusTooManyRequests {
		t.Errorf("the device cookie under its OLD name: %d, want 429 (not read)", code)
	}
}

// TestTheThrottleExemptionNeedsTheOwnOrigin: a known device, or a signed-in
// browser, is exempt from a tripped limit only on a request from muster's own
// origin. The same cookies on a POST from a same-site sibling, or with no
// Origin, are refused like an unknown device: SameSite=Lax would otherwise let
// a sibling page guess passwords unthrottled. The own-origin case is the
// positive control.
func TestTheThrottleExemptionNeedsTheOwnOrigin(t *testing.T) {
	s := cookieServer(prefixedAuth(""))
	ip := testNet2(8)
	ck := signInPrefixed(t, s, ip)
	dev, sess := ck["__Host-muster_device"], ck["__Host-muster_session"]
	if dev == nil || sess == nil {
		t.Fatalf("sign-in cookies: %v", ck)
	}
	trip(t, s, ip)
	for name, cks := range map[string][]*http.Cookie{"device": {dev}, "session": {sess}} {
		if code := postLogin(s, ip, testUIPassword, testPrefixedURL, cks...); code != http.StatusSeeOther {
			t.Errorf("%s, own origin: %d, want 303 (exempt)", name, code)
		}
		for _, origin := range []string{"https://meet.example.test", ""} {
			if code := postLogin(s, ip, testUIPassword, origin, cks...); code != http.StatusTooManyRequests {
				t.Errorf("%s, Origin %q: %d, want 429 (not exempt off muster's own origin)", name, origin, code)
			}
		}
	}
}

// TestThePushDeviceCookieIsReadPrefixed: sign-out forgets the browser's push
// subscription named by its device cookie, and clears that cookie. On a
// prefixed deployment it reads __Host-muster_push_device (and so clears it);
// the old name alone is not read (nothing is cleared).
func TestThePushDeviceCookieIsReadPrefixed(t *testing.T) {
	s := cookieServer(prefixedAuth(""))
	forget := func(name string) []string {
		r := httpsPage(http.MethodPost, "/logout", nil)
		r.AddCookie(&http.Cookie{Name: name, Value: "device-hash"})
		w := httptest.NewRecorder()
		s.forgetPushDevice(w, r, 0)
		return setCookieLines(w)
	}
	if got := forget("__Host-muster_push_device"); len(got) != 1 || got[0] != "__Host-muster_push_device=V; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax" {
		t.Errorf("the __Host- push-device cookie: Set-Cookie %q, want it read and cleared", got)
	}
	if got := forget(pushDeviceCookie); len(got) != 0 {
		t.Errorf("the OLD push-device name: Set-Cookie %q, want nothing (not read)", got)
	}
}

// TestStepUpReadsThePrefixedSession: the step-up confirm (password mode) is
// behind requireSession, so on a prefixed deployment it is reached with the
// __Host- session and the confirm re-issues it under that name; with the old
// name it is signed out.
func TestStepUpReadsThePrefixedSession(t *testing.T) {
	s := cookieServer(prefixedAuth(""))
	ck := signInPrefixed(t, s, testNet2(9))
	confirm := func(cks ...*http.Cookie) *httptest.ResponseRecorder {
		r := httpsPage(http.MethodPost, stepUpConfirmPath, strings.NewReader(url.Values{"password": {testUIPassword}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		for _, c := range cks {
			r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	w := confirm(ck["__Host-muster_session"], ck["__Host-muster_device"])
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusNotFound {
		t.Fatalf("step-up with the __Host- session: %d %s", w.Code, w.Body.String())
	}
	reissued := false
	for _, c := range w.Result().Cookies() {
		reissued = reissued || (c.Name == "__Host-muster_session" && c.MaxAge > 0)
	}
	if !reissued {
		t.Errorf("step-up confirm (%d) did not re-issue __Host-muster_session: %v", w.Code, w.Result().Cookies())
	}
	old := ck["__Host-muster_session"]
	if w := confirm(&http.Cookie{Name: sessionCookieName, Value: old.Value}); w.Code != http.StatusUnauthorized {
		t.Errorf("step-up with the OLD session name: %d, want 401 (signed out)", w.Code)
	}
}

// TestComposeWithATailscaleURLPointsTheDesktopAtIt: the quickstart's Compose
// stack (MUSTER_SECURE_COOKIES=1) given an https Tailscale address is a
// prefixed deployment, so the desktop's sign-in at http://localhost:8105 gets
// the refusal page — with a link to the https address's sign-in page — and no
// cookie. Its phone, at the https address, signs in.
func TestComposeWithATailscaleURLPointsTheDesktopAtIt(t *testing.T) {
	const public = "https://box.tailnet-example.ts.net"
	s := cookieServer(AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, SecureCookies: true, PublicURL: public})
	r := loopbackPage(http.MethodPost, loginPath, strings.NewReader(url.Values{"password": {testUIPassword}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(w.Result().Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("desktop sign-in at localhost: %d, Set-Cookie %q; want 403 and none", w.Code, w.Result().Header.Values("Set-Cookie"))
	}
	if want := `href="` + public + `/login"`; !strings.Contains(w.Body.String(), want) {
		t.Errorf("the refusal page has no link %s:\n%s", want, w.Body.String())
	}
	p := httptest.NewRequest(http.MethodPost, public+loginPath, strings.NewReader(url.Values{"password": {testUIPassword}}.Encode()))
	p.Host = "box.tailnet-example.ts.net"
	p.Header.Set("Origin", public)
	p.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	p.Header.Set("X-Forwarded-Proto", "https") // tailscale serve terminates TLS in front of the plain port
	pw := httptest.NewRecorder()
	s.Handler().ServeHTTP(pw, p)
	if pw.Code != http.StatusSeeOther || pw.Result().Cookies()[0].Name != "__Host-muster_session" {
		t.Errorf("CONTROL: the phone's sign-in at the https address: %d %v", pw.Code, pw.Result().Cookies())
	}
}
