// Package clientaddr is the ONE reading of "which address is this request
// from". Every per-address limit, every address forwarded to GoTrue
// (X-Muster-Client-IP), every audit row and log line that names a client takes
// it from here (Of), in muster-server and in muster-preview alike.
//
// 🔴 THE DEFAULT TRUSTS NOTHING. The answer is the TCP peer of the connection,
// whatever the request's headers say, on every listener. A forwarding header
// (CF-Connecting-IP, X-Forwarded-For, X-Real-IP) is something any client can
// type, and a plain reverse proxy passes most of them through unchanged, so a
// header is believed ONLY when the TCP peer is inside a configured trusted range
// (MUSTER_TRUSTED_PROXIES) — and then only the one header the operator named
// (MUSTER_CLIENT_IP_HEADER).
//
// 🔴 X-Forwarded-For IS READ FROM THE RIGHT. Each proxy APPENDS the address it
// received the request from, so the right-most entries are the ones written by
// proxies, and the left-most is whatever the client sent. The client is the
// first entry, walking from the right, that is NOT a trusted proxy. The
// left-most entry is used only when every entry is trusted.
//
// ⚠ THAT IS SAFE ONLY BEHIND A PROXY THAT APPENDS. A proxy that forwards the
// client's X-Forwarded-For untouched (nginx with a bare proxy_pass) leaves a
// client-typed value right-most, and no reading of the header can undo that:
// such a proxy must name the client in a header it OVERWRITES (X-Real-IP) or
// append it ($proxy_add_x_forwarded_for). docs/configuration.md has the
// recipes.
//
// The package is a leaf: the standard library only.
package clientaddr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The headers MUSTER_CLIENT_IP_HEADER may name, in their canonical spelling.
const (
	HeaderCFConnectingIP = "CF-Connecting-IP"
	HeaderXForwardedFor  = "X-Forwarded-For"
	HeaderXRealIP        = "X-Real-IP"
)

// Unknown is the address of a request whose TCP peer does not parse (never a
// real network connection; a test's hand-made RemoteAddr).
const Unknown = "unknown"

// headers is the closed set a Policy may read, by lower-cased name.
var headers = map[string]string{
	"cf-connecting-ip": HeaderCFConnectingIP,
	"x-forwarded-for":  HeaderXForwardedFor,
	"x-real-ip":        HeaderXRealIP,
}

// forwarding is every header that names a client address. Their arrival from
// an untrusted peer is logged (rate-limited): a deployment behind a proxy that
// has not configured the trusted ranges keys everything on the proxy, and the
// log line is how its operator finds out.
var forwarding = []string{HeaderCFConnectingIP, HeaderXForwardedFor, HeaderXRealIP, "True-Client-IP", "Forwarded"}

// Config is what New reads. The two *Var names are only for messages, so the
// same code serves muster-server (MUSTER_*) and muster-preview (PREVIEW_*).
type Config struct {
	// TrustedProxies is the comma-separated CIDR list (a bare address is a
	// single host). Empty trusts nothing.
	TrustedProxies string
	// Header is the header a trusted peer names the client in: one of the
	// Header* constants, case-insensitive. Empty with TrustedProxies empty.
	Header string

	TrustedVar, HeaderVar string
}

// Policy decides a request's client address. A nil *Policy is the strict
// default: the TCP peer, always.
type Policy struct {
	trusted    []netip.Prefix
	header     string // canonical; "" when nothing is trusted
	trustedVar string
	headerVar  string

	logf func(format string, args ...any)
	now  func() time.Time
	// untrusted and missing rate-limit the two warnings.
	untrusted, missing *every
}

