// Package loopback is THE rule for "this address is this machine": the one
// predicate every part of muster uses to treat plain http to the loopback
// interface as safe to use, and nothing else.
//
// A request to http://localhost, http://127.0.0.1 (any of 127.0.0.0/8) or
// http://[::1] never leaves the machine, so a token sent over it is not in
// cleartext on any network, and browsers already treat such an origin as a
// secure context. Everything else that is plain http — a LAN address, a
// private range, a hostname that merely STARTS with "localhost" — is not this
// machine, and every caller keeps refusing it.
//
// One rule, one place: the mirror's URL checks (internal/mirrorhost,
// cmd/muster-mirror), the CLI's doctor (cmd/muster) and the server's cookie
// policy (internal/api) all ask this package, so they cannot disagree about
// which addresses count.
//
// 🔴 NAMES ARE NOT RESOLVED. "localhost" is accepted by name (RFC 6761 reserves
// it for the loopback interface, and browsers treat it so); no other name is,
// however it resolves — not "localhost.", not "foo.localhost", not
// "localhost.example.com". An IP literal counts only when it IS a loopback
// address (an IPv4-mapped IPv6 one is unmapped first).
package loopback

import (
	"net/netip"
	"net/url"
	"strings"
)

// Host reports whether h names this machine's loopback interface. h is a host
// as a URL or a Host header carries it: a name or an IP literal, with or
// without a port, an IPv6 literal with or without its brackets
// ("[::1]:8105", "[::1]", "::1").
func Host(h string) bool {
	h = strings.TrimSpace(h)
	if h == "" {
		return false
	}
	// Brackets hold an IPv6 literal and nothing else ("[localhost]" and
	// "[127.0.0.1]" are not hosts), optionally followed by ":<port>".
	if strings.HasPrefix(h, "[") {
		end := strings.IndexByte(h, ']')
		if end < 0 {
			return false
		}
		inner, rest := h[1:end], h[end+1:]
		if rest != "" && (rest[0] != ':' || !validPort(rest[1:])) {
			return false
		}
		addr, err := netip.ParseAddr(inner)
		return err == nil && addr.Is6() && addr.Unmap().IsLoopback()
	}
	// One colon is "host:port"; more is a bare IPv6 literal.
	if strings.Count(h, ":") == 1 {
		host, port, _ := strings.Cut(h, ":")
		if !validPort(port) {
			return false
		}
		h = host
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(h)
	if err != nil {
		return false
	}
	return addr.Unmap().IsLoopback()
}

// validPort is a decimal port number, 1 to 65535 digits' worth of it.
func validPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n <= 65535
}

// HTTPURL reports whether raw is a plain http:// URL whose host is this
// machine's loopback interface (Host). An https URL is NOT one: callers accept
// https on its own terms, and this predicate answers only "is this the plain
// http that is safe anyway".
func HTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	return Host(u.Host)
}
