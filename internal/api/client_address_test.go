package api

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// The client address under the DEFAULT configuration (no trusted proxy):
// the TCP peer on every listener, whatever a request's forwarding headers say.
// These tests need no setting, so they run unchanged against a build from
// before the trusted-proxy model, where the browser listener believed
// CF-Connecting-IP and the left-most X-Forwarded-For from any peer.
//
// The header sets are what each proxy was MEASURED to forward upstream
// (Caddy 2.11 reverse_proxy with its defaults; nginx 1.30 with a bare
// proxy_pass; nginx 1.30 with the common X-Real-IP / $proxy_add_x_forwarded_for
// recipe) when the client typed CF-Connecting-IP 203.0.113.6 and X-Forwarded-For
// 203.0.113.7, and, as a second row each, X-Forwarded-For alone. The real client
// is 198.51.100.7; the proxy is the TCP peer 192.0.2.5.
var measuredProxyHeaders = []struct {
	name string
	hdr  map[string]string
}{
	{"caddy/cf+xff", map[string]string{"Cf-Connecting-Ip": "203.0.113.6", "X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https"}},
	{"caddy/xff-only", map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https"}},
	{"nginx-bare/cf+xff", map[string]string{"CF-Connecting-IP": "203.0.113.6", "X-Forwarded-For": "203.0.113.7"}},
	{"nginx-bare/xff-only", map[string]string{"X-Forwarded-For": "203.0.113.7"}},
	{"nginx-recipe/cf+xff", map[string]string{"CF-Connecting-IP": "203.0.113.6", "X-Forwarded-For": "203.0.113.7, 198.51.100.7", "X-Real-IP": "198.51.100.7"}},
	{"nginx-recipe/xff-only", map[string]string{"X-Forwarded-For": "203.0.113.7, 198.51.100.7", "X-Real-IP": "198.51.100.7"}},
}

const measuredProxyPeer = "192.0.2.5"

// arriveFrom sets the request's TCP peer (unlike fromIP's history, this
// helper has always meant the peer, so it reads the same on every build).
func arriveFrom(ip string) reqOpt {
	return func(r *http.Request) { r.RemoteAddr = net.JoinHostPort(ip, "40000") }
}

func withHeaders(h map[string]string) reqOpt {
	return func(r *http.Request) {
		for k, v := range h {
			r.Header.Set(k, v)
		}
	}
}

// TestTheMeasuredProxyHeadersNeverChooseTheAddress: by default no measured
// header set moves the client address off the TCP peer — through the whole
// handler, on the main listener AND the browser listener (which used to
// believe the headers from anyone), and through clientIP on a request that
// never passed the handler's middleware.
func TestTheMeasuredProxyHeadersNeverChooseTheAddress(t *testing.T) {
	var got string
	s := New(nil, AuthConfig{HookToken: pHookToken, UIPassword: pPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: principalAPITokens{}})
	s.principalProbe = func(r *http.Request) { got = clientIP(r); panic(errProbeAbort) }
	h := s.Handler()
	listeners := []struct {
		name string
		h    http.Handler
	}{
		{"main", h},
		{"browser", BrowserListener(h)},
		{"no middleware", BrowserListener(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = clientIP(r) }))},
	}
	for _, l := range listeners {
		// Positive control: the harness reads the address at all, and a
		// different peer moves it.
		got = ""
		serveForAddress(l.h, arriveFrom("192.0.2.77"))
		if got != "192.0.2.77" {
			t.Fatalf("CONTROL %s: no headers, peer 192.0.2.77: clientIP = %q", l.name, got)
		}
		for _, m := range measuredProxyHeaders {
			got = ""
			serveForAddress(l.h, arriveFrom(measuredProxyPeer), withHeaders(m.hdr))
			if got != measuredProxyPeer {
				t.Errorf("%s listener, %s: clientIP = %q, want the TCP peer %s (no proxy is trusted by default)",
					l.name, m.name, got, measuredProxyPeer)
			}
		}
	}
}

func serveForAddress(h http.Handler, opts ...reqOpt) {
	r := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	r.Header.Set("X-Muster-Token", pHookToken)
	for _, o := range opts {
		o(r)
	}
	h.ServeHTTP(httptest.NewRecorder(), r)
}

// TestATypedAddressCannotLockAnotherClientOutOfSignIn: an attacker who reaches
// the browser listener directly and names a victim's address in every
// forwarding header spends only its OWN address's sign-in budget. The victim,
// at its own address, still signs in.
func TestATypedAddressCannotLockAnotherClientOutOfSignIn(t *testing.T) {
	f := newGoTrueFixture(t)
	f.member("victim@example.com", 7, authz.RoleMember)
	const victim, attacker = "198.51.100.23", "203.0.113.66"
	if w := do(f.edge, "POST", "/login", url.Values{"email": {"victim@example.com"}, "password": {"correct horse battery"}},
		arriveFrom(victim)); w.Code != http.StatusSeeOther {
		t.Fatalf("CONTROL: the victim's sign-in: %d", w.Code)
	}
	typed := withHeaders(map[string]string{"CF-Connecting-IP": victim, "X-Forwarded-For": victim, "X-Real-IP": victim})
	for i := 0; i < loginFailureLimit; i++ {
		do(f.edge, "POST", "/login", url.Values{"email": {"nobody" + strconv.Itoa(i) + "@example.com"}, "password": {"x"}},
			arriveFrom(attacker), typed)
	}
	if w := do(f.edge, "POST", "/login", url.Values{"email": {"victim@example.com"}, "password": {"correct horse battery"}},
		arriveFrom(victim)); w.Code != http.StatusSeeOther {
		t.Errorf("the victim's correct sign-in after %d failures that NAMED its address in every forwarding header: %d, want 303",
			loginFailureLimit, w.Code)
	}
	// The victim's address has spent nothing: a wrong password there is an
	// ordinary refusal, not the address limit (whatever that limit does to a
	// correct credential).
	if w := do(f.edge, "POST", "/login", url.Values{"email": {"nobody-else@example.com"}, "password": {"x"}},
		arriveFrom(victim)); w.Code == http.StatusTooManyRequests {
		t.Errorf("a failure from the victim's address after the attacker NAMED it %d times: 429 — the attacker spent the victim's bucket",
			loginFailureLimit)
	}
	// Positive control: the attacker's failures did fill a bucket — its own.
	if w := do(f.edge, "POST", "/login", url.Values{"email": {"nobody-more@example.com"}, "password": {"x"}},
		arriveFrom(attacker)); w.Code != http.StatusTooManyRequests {
		t.Errorf("CONTROL: the attacker's own address after %d failures: %d, want 429", loginFailureLimit, w.Code)
	}
}