// New parses c. Both settings or neither: a header with no trusted range would
// never be read, and a trusted range with no header would believe nothing,
// and either is a configuration that silently does not do what it says.
func New(c Config) (*Policy, error) {
	if c.TrustedVar == "" {
		c.TrustedVar = "trusted proxies"
	}
	if c.HeaderVar == "" {
		c.HeaderVar = "client IP header"
	}
	p := &Policy{trustedVar: c.TrustedVar, headerVar: c.HeaderVar}
	for _, f := range strings.Split(c.TrustedProxies, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		pfx, err := parsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", c.TrustedVar, f, err)
		}
		p.trusted = append(p.trusted, pfx)
	}
	h := strings.TrimSpace(c.Header)
	switch {
	case h == "" && len(p.trusted) == 0:
		return p, nil
	case h == "":
		return nil, fmt.Errorf("%s is set but %s is not: name the header your proxy writes the client's address in (%s, %s or %s)",
			c.TrustedVar, c.HeaderVar, HeaderCFConnectingIP, HeaderXForwardedFor, HeaderXRealIP)
	case len(p.trusted) == 0:
		return nil, fmt.Errorf("%s=%s is set but %s is empty, so the header would never be believed: list the proxy's address range",
			c.HeaderVar, h, c.TrustedVar)
	}
	canon, ok := headers[strings.ToLower(h)]
	if !ok {
		return nil, fmt.Errorf("%s=%q: want %s, %s or %s", c.HeaderVar, h, HeaderCFConnectingIP, HeaderXForwardedFor, HeaderXRealIP)
	}
	p.header = canon
	return p, nil
}

// parsePrefix reads one trusted range: a CIDR or a bare address.
func parsePrefix(s string) (netip.Prefix, error) {
	var pfx netip.Prefix
	if strings.Contains(s, "/") {
		var err error
		if pfx, err = netip.ParsePrefix(s); err != nil {
			return netip.Prefix{}, errors.New("not a CIDR range")
		}
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" {
			return netip.Prefix{}, errors.New("not an IP address or CIDR range")
		}
		pfx = netip.PrefixFrom(a, a.BitLen())
	}
	if pfx.Addr().Is4In6() {
		return netip.Prefix{}, errors.New("an IPv4-mapped IPv6 range: write the IPv4 form")
	}
	if pfx.Bits() == 0 {
		// Trusting every address is the spoofable behaviour this setting exists
		// to remove, under a name that reads as configured.
		return netip.Prefix{}, errors.New("a /0 range trusts every peer; list the proxies' own ranges")
	}
	return pfx.Masked(), nil
}

// WithLog returns a copy of p that logs, through logf, forwarding headers
// arriving from an untrusted peer and a trusted peer sending no usable header,
// each at most once a minute. A nil p gives the strict default with logging.
func (p *Policy) WithLog(logf func(format string, args ...any)) *Policy {
	var c Policy
	if p != nil {
		c = *p
	}
	if c.trustedVar == "" {
		c.trustedVar, c.headerVar = "trusted proxies", "client IP header"
	}
	c.logf = logf
	c.now = time.Now
	c.untrusted, c.missing = &every{}, &every{}
	return &c
}

// Header is the header a trusted peer is believed in; "" when none is.
func (p *Policy) Header() string {
	if p == nil {
		return ""
	}
	return p.header
}

// Trusted is the trusted ranges, as written back out.
func (p *Policy) Trusted() []string {
	if p == nil {
		return nil
	}
	out := make([]string, len(p.trusted))
	for i, t := range p.trusted {
		out[i] = t.String()
	}
	return out
}

// Describe is the boot log's line for the active mode.
func (p *Policy) Describe() string {
	if p == nil || p.header == "" {
		unset := "no trusted proxy is configured"
		if p != nil && p.trustedVar != "" {
			unset = p.trustedVar + " and " + p.headerVar + " are unset"
		}
		return fmt.Sprintf("client address: STRICT — the TCP peer on every listener; %s, %s and %s are ignored "+
			"(%s). Behind a reverse proxy every client then shares the proxy's address",
			HeaderCFConnectingIP, HeaderXForwardedFor, HeaderXRealIP, unset)
	}
	how := "its value"
	if p.header == HeaderXForwardedFor {
		how = "the right-most entry that is not a trusted proxy"
	}
	return fmt.Sprintf("client address: TRUSTED PROXIES — when the TCP peer is in %s (%s), %s of %s (%s); "+
		"from any other peer, the TCP peer, and every forwarding header is ignored",
		strings.Join(p.Trusted(), ","), p.trustedVar, how, p.header, p.headerVar)
}

