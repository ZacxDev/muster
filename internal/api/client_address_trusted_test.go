package api

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/clientaddr"
)

func trustedPolicy(t *testing.T, cidrs, header string) *clientaddr.Policy {
	t.Helper()
	p, err := clientaddr.New(clientaddr.Config{TrustedProxies: cidrs, Header: header})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestATrustedProxyIsBelievedTheSameOnEveryListener: with a trusted range and
// a header configured, the client a trusted proxy names is the address on the
// main listener and the browser listener alike (no listener is special any
// more), and a peer outside the range is its own address on both, whatever
// it sends.
func TestATrustedProxyIsBelievedTheSameOnEveryListener(t *testing.T) {
	var got string
	s := New(nil, AuthConfig{HookToken: pHookToken, UIPassword: pPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: principalAPITokens{}, ClientAddr: trustedPolicy(t, "192.0.2.0/24", "X-Forwarded-For")})
	s.principalProbe = func(r *http.Request) { got = clientIP(r); panic(errProbeAbort) }
	h := s.Handler()
	for _, l := range []struct {
		name string
		h    http.Handler
	}{{"main", h}, {"browser", BrowserListener(h)}} {
		for _, c := range []struct {
			peer, xff, want string
		}{
			{measuredProxyPeer, "203.0.113.7, 198.51.100.7", "198.51.100.7"},
			{measuredProxyPeer, "198.51.100.7, 192.0.2.3", "198.51.100.7"},
			{"203.0.113.66", "203.0.113.7, 198.51.100.7", "203.0.113.66"},
		} {
			got = ""
			serveForAddress(l.h, arriveFrom(c.peer), withHeaders(map[string]string{"X-Forwarded-For": c.xff, "CF-Connecting-IP": "203.0.113.6"}))
			if got != c.want {
				t.Errorf("%s listener, peer %s, X-Forwarded-For %q: clientIP = %q, want %q", l.name, c.peer, c.xff, got, c.want)
			}
		}
	}
}

// TestTwoClientsBehindATrustedProxyGetTheirOwnSignInBuckets: behind one
// trusted proxy (every request from its TCP peer), client A's failures fill
// A's per-address bucket and not B's — even when A's left-most
// X-Forwarded-For claims to be B. The twin below is the default: the same
// traffic with no proxy trusted shares ONE bucket, the proxy's.
func TestTwoClientsBehindATrustedProxyGetTheirOwnSignInBuckets(t *testing.T) {
	const a, b = "198.51.100.7", "198.51.100.8"
	run := func(t *testing.T, ext *Extensions) (aAfter, bWrong, bRight int) {
		f := newGoTrueFixtureWith(t, ext)
		f.member("b@example.com", 8, authz.RoleMember)
		viaProxy := func(xff string) reqOpt {
			return func(r *http.Request) {
				arriveFrom(measuredProxyPeer)(r)
				r.Header.Set("X-Forwarded-For", xff)
			}
		}
		for i := 0; i < loginFailureLimit; i++ {
			// A's own address appended by the proxy; B's typed in front.
			do(f.edge, "POST", "/login", url.Values{"email": {"nobody" + strconv.Itoa(i) + "@example.com"}, "password": {"x"}},
				viaProxy(b+", "+a))
		}
		aAfter = do(f.edge, "POST", "/login", url.Values{"email": {"nobody-more@example.com"}, "password": {"x"}}, viaProxy(a)).Code
		bWrong = do(f.edge, "POST", "/login", url.Values{"email": {"nobody-b@example.com"}, "password": {"x"}}, viaProxy(b)).Code
		bRight = do(f.edge, "POST", "/login", url.Values{"email": {"b@example.com"}, "password": {"correct horse battery"}}, viaProxy(b)).Code
		return aAfter, bWrong, bRight
	}
	t.Run("trusted proxy", func(t *testing.T) {
		aAfter, bWrong, bRight := run(t, &Extensions{ClientAddr: trustedPolicy(t, "192.0.2.0/24", "X-Forwarded-For")})
		if aAfter != http.StatusTooManyRequests {
			t.Fatalf("CONTROL: A after %d failures: %d, want 429 (A's bucket filled)", loginFailureLimit, aAfter)
		}
		if bWrong == http.StatusTooManyRequests || bRight != http.StatusSeeOther {
			t.Fatalf("B behind the same trusted proxy: a failure %d (want an ordinary refusal, not 429), its correct "+
				"sign-in %d (want 303) — A's failures must be A's", bWrong, bRight)
		}
	})
	t.Run("default: one shared bucket", func(t *testing.T) {
		aAfter, bWrong, _ := run(t, nil)
		if aAfter != http.StatusTooManyRequests || bWrong != http.StatusTooManyRequests {
			t.Fatalf("no proxy trusted: A %d, B %d — want both 429: every client is the proxy's address, one shared "+
				"bucket (the safe default; MUSTER_TRUSTED_PROXIES is how it is split)", aAfter, bWrong)
		}
	})
}
