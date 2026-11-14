package main

import (
	"slices"
	"strings"
	"testing"
)

// TestOutboundPublicLinksConfig: MUSTER_OUTBOUND_PUBLIC_LINKS is parsed into
// canonical host[/path] entries, and an entry that is not a public link — an
// internal host, an IP address, not link-shaped — refuses to boot, naming the
// variable.
func TestOutboundPublicLinksConfig(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(k string) string {
			if k == envOutboundPublicLinks {
				return v
			}
			return ""
		})
	}
	cfg, err := load(" https://GitHub.com/example/project/ , www.example.org ")
	if err != nil {
		t.Fatal(err)
	}
	// splitList lower-cases; the screen compares without case.
	if want := []string{"github.com/example/project", "example.org"}; !slices.Equal(cfg.OutboundPublicLinks, want) {
		t.Fatalf("OutboundPublicLinks = %q; want %q", cfg.OutboundPublicLinks, want)
	}
	if cfg, err := load(""); err != nil || cfg.OutboundPublicLinks != nil {
		t.Fatalf("unset: %q, %v; want none", cfg.OutboundPublicLinks, err)
	}
	for _, bad := range []string{"example.org,store.example." + "lan", "10.1.2." + "3", "not a link"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envOutboundPublicLinks) {
			t.Errorf("%q: %v; want refused naming %s", bad, err, envOutboundPublicLinks)
		}
	}
}
