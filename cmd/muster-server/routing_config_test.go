package main

import (
	"strings"
	"testing"
	"time"
)

// 🔴 R1: MUSTER_ROUTE_RELAY_ALLOW binds each sender to a plugin. The old
// "<uid>:<address>" form is REFUSED at boot — never read as something else —
// and so is a non-ASCII address.
func TestTheRelayAllowlistNeedsAPluginAndAnASCIIAddress(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(n string) string {
			if n == envRouteRelayAllow {
				return v
			}
			return ""
		})
	}
	cfg, err := load("1:example-inbox:ada@example.invalid, 2:example-inbox:*@corp.example.invalid")
	if err != nil || cfg.RouteRelayAllow.Owners() != 2 {
		t.Fatalf("the plugin-bound form: %v, %d owners", err, cfg.RouteRelayAllow.Owners())
	}
	// An agent-mail owner wildcard (and an exact agent address) boots beside a
	// mail entry, and is read as an entry rather than refused.
	cfg, err = load("1:agent-mail:example-owner/*, 1:agent-mail:hq/tasks, 1:example-inbox:ada@example.invalid")
	if err != nil {
		t.Fatalf("the agent-mail form: %v", err)
	}
	if !cfg.RouteRelayAllow.Allows(1, "agent-mail", "example-owner/agent-infra", true) || !cfg.RouteRelayAllow.Allows(1, "agent-mail", "hq/tasks", true) {
		t.Fatal("the agent-mail entries parsed but match nothing")
	}
	for _, bad := range []string{"1:agent-mail:Example-owner/*", "1:agent-mail:*/*", "1:agent-mail:example-owner"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envRouteRelayAllow) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
	for _, bad := range []string{"1:ada@example.invalid", "1:example-inbox:Kate@example.invalid"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envRouteRelayAllow) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
}

// MUSTER_ROUTE_TIMEOUT: unset is api.DefaultRouteTimeout (30m, the deadline
// before the variable existed); a duration inside [1m, 24h] is taken as given
// and reaches the server's Routing; anything else refuses to boot, naming the
// variable.
func TestTheRouteTimeoutDefaultsParsesAndRefusesNonsense(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(n string) string {
			if n == envRouteTimeout {
				return v
			}
			return ""
		})
	}
	cfg, err := load("")
	if err != nil || cfg.RouteTimeout != 30*time.Minute {
		t.Fatalf("unset: %v, %v; want 30m", cfg.RouteTimeout, err)
	}
	// Values distinct from the default and from each other, at both bounds and
	// in the middle, so a parser that ignored the variable cannot pass.
	for v, want := range map[string]time.Duration{"1m": time.Minute, "45m": 45 * time.Minute, " 2h ": 2 * time.Hour, "24h": 24 * time.Hour} {
		cfg, err := load(v)
		if err != nil || cfg.RouteTimeout != want {
			t.Errorf("%q: %v, %v; want %v", v, cfg.RouteTimeout, err, want)
			continue
		}
		if got := sessionRouting(cfg).Timeout; got != want {
			t.Errorf("%q: the server's Routing.Timeout is %v, want %v", v, got, want)
		}
	}
	for _, bad := range []string{"soon", "30", "0", "-5m", "59s", "24h1s", "1000h"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envRouteTimeout) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
}
