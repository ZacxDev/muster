package clientaddr

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The header sets below are what each proxy was MEASURED to forward upstream
// (Caddy 2.11 `reverse_proxy` with its defaults; nginx 1.30 with a bare
// `proxy_pass`; nginx 1.30 with the common `X-Real-IP $remote_addr` /
// `X-Forwarded-For $proxy_add_x_forwarded_for` recipe) when the CLIENT sent
// `CF-Connecting-IP: 203.0.113.6` and `X-Forwarded-For: 203.0.113.7`. The Cloudflare row
// is from Cloudflare's documentation (CF-Connecting-IP overwritten, X-Forwarded-For
// appended). The real client is 198.51.100.7; the proxy is the TCP peer
// 192.0.2.5. 203.0.113.6 and 203.0.113.7 are what an attacker typed: no row may ever
// resolve to either.
const (
	realClient = "198.51.100.7"
	proxyPeer  = "192.0.2.5:443"
	proxyAddr  = "192.0.2.5"
	typedCF    = "203.0.113.6"
	typedXFF   = "203.0.113.7"
	// attackerPeer reaches muster directly, past every proxy.
	attackerPeer = "203.0.113.99:5000"
	attackerAddr = "203.0.113.99"
)

var measured = []struct {
	name string
	hdr  map[string]string
}{
	{"caddy-default", map[string]string{"Cf-Connecting-Ip": typedCF, "X-Forwarded-For": realClient, "X-Forwarded-Proto": "https"}},
	{"nginx-bare", map[string]string{"CF-Connecting-IP": typedCF, "X-Forwarded-For": typedXFF}},
	{"nginx-recipe", map[string]string{"CF-Connecting-IP": typedCF, "X-Forwarded-For": typedXFF + ", " + realClient, "X-Real-IP": realClient}},
	{"cloudflare", map[string]string{"CF-Connecting-IP": realClient, "X-Forwarded-For": typedXFF + ", " + realClient}},
}

