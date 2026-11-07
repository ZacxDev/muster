// Package safedial is THE rule for "may muster-server open a connection to this
// address": one predicate over a resolved IP ([Guard.Allowed]) and one dialer
// that applies it at connect time ([Guard.DialContext]).
//
// One rule, one place. The HTTP connector proxy (internal/connectors, always
// on), the postgres connector and Web Push sends (both only with
// MUSTER_DIAL_DENY_PRIVATE=1) all dial through a Guard, so a range added here
// is refused by every one of them.
//
// 🔴 THE CHECK IS ON THE RESOLVED ADDRESS, NEVER ON THE NAME. DialContext
// resolves the host itself, refuses the dial if ANY answer is non-public, and
// connects to a checked ADDRESS rather than handing the name back to the
// system dialer. Checking a name's resolution and then dialling the NAME
// resolves twice, and a zone that answers public the first time and private
// the second (DNS rebinding) would get through. The Dialer's Control hook
// re-checks the socket's actual destination as a backstop for any path that
// reaches the socket without coming through the resolution check.
package safedial

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrForbiddenAddress is the dial refusal for a non-public destination.
var ErrForbiddenAddress = errors.New("refusing to connect to a non-public address")

// nonPublic are the ranges refused beyond what netip's own predicates
// (private, loopback, link-local, multicast, unspecified) cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // RFC 6598 shared address space (CGNAT, mesh overlays)
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: maps onto IPv4, private ranges included
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2002::/16"),      // 6to4: embeds an IPv4 address
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
	netip.MustParsePrefix("::ffff:0:0/96"),  // IPv4-mapped (unmapped before the check; belt and braces)
}

// DefaultDialTimeout bounds one connection attempt when Guard.Timeout is 0.
const DefaultDialTimeout = 15 * time.Second

// Guard is a dialer that connects only to public addresses.
//
// 🔴 THE ZERO VALUE IS THE STRICT ONE: the system resolver, and no
// non-public destination. AllowLoopback and Resolve LOOSEN or replace it and
// exist for tests.
type Guard struct {
	// AllowLoopback admits loopback destinations (127.0.0.0/8, ::1). Every
	// other non-public range stays refused.
	AllowLoopback bool
	// Resolve replaces the system resolver. nil means net.DefaultResolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Timeout bounds one connection attempt; 0 is DefaultDialTimeout.
	Timeout time.Duration
}

// Allowed reports whether g permits connecting to ip. An IPv4-mapped IPv6
// address is judged as the IPv4 address it carries.
func (g Guard) Allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() {
		return g.AllowLoopback
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, pfx := range nonPublic {
		if pfx.Contains(ip) {
			return false
		}
	}
	return true
}

func (g Guard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if g.Resolve != nil {
		return g.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// DialContext resolves addr's host, refuses the dial if ANY resolved address
// is non-public, and connects to a checked address (see the package comment).
// Its signature is net.Dialer.DialContext's, so it slots into an
// http.Transport or a pgx DialFunc unchanged.
func (g Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = g.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s resolved to no address", host)
	}
	for _, ip := range ips {
		if !g.Allowed(ip) {
			return nil, fmt.Errorf("%w (%s resolves to one)", ErrForbiddenAddress, host)
		}
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	d := &net.Dialer{Timeout: timeout, Control: g.Control}
	var lastErr error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Control is a net.Dialer Control hook: the last-moment check on the address
// a socket is about to connect to ("ip:port").
func (g Guard) Control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !g.Allowed(ap.Addr()) {
		return fmt.Errorf("%w (socket destination)", ErrForbiddenAddress)
	}
	return nil
}

// Transport is an http.Transport whose every connection is dialled through g.
//
// 🔴 Proxy IS nil, DELIBERATELY: with an HTTP(S)_PROXY in the environment the
// guard would be checking the PROXY's address rather than the destination's,
// and a request to a private destination would be handed to the proxy to make.
func (g Guard) Transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           g.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
