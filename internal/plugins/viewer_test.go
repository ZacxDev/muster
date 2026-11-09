package plugins

import (
	"errors"
	"testing"
)

// TestValidViewerLinkRefusesEveryEntryURLTrick: the origin must be one of the
// configured patterns, and the entry URL an https URL whose host is under it —
// so a look-alike host with the zone in its query or path, user info naming
// the zone, a port, a query, a fragment, another scheme, an upper-case or
// trailing-dot host, a backslash, and an origin the admin never allowed are
// each refused. The positive controls: an entry under a wildcard pattern and
// one equal to an exact pattern's host.
func TestValidViewerLinkRefusesEveryEntryURLTrick(t *testing.T) {
	pats, err := ParseOrigins("https://*.viewer.example.net,https://w.exact.example.org")
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range []struct{ origin, entry string }{
		{"https://*.viewer.example.net", "https://w.viewer.example.net/__muster/enter"},
		{"https://*.viewer.example.net", "https://a.b.viewer.example.net/"},
		{"https://w.exact.example.org", "https://w.exact.example.org/enter"},
	} {
		got, err := ValidViewerLink(pats, ok.origin, ok.entry)
		if err != nil || got != ok.origin {
			t.Errorf("ValidViewerLink(%q, %q) = %q, %v; want accepted", ok.origin, ok.entry, got, err)
		}
	}
	const zone = "https://*.viewer.example.net"
	for _, bad := range []struct{ origin, entry string }{
		{zone, "https://evil.example.net/x?.viewer.example.net"},
		{zone, "https://evil.example.net/w.viewer.example.net"},
		{zone, "https://w.viewer.example.net@evil.example.net/"},
		{zone, "https://evil.example.net@w.viewer.example.net/"},
		{zone, "https://w.viewer.example.net:8443/enter"},
		{zone, "https://w.viewer.example.net/enter?x=1"},
		{zone, "https://w.viewer.example.net/enter?"},
		{zone, "https://w.viewer.example.net/enter#frag"},
		{zone, "http://w.viewer.example.net/enter"},
		{zone, "https://W.viewer.example.net/enter"},
		{zone, "https://w.viewer.example.net./enter"},
		{zone, `https://evil.example.net\@w.viewer.example.net/`},
		{zone, "https://viewer.example.net/enter"},
		{zone, "https://evilviewer.example.net/enter"},
		{zone, "//w.viewer.example.net/enter"},
		{zone, "https:w.viewer.example.net/enter"},
		{zone, ""},
		{"https://*.example.net", "https://w.viewer.example.net/enter"},
		{"https://w.exact.example.org", "https://x.exact.example.org/enter"},
	} {
		if got, err := ValidViewerLink(pats, bad.origin, bad.entry); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidViewerLink(%q, %q) = %q, %v; want ErrInvalid", bad.origin, bad.entry, got, err)
		}
	}
}
