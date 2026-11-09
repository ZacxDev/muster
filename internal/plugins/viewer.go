package plugins

import (
	"net/url"
	"slices"
	"strings"
)

// validCapabilities is caps, de-duplicated and in a fixed order, or ErrInvalid
// when it is empty or names a capability muster does not know (0102's CHECKs,
// said before the insert).
func validCapabilities(caps []Capability) ([]Capability, error) {
	var out []Capability
	for _, c := range []Capability{CapMessages, CapViewer} {
		if slices.Contains(caps, c) {
			out = append(out, c)
		}
	}
	for _, c := range caps {
		if c != CapMessages && c != CapViewer {
			return nil, invalid("%q is not a plugin capability (messages, viewer)", string(c))
		}
	}
	if len(out) == 0 {
		return nil, invalid("a plugin needs at least one capability")
	}
	return out, nil
}

// ValidViewerLink checks a viewer token's origin and entry URL against the
// deployment's MUSTER_PLUGIN_ORIGINS (patterns) and returns the origin as
// stored (the pattern's own spelling).
//
// The origin must BE one of patterns. The entry URL must be exactly an
// `https://<host>/<path>` URL, with its host under that origin: no user info,
// no port, no query, no fragment, no trailing dot on the host, and it must
// read back byte for byte as it parses (so no encoding or backslash trick
// reads one way here and another in a browser). So
// `https://evil.example.net/x?.<zone>` and `https://w.<zone>@evil.example.net/`
// are refused.
func ValidViewerLink(patterns []OriginPattern, origin, entry string) (string, error) {
	origin, entry = strings.TrimSpace(origin), strings.TrimSpace(entry)
	var p OriginPattern
	found := false
	for _, c := range patterns {
		if c.String() == origin {
			p, found = c, true
			break
		}
	}
	if !found {
		return "", invalid("choose an origin from this server's MUSTER_PLUGIN_ORIGINS")
	}
	u, err := url.Parse(entry)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Port() != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Host == "" || strings.HasSuffix(u.Host, ".") ||
		u.String() != entry || len(entry) > 2048 {
		return "", invalid("the entry URL must be https://<host>/<path> with no user, port, query or fragment")
	}
	if !p.MatchesHost(u.Host) || strings.ToLower(u.Host) != u.Host {
		return "", invalid("the entry URL's host must be under %s, in lower case", p.String())
	}
	return p.String(), nil
}
