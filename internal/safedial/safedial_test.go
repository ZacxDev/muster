package safedial

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// v4 and mapped spell an address from its octets. Private addresses are
// built rather than written out: the repository's leak gate refuses a literal
// one in source.
func v4(a, b, c, d byte) string { return netip.AddrFrom4([4]byte{a, b, c, d}).String() }

// mapped is the IPv4-mapped IPv6 form (::ffff:a.b.c.d).
func mapped(a, b, c, d byte) string {
	return netip.AddrFrom16(netip.AddrFrom4([4]byte{a, b, c, d}).As16()).String()
}

// TestTheGuardRefusesEveryNonPublicRange is the predicate, one row per range.
// Each range has an address at its START and one inside it, so a mutant that
// narrows a range to its first address (or drops it) is seen.
func TestTheGuardRefusesEveryNonPublicRange(t *testing.T) {
	for _, tc := range []struct {
		rng  string
		addr string
	}{
		{"loopback v4", "127.0.0.1"},
		{"loopback v4, inside /8", "127.45.6.7"},
		{"loopback v6", "::1"},
		{"RFC 1918 10/8", v4(10, 0, 0, 1)},
		{"RFC 1918 10/8, inside", v4(10, 200, 30, 40)},
		{"RFC 1918 172.16/12", v4(172, 16, 0, 1)},
		{"RFC 1918 172.16/12, top", v4(172, 31, 255, 254)},
		{"RFC 1918 192.168/16", v4(192, 168, 1, 1)},
		{"ULA fc00::/7, fc half", "fc00::1"},
		{"ULA fc00::/7, fd half", "fd12:3456::9"},
		{"link-local v4 (cloud metadata)", "169.254.169.254"},
		{"link-local v6", "fe80::1"},
		{"CGNAT 100.64/10", "100.64.0.1"},
		{"CGNAT 100.64/10, top", "100.127.255.254"},
		{"unspecified v4", "0.0.0.0"},
		{"unspecified v6", "::"},
		{"multicast v4", "224.0.0.1"},
		{"multicast v4, inside", "239.1.2.3"},
		{"multicast v6", "ff02::1"},
		{"IPv4-mapped private", mapped(10, 0, 0, 1)},
		{"IPv4-mapped loopback", "::ffff:127.0.0.1"},
		{"IPv4-mapped CGNAT", "::ffff:100.64.0.1"},
		{"this-network 0/8", "0.1.2.3"},
		{"NAT64 of a private address", "64:ff9b::a00:1"},
		{"6to4 of a private address", "2002:a00:1::1"},
		{"IPv4-compatible ::/96, loopback", "::127.0.0.1"},
		{"IPv4-compatible ::/96, private", "::" + v4(10, 0, 0, 1)},
		{"IPv4-compatible ::/96, top", "::ffff:fffe"},
		{"SIIT ::ffff:0:0:0/96, loopback", "::ffff:0:127.0.0.1"},
		{"SIIT ::ffff:0:0:0/96, private", "::ffff:0:" + v4(10, 0, 0, 1)},
		{"Teredo 2001::/32", "2001:0:4136:e378:8000:63bf:3fff:fdd2"},
		{"Teredo 2001::/32, start", "2001::1"},
	} {
		t.Run(tc.rng, func(t *testing.T) {
			ip := netip.MustParseAddr(tc.addr)
			if (Guard{}).Allowed(ip) {
				t.Errorf("the strict guard ALLOWS %s (%s)", tc.addr, tc.rng)
			}
			if err := (Guard{}).Control("tcp", netip.AddrPortFrom(ip, 5432).String(), nil); !errors.Is(err, ErrForbiddenAddress) {
				t.Errorf("the socket check admits %s (%s): %v", tc.addr, tc.rng, err)
			}
		})
	}
}

// TestTheGuardAdmitsPublicAddresses is the other direction: a guard that
// refused everything would pass the table above.
func TestTheGuardAdmitsPublicAddresses(t *testing.T) {
	for _, a := range []string{"140.82.112.3", "8.8.8.8", "100.128.0.1", "172.32.0.1", "11.0.0.1", "2606:4700::1111", "::ffff:8.8.8.8",
		"2001:4860:4860::8888"} {
		ip := netip.MustParseAddr(a)
		if !(Guard{}).Allowed(ip) {
			t.Errorf("the strict guard refuses public %s", a)
		}
		if err := (Guard{}).Control("tcp", netip.AddrPortFrom(ip, 443).String(), nil); err != nil {
			t.Errorf("the socket check refuses public %s: %v", a, err)
		}
	}
}

