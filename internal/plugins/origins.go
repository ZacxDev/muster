package plugins

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// OriginPattern is one entry of MUSTER_PLUGIN_ORIGINS: the https origins a
// viewer plugin may be given (docs/design/previews.md). Either one exact host
// (`https://w.example.net`) or one leading wildcard label over a zone
// (`https://*.example.net`). Host is lower case, without the `*.`.
type OriginPattern struct {
	Wildcard bool
	Host     string
}

// String is the pattern as an origin source: `https://*.example.net`.
func (o OriginPattern) String() string {
	if o.Wildcard {
		return "https://*." + o.Host
	}
	return "https://" + o.Host
}

// MatchesHost reports whether a request's Host (a port and trailing dots
// allowed, any case) is under o. A wildcard pattern matches every host BELOW
// its zone, at any depth, as a CSP source does, and not the zone itself.
func (o OriginPattern) MatchesHost(host string) bool {
	h := normalizeRequestHost(host)
	if h == "" {
		return false
	}
	if o.Wildcard {
		return strings.HasSuffix(h, "."+o.Host)
	}
	return h == o.Host
}

// AnyMatchesHost reports whether host is under any of patterns.
func AnyMatchesHost(patterns []OriginPattern, host string) bool {
	for _, p := range patterns {
		if p.MatchesHost(host) {
			return true
		}
	}
	return false
}

// normalizeRequestHost is a Host header's host part: no port, no trailing dots
// (every one: `p.zone..` is the same host to a resolver), lower case. "" when
// there is none.
func normalizeRequestHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimRight(strings.ToLower(host), ".")
}

// ParseOrigins parses MUSTER_PLUGIN_ORIGINS: comma-separated patterns, each
// `https://<host>` or `https://*.<zone>`. Empty input is nil (unset: the seams
// are off). Anything else that is not exactly that shape is an error, so a
// typo refuses to boot rather than widening or silently disabling anything:
// another scheme, a port, a path, a query, user info, an empty entry, a
// wildcard anywhere but the whole first label, a single-label host or zone, or
// an IP address.
func ParseOrigins(raw string) ([]OriginPattern, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []OriginPattern
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		p, err := parseOrigin(entry)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", entry, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func parseOrigin(entry string) (OriginPattern, error) {
	if entry == "" {
		return OriginPattern{}, errors.New("an empty entry")
	}
	rest, ok := strings.CutPrefix(strings.ToLower(entry), "https://")
	if !ok {
		return OriginPattern{}, errors.New("not https://")
	}
	var p OriginPattern
	if zone, ok := strings.CutPrefix(rest, "*."); ok {
		p.Wildcard, rest = true, zone
	}
	if err := validPatternHost(rest); err != nil {
		return OriginPattern{}, err
	}
	p.Host = rest
	return p, nil
}

// validPatternHost accepts a DNS name of at least two labels, each 1-63 of
// [a-z0-9-] not starting or ending with '-', whose last label is not all
// digits (so not an IPv4 address), at most 253 characters.
func validPatternHost(h string) error {
	if h == "" || len(h) > 253 {
		return errors.New("want https://<host> or https://*.<zone>, nothing after the host")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return errors.New("a host or zone needs at least two labels")
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return errors.New("want https://<host> or https://*.<zone>, nothing after the host")
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return errors.New("want https://<host> or https://*.<zone>, nothing after the host")
			}
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return errors.New("an IP address is not an origin pattern")
	}
	return nil
}
