package main

import (
	"bytes"
	"context"
	"log"
	"net/netip"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/safedial"
)

// MUSTER_DIAL_DENY_PRIVATE from the environment to the dial sites.

// TestTheDialGuardIsOffByDefaultAndStrictWhenOn: unset (and every non-true
// spelling) is no guard; a true spelling is the STRICT guard, which refuses
// loopback.
func TestTheDialGuardIsOffByDefaultAndStrictWhenOn(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{{"", false}, {"0", false}, {"off", false}, {"1", true}, {"true", true}, {"on", true}} {
		cfg, err := loadConfig(func(k string) string {
			if k == envDialDenyPrivate {
				return tc.val
			}
			return ""
		})
		if err != nil {
			t.Fatalf("%s=%q: loadConfig: %v", envDialDenyPrivate, tc.val, err)
		}
		g := cfg.dialGuard()
		if (g != nil) != tc.want {
			t.Fatalf("%s=%q: guard = %v, want on=%v", envDialDenyPrivate, tc.val, g, tc.want)
		}
		if g != nil && (g.Allowed(netip.MustParseAddr("127.0.0.1")) || g.Resolve != nil) {
			t.Fatalf("%s=%q: the guard is not the strict one: %+v", envDialDenyPrivate, tc.val, g)
		}
	}
}

// TestTheDialGuardBannerLine: both directions, each naming the variable.
func TestTheDialGuardBannerLine(t *testing.T) {
	on := lineNaming(renderBanner(t, config{DialDenyPrivate: true}, api.Extensions{DialGuard: &safedial.Guard{}}), envDialDenyPrivate)
	off := lineNaming(renderBanner(t, config{}, api.Extensions{}), envDialDenyPrivate)
	if !strings.Contains(on, "dial guard: ON") || !strings.Contains(off, "dial guard: OFF") {
		t.Fatalf("banner lines:\n  on:  %q\n  off: %q", on, off)
	}
}

// TestBuildAppWiresTheDialGuard: buildApp's own banner reads the WIRED
// ext.DialGuard (the field the postgres executor is built from), so ON there
// means the extension was set, not only the config.
func TestBuildAppWiresTheDialGuard(t *testing.T) {
	for _, on := range []bool{true, false} {
		var buf bytes.Buffer
		a, err := buildApp(context.Background(), config{DialDenyPrivate: on}, log.New(&buf, "", 0))
		if err != nil {
			t.Fatalf("buildApp (guard %v): %v", on, err)
		}
		a.Close()
		line := lineNaming(buf.String(), envDialDenyPrivate)
		want := map[bool]string{true: "dial guard: ON", false: "dial guard: OFF"}[on]
		if !strings.Contains(line, want) {
			t.Fatalf("buildApp with DialDenyPrivate=%v: banner line %q, want %q", on, line, want)
		}
	}
}
