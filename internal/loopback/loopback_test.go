package loopback

import (
	"net/netip"
	"testing"
)

// Private (RFC 1918) test hosts, each derived from its block's NAME so no test
// spells an address anybody's network might use (tests/leakscan.py).
var (
	testLAN10  = netip.MustParsePrefix("10.0.0.0/8").Addr().Next().String()
	testLAN192 = netip.MustParsePrefix("192.168.0.0/16").Addr().Next().String()
	testLAN172 = netip.MustParsePrefix("172.16.0.0/12").Addr().Next().String()
)

// TestHost pins exactly which hosts are this machine. Every refused case is
// one that differs from an accepted one in a single way (a suffix, a prefix,
// the network, the port's form), so a predicate that is merely "looks like
// localhost" or "is a private address" cannot pass.
func TestHost(t *testing.T) {
	accept := []string{
		"localhost", "LOCALHOST", "Localhost", "localhost:8105",
		"127.0.0.1", "127.0.0.1:8105", "127.1.2.3", "127.255.255.254:1",
		"::1", "[::1]", "[::1]:8105", "[0:0:0:0:0:0:0:1]:80",
		"::ffff:127.0.0.1", "[::ffff:127.0.0.1]:8105",
		" localhost ",
	}
	refuse := []string{
		"", " ", "localhost.evil.com", "localhost.evil.com:8105", "evil-localhost", "localhost.",
		"foo.localhost", "mylocalhost", "localhost:8105.evil.com",
		"127.0.0.1.evil.com", "127.0.0.1.nip.io", "0.0.0.0", "0.0.0.0:8105", "[::]", "::",
		testLAN10, testLAN10 + ":8105", testLAN192, testLAN192 + ":8105", testLAN172,
		"169.254.0.1", "100.64.0.1", "8.8.8.8",
		"[::2]", "fe80::1", "[fd00::1]:8105", "::ffff:" + testLAN10,
		"128.0.0.1", "126.255.255.255",
		"[localhost]", "[127.0.0.1]:x", "[127.0.0.1]", "localhost:", "localhost:99999", "localhost:80x", "[::1]8105", "[::1",
	}
	for _, h := range accept {
		if !Host(h) {
			t.Errorf("Host(%q) = false, want true (it is this machine)", h)
		}
	}
	for _, h := range refuse {
		if Host(h) {
			t.Errorf("Host(%q) = true, want false (it is not this machine)", h)
		}
	}
}

// TestHTTPURL: only plain http to a loopback host. https is not "loopback
// http" (callers accept it on its own terms), and nothing else is either.
func TestHTTPURL(t *testing.T) {
	accept := []string{
		"http://localhost", "http://localhost:8105", "http://localhost:8105/", "http://127.0.0.1:8105",
		"http://127.0.0.1:8105/muster", "http://[::1]:8105", "http://[::1]", "HTTP://LOCALHOST:8105",
		"  http://localhost:8105  ",
	}
	refuse := []string{
		"", "localhost:8105", "//localhost:8105", "https://localhost:8105", "ws://localhost:8105",
		"http://localhost.evil.com", "http://localhost.evil.com:8105", "http://127.0.0.1.evil.com",
		"http://" + testLAN10 + ":8105", "http://" + testLAN192 + ":8105", "http://muster.example.ts.net",
		"http://[fd00::1]:8105", "http://0.0.0.0:8105", "http://", "http:///path", "file:///etc/passwd",
		"http://%zz",
	}
	for _, u := range accept {
		if !HTTPURL(u) {
			t.Errorf("HTTPURL(%q) = false, want true", u)
		}
	}
	for _, u := range refuse {
		if HTTPURL(u) {
			t.Errorf("HTTPURL(%q) = true, want false", u)
		}
	}
}
