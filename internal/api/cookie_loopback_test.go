package api

import (
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// Private (RFC 1918) test hosts, each derived from its block's NAME so no test
// spells an address anybody's network might use (tests/leakscan.py).
var (
	testLAN10  = netip.MustParsePrefix("10.0.0.0/8").Addr().Next().String()
	testLAN192 = netip.MustParsePrefix("192.168.0.0/16").Addr().Next().String()
	testLAN172 = netip.MustParsePrefix("172.16.0.0/12").Addr().Next().String()
)

// TestPasswordCookieSecureOnlyOffForALoopbackHTTPPage: with
// MUSTER_SECURE_COOKIES on, the session cookie a sign-in sets is Secure on
// every request EXCEPT a browser's from a page served over plain http on this
// machine's loopback interface (http://localhost, 127.0.0.0/8, [::1], the
// same host as the request's own). Every refused shape differs from an
// accepted one in one way, so a policy that drops Secure for any http (or any
// loopback Host, or any loopback Origin) fails here. With the flag off, the
// cookie is never Secure, whatever the request.
func TestPasswordCookieSecureOnlyOffForALoopbackHTTPPage(t *testing.T) {
	type req struct {
		host, origin, referer, fwdProto string
		tls                             bool
	}
	insecure := map[string]req{
		"localhost":             {host: "localhost:8105", origin: "http://localhost:8105"},
		"127.0.0.1":             {host: "127.0.0.1:8105", origin: "http://127.0.0.1:8105"},
		"[::1]":                 {host: "[::1]:8105", origin: "http://[::1]:8105"},
		"127.0.0.0/8, no port":  {host: "127.3.4.5", origin: "http://127.3.4.5"},
		"referer, no origin":    {host: "localhost:8105", referer: "http://localhost:8105/login?next=%2F"},
		"forwarded http":        {host: "localhost:8105", origin: "http://localhost:8105", fwdProto: "http"},
		"origin case-different": {host: "localhost:8105", origin: "http://LOCALHOST:8105"},
	}
	secure := map[string]req{
		"no origin, no referer (curl)":   {host: "localhost:8105"},
		"https origin, loopback host":    {host: "localhost:8105", origin: "https://muster.example.ts.net"},
		"proxy kept its host":            {host: "muster.example.ts.net", origin: "https://muster.example.ts.net"},
		"forwarded https":                {host: "localhost:8105", origin: "http://localhost:8105", fwdProto: "https"},
		"tls":                            {host: "localhost:8105", origin: "http://localhost:8105", tls: true},
		"lan host":                       {host: testLAN192 + ":8105", origin: "http://" + testLAN192 + ":8105"},
		"private 10.x":                   {host: testLAN10 + ":8105", origin: "http://" + testLAN10 + ":8105"},
		"localhost.evil.com":             {host: "localhost.evil.com:8105", origin: "http://localhost.evil.com:8105"},
		"loopback origin, other port":    {host: "localhost:8105", origin: "http://localhost:3000"},
		"loopback origin, lan host":      {host: testLAN192 + ":8105", origin: "http://localhost:8105"},
		"lan origin, loopback host":      {host: "localhost:8105", origin: "http://" + testLAN192 + ":8105"},
		"null origin":                    {host: "localhost:8105", origin: "null"},
		"https referer, loopback host":   {host: "localhost:8105", referer: "https://localhost:8105/login"},
		"evil referer, loopback host":    {host: "localhost:8105", referer: "http://localhost.evil.com:8105/"},
		"https origin with http referer": {host: "localhost:8105", origin: "https://muster.example.ts.net", referer: "http://localhost:8105/"},
	}
	signIn := func(t *testing.T, secureFlag bool, c req) *http.Cookie {
		t.Helper()
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, SecureCookies: secureFlag},
			log.New(io.Discard, "", 0))
		r := httptest.NewRequest(http.MethodPost, "http://"+c.host+loginPath,
			strings.NewReader(url.Values{"password": {testUIPassword}}.Encode()))
		r.Host = c.host
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.referer != "" {
			r.Header.Set("Referer", c.referer)
		}
		if c.fwdProto != "" {
			r.Header.Set("X-Forwarded-Proto", c.fwdProto)
		}
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("sign-in answered %d, want 303: %s", w.Code, w.Body.String())
		}
		for _, ck := range w.Result().Cookies() {
			if ck.Name == sessionCookieName {
				return ck
			}
		}
		t.Fatalf("sign-in set no %s cookie", sessionCookieName)
		return nil
	}
	for name, c := range insecure {
		if ck := signIn(t, true, c); ck.Secure {
			t.Errorf("%s: the session cookie is Secure; a loopback http page must get it without (WebKit drops it)", name)
		}
	}
	for name, c := range secure {
		if ck := signIn(t, true, c); !ck.Secure {
			t.Errorf("%s: the session cookie is NOT Secure, but this is not a loopback http page", name)
		}
	}
	// The flag off: never Secure (the exception only ever REMOVES the flag).
	for name, c := range secure {
		if ck := signIn(t, false, c); ck.Secure {
			t.Errorf("flag off, %s: the session cookie is Secure", name)
		}
	}
}

