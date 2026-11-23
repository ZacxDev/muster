package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestOpencodeIsASessionRuntime: MUSTER_CHIEF_SESSION_RUNTIMES admits
// opencode beside claude-code, with the claude-code kind on (ccd runs it), and
// refuses it without that kind, naming the variable.
func TestOpencodeIsASessionRuntime(t *testing.T) {
	load := func(env map[string]string) config {
		t.Helper()
		cfg, err := loadConfig(func(n string) string { return env[n] })
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cc := map[string]string{envAgentKinds: "gateway,claude-code", envAgentGateway: gatewayHooksSHA256,
		envChiefSessionRuntimes: "claude-code,opencode"}
	cfg := load(cc)
	if !reflect.DeepEqual(cfg.ChiefSessionRuntimes, []string{"claude-code", "opencode"}) || cfg.validateSessionRuntimes() != nil {
		t.Fatalf("claude-code,opencode: %v %v", cfg.ChiefSessionRuntimes, cfg.validateSessionRuntimes())
	}
	off := load(map[string]string{envChiefSessionRuntimes: "opencode"})
	if err := off.validateSessionRuntimes(); err == nil || !strings.Contains(err.Error(), envChiefSessionRuntimes) || !strings.Contains(err.Error(), "opencode") {
		t.Fatalf("opencode without the claude-code kind: %v, want a refusal naming the variable and the runtime", err)
	}
}

// TestTheBannerSaysWhyOpencodeIsNotOffered: opencode named with the spend
// door off boots, and opencodeSessionsInert names the switch it waits for;
// with the door on, or opencode not named, it says nothing.
func TestTheBannerSaysWhyOpencodeIsNotOffered(t *testing.T) {
	cfg := config{ChiefSessionRuntimes: []string{"claude-code", "opencode"}}
	if why := cfg.opencodeSessionsInert(); !strings.Contains(why, envChiefSessionsOpenRouter) || !strings.Contains(why, envChiefSessionsKey) {
		t.Fatalf("door off: %q, want the switch and the key named", why)
	}
	cfg.ChiefSessions.On = true
	if why := cfg.opencodeSessionsInert(); why != "" {
		t.Fatalf("door on: %q", why)
	}
	if why := (config{ChiefSessionRuntimes: []string{"claude-code"}}).opencodeSessionsInert(); why != "" {
		t.Fatalf("opencode not named: %q", why)
	}
}