func req(peer string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = peer
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func mustNew(t *testing.T, trusted, header string) *Policy {
	t.Helper()
	p, err := New(Config{TrustedProxies: trusted, Header: header, TrustedVar: "T", HeaderVar: "H"})
	if err != nil {
		t.Fatalf("New(%q, %q): %v", trusted, header, err)
	}
	return p
}

// TestTheStrictDefaultIsTheTCPPeerBehindEveryMeasuredProxy: with nothing
// configured (the default), no measured header set moves the address off the
// TCP peer — not the typed CF-Connecting-IP every proxy passes through, not the
// left-most X-Forwarded-For.
func TestTheStrictDefaultIsTheTCPPeerBehindEveryMeasuredProxy(t *testing.T) {
	for _, p := range []*Policy{nil, mustNew(t, "", "")} {
		for _, m := range measured {
			if got := p.Resolve(req(proxyPeer, m.hdr)); got != proxyAddr {
				t.Errorf("%s (policy %v): Resolve = %q, want the TCP peer %s", m.name, p != nil, got, proxyAddr)
			}
		}
	}
}

// TestATrustedProxyNamesTheClient is the matrix: each measured proxy, behind a
// policy naming the header that proxy writes reliably, from the proxy (trusted)
// and from an attacker who reached muster directly with the same headers
// (untrusted).
func TestATrustedProxyNamesTheClient(t *testing.T) {
	type row struct {
		proxy, header string
		want          string // from the trusted proxy
	}
	rows := []row{
		{"caddy-default", HeaderXForwardedFor, realClient}, // Caddy replaces X-Forwarded-For
		{"nginx-recipe", HeaderXRealIP, realClient},        // overwritten by $remote_addr
		{"nginx-recipe", HeaderXForwardedFor, realClient},  // appended: right-most untrusted
		{"cloudflare", HeaderCFConnectingIP, realClient},   // overwritten by Cloudflare
		{"cloudflare", HeaderXForwardedFor, realClient},    // appended
		// A proxy that does not write the configured header: the proxy itself,
		// never a value the client typed.
		{"nginx-bare", HeaderXRealIP, proxyAddr},
		{"caddy-default", HeaderXRealIP, proxyAddr},
	}
	byName := map[string]map[string]string{}
	for _, m := range measured {
		byName[m.name] = m.hdr
	}
	for _, rw := range rows {
		p := mustNew(t, "192.0.2.0/24", rw.header)
		hdr := byName[rw.proxy]
		if got := p.Resolve(req(proxyPeer, hdr)); got != rw.want {
			t.Errorf("%s / %s from the trusted proxy: Resolve = %q, want %q", rw.proxy, rw.header, got, rw.want)
		}
		if got := p.Resolve(req(attackerPeer, hdr)); got != attackerAddr {
			t.Errorf("%s / %s from an untrusted peer: Resolve = %q, want that peer %s", rw.proxy, rw.header, got, attackerAddr)
		}
	}
}

func TestXForwardedForIsWalkedFromTheRight(t *testing.T) {
	p := mustNew(t, "192.0.2.0/24, 2001:db8:ff::/48", HeaderXForwardedFor)
	for _, c := range []struct {
		name string
		xff  []string // one header line each
		want string
	}{
		{"one hop", []string{realClient}, realClient},
		{"typed left-most is skipped", []string{"203.0.113.6, " + realClient}, realClient},
		{"trusted hops are skipped", []string{"203.0.113.6, " + realClient + ", 192.0.2.4, 192.0.2.3"}, realClient},
		{"across header lines, in order", []string{"203.0.113.6", realClient + ", 192.0.2.3"}, realClient},
		{"every hop trusted: the left-most", []string{"192.0.2.9, 192.0.2.4"}, "192.0.2.9"},
		{"a broken hop ends the chain at the proxy", []string{realClient + ", not-an-ip, 192.0.2.3"}, proxyAddr},
		{"an empty hop ends the chain", []string{realClient + ", , 192.0.2.3"}, proxyAddr},
		{"with a port", []string{"198.51.100.7:4711"}, realClient},
		{"ipv6", []string{"2001:db8::7"}, "2001:db8::7"},
		{"ipv6 bracketed with a port", []string{"[2001:db8::7]:4711, [2001:db8:ff::1]"}, "2001:db8::7"},
		{"ipv4-mapped is unmapped", []string{"::ffff:198.51.100.7"}, realClient},
		{"unspecified is refused", []string{"0.0.0.0"}, proxyAddr},
		{"absent", nil, proxyAddr},
	} {
		r := req(proxyPeer, nil)
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := p.Resolve(r); got != c.want {
			t.Errorf("%s %q: Resolve = %q, want %q", c.name, c.xff, got, c.want)
		}
	}
}

func TestSingleAddressHeadersAreValidated(t *testing.T) {
	p := mustNew(t, "192.0.2.5", HeaderCFConnectingIP)
	for _, c := range []struct {
		name string
		vals []string
		want string
	}{
		{"valid", []string{realClient}, realClient},
		{"spaces trimmed", []string{"  " + realClient + " "}, realClient},
		{"not an address", []string{"<script>"}, proxyAddr},
		{"a list is not one address", []string{"203.0.113.6, " + realClient}, proxyAddr},
		{"two lines are ambiguous", []string{"203.0.113.6", realClient}, proxyAddr},
		{"over-long", []string{strings.Repeat("1", 65)}, proxyAddr},
		{"absent", nil, proxyAddr},
	} {
		r := req(proxyPeer, nil)
		for _, v := range c.vals {
			r.Header.Add("CF-Connecting-IP", v)
		}
		if got := p.Resolve(r); got != c.want {
			t.Errorf("%s %q: Resolve = %q, want %q", c.name, c.vals, got, c.want)
		}
	}
	// The bare address is a /32: its neighbour is not trusted.
	if got := p.Resolve(req("192.0.2.6:1", map[string]string{"CF-Connecting-IP": realClient})); got != "192.0.2.6" {
		t.Errorf("a neighbour of the trusted /32: Resolve = %q, want the peer", got)
	}
}

func TestPeerForms(t *testing.T) {
	p := mustNew(t, "192.0.2.0/24, ::1", HeaderXRealIP)
	for _, c := range []struct{ remote, want string }{
		{"[::ffff:192.0.2.5]:80", realClient}, // a v4-mapped peer matches the IPv4 range
		{"[::1]:80", realClient},
		{"[fe80::1%eth0]:80", "fe80::1"},
		{"garbage", Unknown},
	} {
		r := req(c.remote, map[string]string{"X-Real-IP": realClient})
		if got := p.Resolve(r); got != c.want {
			t.Errorf("peer %q: Resolve = %q, want %q", c.remote, got, c.want)
		}
	}
}

func TestNewRefusesASettingThatWouldNotDoWhatItSays(t *testing.T) {
	for _, c := range []struct{ trusted, header, want string }{
		{"192.0.2.0/24", "", "T is set but H is not"},
		{"", "X-Forwarded-For", "T is empty"},
		{"192.0.2.0/24", "Forwarded", "want CF-Connecting-IP, X-Forwarded-For or X-Real-IP"},
		{"192.0.2.0/24", "True-Client-IP", "want CF-Connecting-IP"},
		{"10.0.0/8", "X-Real-IP", "not a CIDR range"},
		{"example.com", "X-Real-IP", "not an IP address or CIDR range"},
		{"0.0.0.0/0", "X-Real-IP", "a /0 range"},
		{"::/0", "X-Real-IP", "a /0 range"},
		{"::ffff:192.0.2.0/120", "X-Real-IP", "IPv4-mapped"},
	} {
		_, err := New(Config{TrustedProxies: c.trusted, Header: c.header, TrustedVar: "T", HeaderVar: "H"})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("New(%q, %q) = %v, want an error containing %q", c.trusted, c.header, err, c.want)
		}
	}
	// Accepted: the header in any case; ranges written loosely.
	p := mustNew(t, " 192.0.2.77/24 ,, 2001:db8::/32 ", "x-forwarded-FOR")
	if p.Header() != HeaderXForwardedFor {
		t.Errorf("Header() = %q", p.Header())
	}
	if got := strings.Join(p.Trusted(), ","); got != "192.0.2.0/24,2001:db8::/32" {
		t.Errorf("Trusted() = %q (want masked, empty entries dropped)", got)
	}
}

