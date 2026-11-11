package previewgw

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// loopbackPeers trusts the test's own dials (httptest listens on loopback) as
// the relay in front of the gateway.
var loopbackPeers = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// From a TRUSTED peer (the relay) the listener's client-address header is the
// throttle's key: ten bad tokens under one address, then 429 on the 11th from
// that address, while a second address through the same peer is still 401.
func TestAClientAddressFromATrustedPeerIsTheThrottleKey(t *testing.T) {
	for _, tc := range []struct{ listener, header string }{
		{"mesh", "X-Real-Ip"},
		{"public", "Cf-Connecting-Ip"},
	} {
		t.Run(tc.listener, func(t *testing.T) {
			e := newGW(t, gwOpts{trusted: loopbackPeers})
			bad := wire.TokenPrefix + strings.Repeat("D", hostTokenLen-len(wire.TokenPrefix))
			client := func(ip string) http.Header { return http.Header{tc.header: {ip}} }
			for i := 1; i <= 10; i++ {
				_, resp, _ := e.dialHost(t, dialOpts{listener: tc.listener, token: bad, hostName: "alpha", header: client("203.0.113.7")})
				if statusOf(resp) != http.StatusUnauthorized {
					t.Fatalf("bad token #%d: HTTP %d; want 401", i, statusOf(resp))
				}
			}
			_, resp, _ := e.dialHost(t, dialOpts{listener: tc.listener, token: bad, hostName: "alpha", header: client("203.0.113.7")})
			if statusOf(resp) != http.StatusTooManyRequests {
				t.Fatalf("bad token #11 from one relayed client: HTTP %d; want 429", statusOf(resp))
			}
			_, resp, _ = e.dialHost(t, dialOpts{listener: tc.listener, token: bad, hostName: "alpha", header: client("203.0.113.8")})
			if statusOf(resp) != http.StatusUnauthorized {
				t.Fatalf("another relayed client's first bad token: HTTP %d; want 401 (keyed on the header, not the relay)", statusOf(resp))
			}
		})
	}
}

// peerOf's whole decision, one case per branch. Every peer, header value and
// prefix differs from every other, so a case can only pass on the value it is
// meant to produce.
func TestPeerOfBelievesTheHeaderOnlyFromATrustedPeer(t *testing.T) {
	trusted := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.40/32"),
		netip.MustParsePrefix("198.51.100.0/25"),
		netip.MustParsePrefix("2001:db8:aa::/48"),
	}
	long := strings.Repeat("9", 65)
	for _, tc := range []struct {
		name    string
		path    Path
		remote  string
		hdr     map[string]string
		trusted []netip.Prefix
		want    string
	}{
		{"untrusted peer, mesh header", PathMesh, "192.0.2.41:5000", map[string]string{"X-Real-IP": "203.0.113.1"}, trusted, "192.0.2.41"},
		{"untrusted peer, public header", PathPublic, "198.51.100.200:5001", map[string]string{"CF-Connecting-IP": "203.0.113.2"}, trusted, "198.51.100.200"},
		{"no trusted proxies at all", PathMesh, "192.0.2.40:5002", map[string]string{"X-Real-IP": "203.0.113.3"}, nil, "192.0.2.40"},
		{"trusted /32, mesh", PathMesh, "192.0.2.40:5003", map[string]string{"X-Real-IP": "203.0.113.4"}, trusted, "203.0.113.4"},
		{"trusted /25, public", PathPublic, "198.51.100.77:5004", map[string]string{"CF-Connecting-IP": "203.0.113.5"}, trusted, "203.0.113.5"},
		{"trusted v6", PathMesh, "[2001:db8:aa::9]:5005", map[string]string{"X-Real-IP": "203.0.113.6"}, trusted, "203.0.113.6"},
		{"trusted v4-mapped peer", PathMesh, "[::ffff:192.0.2.40]:5006", map[string]string{"X-Real-IP": "203.0.113.7"}, trusted, "203.0.113.7"},
		{"trusted, only the OTHER listener's header", PathPublic, "198.51.100.78:5007", map[string]string{"X-Real-IP": "203.0.113.8"}, trusted, "198.51.100.78"},
		{"trusted, no header", PathMesh, "198.51.100.79:5008", nil, trusted, "198.51.100.79"},
		{"trusted, header too long", PathMesh, "198.51.100.80:5009", map[string]string{"X-Real-IP": long}, trusted, "198.51.100.80"},
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		for k, v := range tc.hdr {
			r.Header.Set(k, v)
		}
		if got := peerOf(tc.path, r, tc.trusted); got != tc.want {
			t.Errorf("%s: peerOf = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// PREVIEW_TRUSTED_PROXIES: CIDRs or bare addresses, comma-separated; a bare
// address is exactly that address, a CIDR is masked to its network.
func TestTrustedProxiesParse(t *testing.T) {
	got, err := prefixes("PREVIEW_TRUSTED_PROXIES", " 192.0.2.3 , 198.51.100.6/24,,2001:db8::7,::ffff:203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	if want := []string{"192.0.2.3/32", "198.51.100.0/24", "2001:db8::7/128", "203.0.113.9/32"}; !slices.Equal(s, want) {
		t.Fatalf("parsed %v; want %v", s, want)
	}
	if got, err := prefixes("PREVIEW_TRUSTED_PROXIES", ""); err != nil || got != nil {
		t.Fatalf("empty: %v %v; want none", got, err)
	}
	for _, v := range []string{"relay", "192.0.2.0/33", "192.0.2.1/", "fe80::1%eth0", "::ffff:192.0.2.0/104", "192.0.2.256"} {
		if _, err := prefixes("PREVIEW_TRUSTED_PROXIES", v); err == nil {
			t.Errorf("%q: accepted", v)
		}
	}
}
