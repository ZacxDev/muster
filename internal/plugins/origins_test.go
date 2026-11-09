package plugins

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseOriginsAcceptsOnlyHttpsHostsAndOneLeadingWildcard: the accepted
// shapes parse to exactly their patterns (lower-cased, spaces trimmed), unset
// is nil, and every malformed shape is an error naming its entry — so a typo
// refuses to boot rather than widening or silently dropping a pattern.
func TestParseOriginsAcceptsOnlyHttpsHostsAndOneLeadingWildcard(t *testing.T) {
	for raw, want := range map[string][]OriginPattern{
		"":                      nil,
		"   ":                   nil,
		"https://*.example.net": {{Wildcard: true, Host: "example.net"}},
		"https://w.example.net": {{Host: "w.example.net"}},
		" HTTPS://*.Example.NET , https://w.other-zone.example.org": {
			{Wildcard: true, Host: "example.net"}, {Host: "w.other-zone.example.org"}},
	} {
		got, err := ParseOrigins(raw)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseOrigins(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"http://*.example.net",                                // not https
		"*.example.net",                                       // no scheme
		"wss://*.example.net",                                 // another scheme
		"https://*.example.net:8443",                          // a port
		"https://*.example.net/",                              // a path, even an empty one
		"https://w.example.net/__muster/enter",                // a path
		"https://w.example.net?x=1",                           // a query
		"https://user@w.example.net",                          // user info
		"https://*.example.net,",                              // an empty entry
		"https://*.example.net,,https://w.x.org",              // an empty entry inside
		"https://*.com",                                       // a wildcard over one label
		"https://localhost",                                   // one label
		"https://a*.example.net",                              // a partial wildcard
		"https://*.*.example.net",                             // two wildcards
		"https://w.*.example.net",                             // a wildcard not first
		"https://*",                                           // nothing but a wildcard
		"https://192.0.2.1",                                   // an IPv4 address
		"https://[::1]",                                       // an IPv6 address
		"https://-bad.example.net",                            // a label starting with '-'
		"https://w..example.net",                              // an empty label
		"https://" + strings.Repeat("a", 64) + ".example.net", // a label over 63
	} {
		if got, err := ParseOrigins(raw); err == nil {
			t.Errorf("ParseOrigins(%q) = %v; want an error", raw, got)
		}
	}
}

// TestOriginPatternMatchesHost: a wildcard matches every host below its zone at
// any depth and never the zone itself or a look-alike sharing its suffix
// without the dot; an exact pattern matches only its host. A port, a trailing
// dot and case do not change the answer.
func TestOriginPatternMatchesHost(t *testing.T) {
	zone := OriginPattern{Wildcard: true, Host: "example.net"}
	exact := OriginPattern{Host: "w.example.org"}
	for _, c := range []struct {
		p    OriginPattern
		host string
		want bool
	}{
		{zone, "p-abc.example.net", true},
		{zone, "P-ABC.Example.Net:443", true},
		{zone, "p-abc.example.net.", true},
		{zone, "a.b.example.net", true},
		{zone, "example.net", false},
		{zone, "evilexample.net", false},
		{zone, "example.net.evil.org", false},
		{zone, "muster.example.org", false},
		{zone, "", false},
		{exact, "w.example.org", true},
		{exact, "w.example.org:8080", true},
		{exact, "x.w.example.org", false},
		{exact, "example.org", false},
	} {
		if got := c.p.MatchesHost(c.host); got != c.want {
			t.Errorf("%s.MatchesHost(%q) = %v; want %v", c.p, c.host, got, c.want)
		}
	}
	if AnyMatchesHost(nil, "p-abc.example.net") {
		t.Error("no patterns matched a host; want nothing matched")
	}
	if !AnyMatchesHost([]OriginPattern{exact, zone}, "p-abc.example.net") {
		t.Error("the second pattern's host was not matched")
	}
}