func TestDescribeNamesTheMode(t *testing.T) {
	strict := mustNew(t, "", "").Describe()
	if !strings.HasPrefix(strict, "client address: STRICT") || !strings.Contains(strict, "T and H are unset") {
		t.Errorf("strict Describe = %q", strict)
	}
	if got := (*Policy)(nil).Describe(); !strings.HasPrefix(got, "client address: STRICT") {
		t.Errorf("nil Describe = %q", got)
	}
	tr := mustNew(t, "192.0.2.0/24", "x-forwarded-for").Describe()
	for _, want := range []string{"client address: TRUSTED PROXIES", "192.0.2.0/24", "X-Forwarded-For", "right-most"} {
		if !strings.Contains(tr, want) {
			t.Errorf("trusted Describe = %q, want %q in it", tr, want)
		}
	}
}

// TestForwardingFromAnUntrustedPeerIsLoggedOnceAMinute: the warning names the
// headers and the peer (never a header's value), is rate-limited, and counts
// what it suppressed. A request with no forwarding header logs nothing.
func TestForwardingFromAnUntrustedPeerIsLoggedOnceAMinute(t *testing.T) {
	var lines []string
	p := mustNew(t, "", "").WithLog(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	now := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return now }

	p.Resolve(req(attackerPeer, nil))
	if len(lines) != 0 {
		t.Fatalf("no forwarding header: logged %q", lines)
	}
	hdr := map[string]string{"X-Forwarded-For": typedXFF, "CF-Connecting-IP": typedCF}
	p.Resolve(req(attackerPeer, hdr))
	if len(lines) != 1 {
		t.Fatalf("first forwarding header: %d lines, want 1", len(lines))
	}
	if l := lines[0]; !strings.Contains(l, "CF-Connecting-IP, X-Forwarded-For") || !strings.Contains(l, attackerAddr) ||
		strings.Contains(l, typedXFF) || strings.Contains(l, typedCF) {
		t.Errorf("warning = %q: want the header names and the peer, never a value", l)
	}
	for range 5 {
		p.Resolve(req(attackerPeer, hdr))
	}
	if len(lines) != 1 {
		t.Fatalf("within the minute: %d lines, want still 1", len(lines))
	}
	now = now.Add(time.Minute)
	p.Resolve(req(attackerPeer, hdr))
	if len(lines) != 2 || !strings.Contains(lines[1], "5 more like it") {
		t.Fatalf("after the minute: %q, want a second line counting the 5 suppressed", lines)
	}
}

func TestATrustedProxyWithoutTheHeaderIsLogged(t *testing.T) {
	var lines []string
	p := mustNew(t, "192.0.2.0/24", HeaderCFConnectingIP).WithLog(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	if got := p.Resolve(req(proxyPeer, map[string]string{"CF-Connecting-IP": realClient})); got != realClient || len(lines) != 0 {
		t.Fatalf("header present: %q, logged %q", got, lines)
	}
	// A trusted peer that sends no forwarding header at all (a health probe
	// from a node address) is its own client and logs nothing, however often.
	for range 3 {
		if got := p.Resolve(req(proxyPeer, nil)); got != proxyAddr || len(lines) != 0 {
			t.Fatalf("no forwarding header from the trusted peer: %q, logged %q", got, lines)
		}
	}
	// The wrong header, or the right one unusable: logged, naming what came.
	if got := p.Resolve(req(proxyPeer, map[string]string{"X-Forwarded-For": realClient})); got != proxyAddr || len(lines) != 1 ||
		!strings.Contains(lines[0], "sent X-Forwarded-For but no usable CF-Connecting-IP") {
		t.Fatalf("the wrong header: %q, logged %q", got, lines)
	}
}

// TestOfIsTheMiddlewaresAnswerAndOtherwiseThePeer: Of returns what Middleware
// resolved; a request that never passed it gets the peer, whatever it says.
func TestOfIsTheMiddlewaresAnswerAndOtherwiseThePeer(t *testing.T) {
	hdr := map[string]string{"CF-Connecting-IP": realClient}
	if got := Of(req(proxyPeer, hdr)); got != proxyAddr {
		t.Errorf("no middleware: Of = %q, want the peer", got)
	}
	var got string
	h := mustNew(t, "192.0.2.0/24", HeaderCFConnectingIP).Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = Of(r)
	}))
	h.ServeHTTP(httptest.NewRecorder(), req(proxyPeer, hdr))
	if got != realClient {
		t.Errorf("through the middleware: Of = %q, want %q", got, realClient)
	}
	(*Policy)(nil).Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = Of(r) })).
		ServeHTTP(httptest.NewRecorder(), req(proxyPeer, hdr))
	if got != proxyAddr {
		t.Errorf("through a nil policy's middleware: Of = %q, want the peer", got)
	}
}
