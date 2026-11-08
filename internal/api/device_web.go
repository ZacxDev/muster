package api

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// THE ANDROID SHELL'S SIGN-IN ORIGINS (MUSTER_ANDROID_SSO_ORIGINS).
//
// An option for deployments behind a single sign-on proxy: a gate at the edge
// answers muster's first page with a redirect to an identity provider. The
// Android app loads muster in a WebView and opens every other origin in a
// Custom Tab, so without this the sign-in runs in the browser, its cookies land
// there, and the WebView never reaches muster. The app reads this list before
// its first page and lets exactly these origins load in that server's WebView
// (and nothing more: the bridge and the rest stay on muster's own origin).
//
// 🔴 OPEN, AND IT MUST STAY CONFIGURATION ONLY. It is read before the first
// sign-in, so before pairing, with no credential of any kind, and through the
// same edge that gates everything else (which must therefore leave
// /api/device/ ungated, as the device tier already needs). It answers the
// operator's list and nothing else: no version, no feature switch, nothing
// per-user (TestTheDeviceWebRouteServesOnlyTheList).

// DeviceWebPath is the open route the Android app reads its sign-in origins from.
const DeviceWebPath = deviceAPIPrefix + "/web"

// MaxSSOOrigins is how many sign-in origins MUSTER_ANDROID_SSO_ORIGINS may
// name. The app keeps at most this many, so a longer list would be cut there
// silently; boot refuses it instead.
const MaxSSOOrigins = 8

// ssoHostLabel is one DNS label: letters, digits and inner hyphens.
var ssoHostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ssoNumericLabel is a label a browser's host parser reads as a number: all
// decimal digits, or `0x` and hex digits (lower case: the host is lower-cased first).
var ssoNumericLabel = regexp.MustCompile(`^([0-9]+|0x[0-9a-f]*)$`)

// ssoHasLetter: the last label of a host name must contain a letter.
var ssoHasLetter = regexp.MustCompile(`[a-z]`)

// ParseSSOOrigins parses MUSTER_ANDROID_SSO_ORIGINS: a comma-separated list of
// exact origins, each `https://host` or `https://host:port`. Empty (or only
// spaces) is "none": an empty, non-nil list and no error.
//
// Each entry must be exactly an origin: scheme https, a host name or IP
// address, an optional numeric port, and NOTHING ELSE — no path (not even
// `/`), query, fragment, user info or wildcard. An empty entry (`a,,b`) is
// refused too. The host is lower-cased and `:443` dropped, which is the form a
// browser gives the origin; duplicates after that are dropped. More than
// MaxSSOOrigins is refused. Any refusal names the offending entry.
func ParseSSOOrigins(raw string) ([]string, error) {
	out := []string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	seen := map[string]bool{}
	for _, entry := range strings.Split(raw, ",") {
		o, err := parseSSOOrigin(strings.TrimSpace(entry))
		if err != nil {
			return nil, err
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if len(out) > MaxSSOOrigins {
		return nil, fmt.Errorf("%d origins: at most %d (the app keeps no more)", len(out), MaxSSOOrigins)
	}
	return out, nil
}

// parseSSOOrigin validates and normalises one entry (see ParseSSOOrigins).
func parseSSOOrigin(s string) (string, error) {
	refuse := func(why string) (string, error) {
		return "", fmt.Errorf("%q is not an exact https origin (https://host[:port]): %s", s, why)
	}
	if s == "" {
		return refuse("an empty entry")
	}
	// url.Parse would decode an escape in the host (`auth%2eexample.test`), so
	// the origin served would not be the one written: refused before parsing.
	if strings.Contains(s, "%") {
		return refuse("it has a percent-escape")
	}
	u, err := url.Parse(s)
	if err != nil {
		return refuse(err.Error())
	}
	switch {
	case u.Scheme != "https":
		return refuse("the scheme must be https")
	case u.Opaque != "" || u.Host == "":
		return refuse("no host")
	case u.User != nil:
		return refuse("it has user info")
	case u.Path != "" || u.RawPath != "":
		return refuse("it has a path")
	case u.RawQuery != "" || u.ForceQuery:
		return refuse("it has a query")
	case u.Fragment != "" || strings.Contains(s, "#"): // an empty one parses to no Fragment
		return refuse("it has a fragment")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return refuse("an empty port")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		// A leading zero is refused, which also refuses port 0 (url.Parse
		// admits digits only, so nothing below 0 reaches here).
		if err != nil || n > 65535 || port[0] == '0' {
			return refuse("the port must be 1-65535, with no leading zero")
		}
	}
	// Only hosts a browser serves exactly as written, so the origin the app
	// compares is the one the WebView reports: a canonical dotted-quad IPv4
	// address, a bracketed IPv6 address, or a DNS name (validSSOHostName).
	bracketed := strings.HasPrefix(u.Host, "[")
	if ip := net.ParseIP(host); ip != nil {
		switch {
		case bracketed && ip.To4() != nil:
			// [::ffff:1.2.3.4]: a browser rewrites it, and it names an IPv4 host.
			return refuse("an IPv4-mapped IPv6 address; write the IPv4 address itself")
		case bracketed:
			// ip.String() is RFC 5952's compressed lower-case form, a browser's too.
			host = "[" + ip.String() + "]"
		case ip.To4() == nil || ip.String() != host:
			return refuse("an IPv4 address must be a canonical dotted quad")
		}
	} else if bracketed {
		return refuse("the bracketed host is not an IPv6 address")
	} else if !validSSOHostName(host) {
		return refuse("the host is not a host name")
	}
	if port == "" || port == "443" {
		return "https://" + host, nil
	}
	return "https://" + host + ":" + port, nil
}

// validSSOHostName reports whether h is a DNS name: dot-separated labels, no
// empty label (so no leading, trailing or doubled dot), at most 253 bytes.
//
// It must not LOOK numeric either: a browser reads `127.1`, `0x7f.0.0.1` or
// `2130706433` as an IPv4 address and rewrites it (`127.0.0.1`), so the origin
// the app would compare is not the one written. So no label may be all digits
// or `0x` hex, and the last label must contain a letter.
func validSSOHostName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	labels := strings.Split(h, ".")
	for _, label := range labels {
		if !ssoHostLabel.MatchString(label) || ssoNumericLabel.MatchString(label) {
			return false
		}
	}
	if !ssoHasLetter.MatchString(labels[len(labels)-1]) {
		return false
	}
	return true
}

// deviceWebInfo is the whole body of GET /api/device/web. 🔴 ADD NOTHING TO IT
// WITHOUT DECIDING IT MAY BE READ BY ANYONE: the route is open.
type deviceWebInfo struct {
	SSOOrigins []string `json:"sso_origins"`
}

// handleDeviceWeb is GET /api/device/web: `{"sso_origins":[...]}`, the
// operator's MUSTER_ANDROID_SSO_ORIGINS (Extensions.SSOOrigins), `[]` when
// unset. Open (see the file header). no-store: a phone re-reads it on every
// shell open, and an edge must not hand it a stale list after a change.
func (s *Server) handleDeviceWeb(w http.ResponseWriter, _ *http.Request) {
	origins := s.ext.SSOOrigins
	if origins == nil {
		origins = []string{}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, deviceWebInfo{SSOOrigins: origins})
}