// Resolve is the client address of r: the TCP peer, unless the peer is a
// trusted proxy, in which case the configured header's client (validated; the
// peer when the header is absent or does not parse). Always a canonical IP
// string, or Unknown.
func (p *Policy) Resolve(r *http.Request) string {
	peer, ok := peerOf(r.RemoteAddr)
	if !ok {
		return Unknown
	}
	if p == nil || p.header == "" || !p.trusts(peer) {
		if p != nil && p.logf != nil {
			if names := forwardingIn(r.Header); len(names) > 0 {
				p.untrusted.do(p.now(), func(suppressed int) {
					p.logf("client address: ignored %s from %s, which is not a trusted proxy (%s)%s",
						strings.Join(names, ", "), peer, p.trustedVar, suppressedNote(suppressed))
				})
			}
		}
		return peer.String()
	}
	if a, ok := p.fromHeader(r.Header); ok {
		return a.String()
	}
	if p.logf != nil {
		p.missing.do(p.now(), func(suppressed int) {
			p.logf("client address: trusted proxy %s sent no usable %s, so the proxy's own address is used%s",
				peer, p.header, suppressedNote(suppressed))
		})
	}
	return peer.String()
}

// fromHeader reads the configured header. CF-Connecting-IP and X-Real-IP are
// one address, written (overwritten) by the proxy: more than one line of it is
// ambiguous and believed as none. X-Forwarded-For is a list across every line,
// walked from the right.
func (p *Policy) fromHeader(h http.Header) (netip.Addr, bool) {
	vals := h.Values(p.header)
	if p.header != HeaderXForwardedFor {
		if len(vals) != 1 {
			return netip.Addr{}, false
		}
		return parseHop(vals[0])
	}
	var hops []string
	for _, v := range vals {
		hops = append(hops, strings.Split(v, ",")...)
	}
	if len(hops) == 0 {
		return netip.Addr{}, false
	}
	var last netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			// A hop that does not parse breaks the chain: nothing to its left
			// was written by a proxy this one can vouch for.
			return netip.Addr{}, false
		}
		if !p.trusts(a) {
			return a, true
		}
		last = a
	}
	return last, true // every hop is a trusted proxy: the left-most
}

func (p *Policy) trusts(a netip.Addr) bool {
	for _, t := range p.trusted {
		if t.Contains(a) {
			return true
		}
	}
	return false
}

// parseHop reads one address from a header: a bare IPv4 or IPv6 address, an
// IPv6 one in brackets, or either with a port. The zone is dropped and an
// IPv4-mapped address is unmapped; the unspecified address is refused.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		ap, perr := netip.ParseAddrPort(s)
		if perr != nil {
			if a, err = netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")); err != nil {
				return netip.Addr{}, false
			}
		} else {
			a = ap.Addr()
		}
	}
	a = a.WithZone("").Unmap()
	if !a.IsValid() || a.IsUnspecified() {
		return netip.Addr{}, false
	}
	return a, true
}

// peerOf parses the TCP peer (http.Request.RemoteAddr: host:port).
func peerOf(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	a = a.WithZone("").Unmap()
	return a, a.IsValid()
}

func forwardingIn(h http.Header) []string {
	var names []string
	for _, n := range forwarding {
		if len(h.Values(n)) > 0 {
			names = append(names, n)
		}
	}
	return names
}

func suppressedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d more like it in the last minute not logged)", n)
}

// every runs a function at most once a minute and counts what it skipped.
type every struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

const logEvery = time.Minute

func (e *every) do(now time.Time, f func(suppressed int)) {
	e.mu.Lock()
	if !e.last.IsZero() && now.Sub(e.last) < logEvery {
		e.suppressed++
		e.mu.Unlock()
		return
	}
	n := e.suppressed
	e.last, e.suppressed = now, 0
	e.mu.Unlock()
	f(n)
}

// --- the request's address, resolved once ------------------------------------

type ctxKey struct{}

// Middleware resolves each request's client address once, with p, and carries
// it on the request for Of.
func (p *Policy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p.Resolve(r))))
	})
}

// Of is r's client address: the one Middleware resolved, or — for a request
// that did not pass through it — the TCP peer (the strict default; never a
// header).
func Of(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKey{}).(string); ok {
		return v
	}
	return (*Policy)(nil).Resolve(r)
}