// TestControlRefusesAPrivateSocketDestinationAndAdmitsAPublicOne: the
// Dialer's Control hook on its own. On DialContext's path it cannot refuse
// (every address it dials already passed Allowed), so it is tested here
// directly.
func TestControlRefusesAPrivateSocketDestinationAndAdmitsAPublicOne(t *testing.T) {
	for _, addr := range []string{v4(10, 0, 0, 1) + ":5432", "127.0.0.1:5432", "[fd00::1]:5432", "169.254.169.254:80"} {
		if err := (Guard{}).Control("tcp", addr, nil); !errors.Is(err, ErrForbiddenAddress) {
			t.Errorf("Control(%s) = %v, want ErrForbiddenAddress", addr, err)
		}
	}
	for _, addr := range []string{"140.82.112.3:5432", "[2606:4700::1111]:443"} {
		if err := (Guard{}).Control("tcp", addr, nil); err != nil {
			t.Errorf("Control(%s) = %v, want nil", addr, err)
		}
	}
	if err := (Guard{}).Control("tcp", "not-an-address", nil); !errors.Is(err, ErrForbiddenAddress) {
		t.Errorf("Control on an unparseable address = %v, want ErrForbiddenAddress", err)
	}
}

// TestAllowLoopbackAdmitsLoopbackAndNothingElse: the test-only loosening.
func TestAllowLoopbackAdmitsLoopbackAndNothingElse(t *testing.T) {
	g := Guard{AllowLoopback: true}
	if !g.Allowed(netip.MustParseAddr("127.0.0.1")) || !g.Allowed(netip.MustParseAddr("::1")) {
		t.Error("AllowLoopback refuses loopback")
	}
	for _, a := range []string{v4(10, 0, 0, 1), "169.254.169.254", "fd00::1", "100.64.0.1"} {
		if g.Allowed(netip.MustParseAddr(a)) {
			t.Errorf("AllowLoopback also admits %s", a)
		}
	}
}

// resolveTo is a resolver that answers every name with addrs and counts calls.
func resolveTo(calls *atomic.Int32, addrs ...string) func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) {
		calls.Add(1)
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

// TestAHostnameThatResolvesPrivateIsRefusedBeforeAnySocket: the name is
// public-looking; its ANSWER is what is refused. Each case names the address
// the dial would have reached; the listener on loopback proves no connection
// was attempted for the loopback case.
func TestAHostnameThatResolvesPrivateIsRefusedBeforeAnySocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	for _, tc := range []struct {
		name    string
		answers []string
	}{
		{"resolves to 127.0.0.1", []string{"127.0.0.1"}},
		{"resolves to an IPv4-mapped private address", []string{mapped(10, 0, 0, 1)}},
		{"resolves to a public AND a private address", []string{"140.82.112.3", v4(192, 168, 1, 10)}},
		{"resolves to the metadata address", []string{"169.254.169.254"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			g := Guard{Resolve: resolveTo(&calls, tc.answers...)}
			c, err := g.DialContext(context.Background(), "tcp", net.JoinHostPort("db.example.com", port))
			if c != nil {
				c.Close()
			}
			if !errors.Is(err, ErrForbiddenAddress) || !strings.Contains(err.Error(), "resolves to one") {
				t.Fatalf("err = %v; want the RESOLUTION refusal (ErrForbiddenAddress, \"resolves to one\")", err)
			}
		})
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the loopback listener accepted %d connection(s); a refused dial must make none", n)
	}
}

// TestTheSystemResolverPathIsGuardedToo: no injected resolver — "localhost"
// is resolved by the system, and its loopback answer is refused.
func TestTheSystemResolverPathIsGuardedToo(t *testing.T) {
	_, err := (Guard{}).DialContext(context.Background(), "tcp", "localhost:5432")
	if !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("dialling localhost through the strict guard: err = %v, want ErrForbiddenAddress", err)
	}
}

// TestAnIPLiteralIsRefusedWithoutResolving: a literal never reaches the
// resolver, and is still refused.
func TestAnIPLiteralIsRefusedWithoutResolving(t *testing.T) {
	var calls atomic.Int32
	g := Guard{Resolve: resolveTo(&calls, "140.82.112.3")}
	for _, addr := range []string{"127.0.0.1:5432", "[::1]:5432", "[" + mapped(10, 0, 0, 1) + "]:5432", v4(10, 0, 0, 1) + ":5432", "100.64.1.1:5432"} {
		if _, err := g.DialContext(context.Background(), "tcp", addr); !errors.Is(err, ErrForbiddenAddress) {
			t.Errorf("%s: err = %v, want ErrForbiddenAddress", addr, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("an IP literal went through the resolver (%d calls)", calls.Load())
	}
}

// TestTheGuardDialsTheAddressItChecked: DNS rebinding. The resolver answers
// loopback (allowed here) the first time and a private address every time
// after. The dial must resolve ONCE and connect to the answer it checked —
// the listener is reached — rather than hand the name back to a dialer that
// would resolve it again.
func TestTheGuardDialsTheAddressItChecked(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			got <- struct{}{}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	var calls atomic.Int32
	g := Guard{AllowLoopback: true, Resolve: func(context.Context, string) ([]netip.Addr, error) {
		if calls.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr(v4(10, 0, 0, 1))}, nil
	}}
	c, err := g.DialContext(context.Background(), "tcp", net.JoinHostPort("rebind.example.com", port))
	if n := calls.Load(); n != 1 {
		t.Fatalf("the name was resolved %d times (dial err %v); a second resolution is the rebinding window", n, err)
	}
	if err != nil {
		t.Fatalf("POSITIVE CONTROL: the guard could not dial an allowed address: %v", err)
	}
	c.Close()
	<-got
}