// TestPushDeviceCookieSecureInGoTrueMode (#707 round 0, F6; mutant M11): the
// push-device cookie, set on subscribe and cleared on unsubscribe or sign-out,
// is ALWAYS Secure in gotrue mode — even where a password-mode cookie drops it
// (a loopback http page, or MUSTER_SECURE_COOKIES off), because a gotrue
// session's own cookies are Secure. The fixture requests are exactly those on
// which passwordCookieSecure is false, so only the gotrue term can make the
// cookie Secure; the control (password mode, the same requests) shows it is
// then NOT Secure, and a request with no Origin in password mode with the flag
// on shows the set and the clear do follow the password-mode policy.
func TestPushDeviceCookieSecureInGoTrueMode(t *testing.T) {
	page := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://localhost:8105/api/push/subscribe", nil)
		r.Host = "localhost:8105"
		r.Header.Set("Origin", "http://localhost:8105")
		return r
	}
	curl := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://localhost:8105/api/push/subscribe", nil)
		r.Host = "localhost:8105"
		return r
	}
	cookie := func(t *testing.T, s *Server, r *http.Request, clear bool) *http.Cookie {
		t.Helper()
		w := httptest.NewRecorder()
		if clear {
			s.clearPushDeviceCookie(w, r)
		} else {
			s.setPushDeviceCookie(w, r, "device-hash")
		}
		for _, ck := range w.Result().Cookies() {
			if ck.Name == pushDeviceCookie {
				if clear != (ck.MaxAge < 0) {
					t.Fatalf("clear=%v: MaxAge %d", clear, ck.MaxAge)
				}
				return ck
			}
		}
		t.Fatalf("no %s cookie (clear=%v)", pushDeviceCookie, clear)
		return nil
	}
	server := func(mode AuthMode, secureFlag bool) *Server {
		return New(nil, AuthConfig{Mode: mode, UIPassword: testUIPassword, HookToken: testHookToken, SecureCookies: secureFlag},
			log.New(io.Discard, "", 0))
	}
	for _, secureFlag := range []bool{true, false} {
		for _, clear := range []bool{false, true} {
			if ck := cookie(t, server(AuthModeGoTrue, secureFlag), page(), clear); !ck.Secure {
				t.Errorf("gotrue, MUSTER_SECURE_COOKIES=%v, clear=%v, loopback http page: the push-device cookie is NOT Secure", secureFlag, clear)
			}
			if ck := cookie(t, server("", secureFlag), page(), clear); ck.Secure {
				t.Errorf("control: password mode, MUSTER_SECURE_COOKIES=%v, clear=%v, loopback http page: Secure (the fixture does not isolate the gotrue term)", secureFlag, clear)
			}
		}
	}
	for _, clear := range []bool{false, true} {
		if ck := cookie(t, server("", true), curl(), clear); !ck.Secure {
			t.Errorf("password mode, flag on, no Origin, clear=%v: NOT Secure", clear)
		}
	}
}
