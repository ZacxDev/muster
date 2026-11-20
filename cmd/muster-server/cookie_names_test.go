package main

import (
	"net/netip"
	"strings"
	"testing"
)

// cookieEnv is a getenv over the three variables the cookie-name rule and the
// boot refusal read.
func cookieEnv(public, secure, origins string) func(string) string {
	return cookieEnvMode("", public, secure, origins)
}

func cookieEnvMode(mode, public, secure, origins string) func(string) string {
	return func(k string) string {
		switch k {
		case envAuthMode:
			return mode
		case envPublicURL:
			return public
		case envSecureCookies:
			return secure
		case envPluginOrigins:
			return origins
		}
		return ""
	}
}

// TestTheCookieNameRuleKeysOnTheResolvedSetting: the name is prefixed exactly
// when the RESOLVED secure-cookie setting is on and the public URL is https.
// The https row with MUSTER_SECURE_COOKIES unset is prefixed (red if the rule
// reads the literal variable rather than secureCookies' resolution); the
// Compose row (1 on http://localhost) and an explicit 0 are not.
func TestTheCookieNameRuleKeysOnTheResolvedSetting(t *testing.T) {
	for _, c := range []struct {
		public, secure string
		want           bool
	}{
		{"https://muster.example.com", "", true},
		{"https://muster.example.com", "1", true},
		{"https://muster.example.com", "0", false},
		{"http://localhost:8105", "1", false},
		{"http://muster.example.com", "", false},
		{"", "1", false},
	} {
		cfg, err := loadConfig(cookieEnv(c.public, c.secure, ""))
		if err != nil {
			t.Fatalf("%q, secure %q: %v", c.public, c.secure, err)
		}
		if got := cfg.authConfig().HostPrefixedCookies(); got != c.want {
			t.Errorf("public %q, MUSTER_SECURE_COOKIES %q: prefixed=%v, want %v", c.public, c.secure, got, c.want)
		}
	}
	// gotrue mode: its cookies are always Secure, so an https URL is enough,
	// MUSTER_SECURE_COOKIES=0 included.
	for _, c := range []struct {
		public, secure string
		want           bool
	}{
		{"https://muster.example.com", "0", true},
		{"https://muster.example.com", "", true},
		{"http://muster.example.com", "1", false},
	} {
		cfg, err := loadConfig(cookieEnvMode("gotrue", c.public, c.secure, ""))
		if err != nil {
			t.Fatalf("%q, secure %q: %v", c.public, c.secure, err)
		}
		if got := cfg.authConfig().HostPrefixedCookies(); got != c.want {
			t.Errorf("public %q, MUSTER_SECURE_COOKIES %q: prefixed=%v, want %v", c.public, c.secure, got, c.want)
		}
	}
}

// TestUnprefixedCookiesBesideASameSitePluginOriginRefuseToBoot: with
// unprefixed names, a plugin origin on muster's REGISTRABLE domain refuses to
// boot — a subdomain of muster's host, a same-site SIBLING, a wildcard over the
// site — and one on another registrable domain loads (the positive control).
// Red if the comparison is "under muster's host" (the sibling rows start) or
// by naive host suffix (the public-suffix row refuses). On a prefixed
// deployment the same sibling loads.
func TestUnprefixedCookiesBesideASameSitePluginOriginRefuseToBoot(t *testing.T) {
	ip := netip.MustParsePrefix("192.0.2.0/24").Addr().Next().String() // TEST-NET-1
	refuse := []struct{ public, secure, origins string }{
		{"http://app.example.com", "", "https://meet.app.example.com"},
		{"http://app.example.com", "", "https://meet.example.com"},
		{"http://app.example.com", "", "https://example.com"},
		{"http://app.example.com", "", "https://*.meet.example.com"},
		{"http://app.example.com", "", "https://other.example.org,https://meet.example.com"},
		{"https://app.example.com", "0", "https://meet.example.com"},
		{"http://app.example.co.uk", "", "https://meet.example.co.uk"},
	}
	for _, c := range refuse {
		_, err := loadConfig(cookieEnv(c.public, c.secure, c.origins))
		if err == nil || !strings.Contains(err.Error(), "shares MUSTER_PUBLIC_URL's registrable domain") ||
			!strings.Contains(err.Error(), "need an https MUSTER_PUBLIC_URL with MUSTER_SECURE_COOKIES on or unset;") {
			t.Errorf("public %q, secure %q, origins %q: %v; want the same-site refusal with the password-mode remedy", c.public, c.secure, c.origins, err)
		}
	}
	// gotrue: refused on a plain-http URL, with the remedy that applies there
	// (no MUSTER_SECURE_COOKIES); with an https URL and the flag off it is
	// prefixed and loads.
	if _, err := loadConfig(cookieEnvMode("gotrue", "http://app.example.com", "", "https://meet.example.com")); err == nil ||
		!strings.Contains(err.Error(), "need an https MUSTER_PUBLIC_URL;") {
		t.Errorf("gotrue, http URL, sibling origin: %v; want the refusal naming only the https URL", err)
	}
	if _, err := loadConfig(cookieEnvMode("gotrue", "https://app.example.com", "0", "https://meet.example.com")); err != nil {
		t.Errorf("gotrue, https URL, MUSTER_SECURE_COOKIES=0, sibling origin: %v; want it to load (prefixed)", err)
	}
	load := []struct{ public, secure, origins string }{
		{"http://app.example.com", "", "https://meet.example.org"},   // another registrable domain
		{"http://app.example.co.uk", "", "https://meet.other.co.uk"}, // co.uk is a public suffix
		{"http://alice.github.io", "", "https://bob.github.io"},      // so is github.io
		{"https://app.example.com", "", "https://meet.example.com"},  // prefixed: the prefix protects it
		{"http://" + ip + ":8105", "", "https://meet.example.com"},   // an IP has no registrable domain
		{"", "", "https://meet.example.com"},                         // no public URL: nothing to compare
	}
	for _, c := range load {
		if _, err := loadConfig(cookieEnv(c.public, c.secure, c.origins)); err != nil {
			t.Errorf("public %q, secure %q, origins %q: %v; want it to load", c.public, c.secure, c.origins, err)
		}
	}
}
