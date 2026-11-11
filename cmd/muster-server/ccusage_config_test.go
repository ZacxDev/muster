package main

import (
	"strings"
	"testing"
	"time"
)

func ccUsageEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// TestCCUsagePollerIsOffByDefaultAndBounded: unset is off at the default
// interval; on with a valid interval takes it; an interval out of range, not a
// duration, or set with the poller off is a boot refusal naming the variable.
func TestCCUsagePollerIsOffByDefaultAndBounded(t *testing.T) {
	c, err := parseCCUsage(ccUsageEnv(nil))
	if err != nil || c.On || c.Interval != 15*time.Minute {
		t.Fatalf("unset: %+v %v; want off at 15m", c, err)
	}
	c, err = parseCCUsage(ccUsageEnv(map[string]string{envCCUsagePoll: "1", envCCUsagePollInterval: "5m"}))
	if err != nil || !c.On || c.Interval != 5*time.Minute {
		t.Fatalf("on at 5m (the floor): %+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		{envCCUsagePoll: "1", envCCUsagePollInterval: "4m59s"},
		{envCCUsagePoll: "1", envCCUsagePollInterval: "25h"},
		{envCCUsagePoll: "1", envCCUsagePollInterval: "often"},
		{envCCUsagePollInterval: "10m"},
	} {
		if _, err := parseCCUsage(ccUsageEnv(bad)); err == nil || !strings.Contains(err.Error(), envCCUsagePollInterval) {
			t.Errorf("%v: %v; want a refusal naming %s", bad, err, envCCUsagePollInterval)
		}
	}
}

// TestCCUsagePollerNeedsThePoolAndADatabase: on, the poller is refused without
// the claude-code kind or without a database; off, nothing is refused.
func TestCCUsagePollerNeedsThePoolAndADatabase(t *testing.T) {
	off := config{}
	if r := off.ccUsageRefusal(false); r != "" {
		t.Fatalf("off: %q", r)
	}
	noKind := config{CCUsage: ccUsageConfig{On: true}}
	if r := noKind.ccUsageRefusal(true); !strings.Contains(r, envAgentKinds) {
		t.Fatalf("on without claude-code: %q", r)
	}
	withKind := config{CCUsage: ccUsageConfig{On: true}, AgentKinds: []string{"gateway", "claude-code"}}
	if r := withKind.ccUsageRefusal(false); !strings.Contains(r, "DATABASE_URL") {
		t.Fatalf("on without a database: %q", r)
	}
	if r := withKind.ccUsageRefusal(true); r != "" {
		t.Fatalf("on with both: %q", r)
	}
}

// TestCCUsagePushRefusalMatrix: the pusher is off unless its URL is set; with
// it, boot is refused when the poller is off, the token is missing, or the URL
// is not https (http only to loopback) — and the token or instance alone is
// refused as a knob that does nothing. No refusal carries the token.
func TestCCUsagePushRefusalMatrix(t *testing.T) {
	const tok = "push-token-SENTINEL-1234"
	on := map[string]string{envCCUsagePoll: "1"}
	with := func(kv map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range on {
			out[k] = v
		}
		for k, v := range kv {
			out[k] = v
		}
		return out
	}
	c, err := parseCCUsage(ccUsageEnv(on))
	if err != nil || c.Push != (ccUsagePushConfig{}) {
		t.Fatalf("no push variables: %+v %v; want the pusher off", c.Push, err)
	}
	c, err = parseCCUsage(ccUsageEnv(with(map[string]string{envCCUsagePushURL: "https://dash.example.com/api/usage",
		envCCUsagePushToken: tok, envPublicURL: "https://muster.example.com"})))
	if err != nil || c.Push != (ccUsagePushConfig{URL: "https://dash.example.com/api/usage", Token: tok, Instance: "muster.example.com"}) {
		t.Fatalf("a valid push: %+v %v", c.Push, err)
	}
	c, err = parseCCUsage(ccUsageEnv(with(map[string]string{envCCUsagePushURL: "http://127.0.0.1:9/x",
		envCCUsagePushToken: tok, envCCUsagePushInstance: "lab"})))
	if err != nil || c.Push.Instance != "lab" {
		t.Fatalf("loopback http with an instance: %+v %v", c.Push, err)
	}
	c, err = parseCCUsage(ccUsageEnv(with(map[string]string{envCCUsagePushURL: "https://dash.example.com/x", envCCUsagePushToken: tok})))
	if err != nil || c.Push.Instance != "muster" {
		t.Fatalf("no public URL: instance %q %v, want muster", c.Push.Instance, err)
	}
	for _, bad := range []struct {
		name  string
		env   map[string]string
		names string // the variable the refusal must name
	}{
		{"poller off", map[string]string{envCCUsagePushURL: "https://dash.example.com/x", envCCUsagePushToken: tok}, envCCUsagePoll},
		{"no token", with(map[string]string{envCCUsagePushURL: "https://dash.example.com/x"}), envCCUsagePushToken},
		{"plain http", with(map[string]string{envCCUsagePushURL: "http://dash.example.com/x", envCCUsagePushToken: tok}), envCCUsagePushURL},
		{"relative", with(map[string]string{envCCUsagePushURL: "/api/usage", envCCUsagePushToken: tok}), envCCUsagePushURL},
		{"userinfo", with(map[string]string{envCCUsagePushURL: "https://u:" + tok + "@dash.example.com/x", envCCUsagePushToken: tok}), envCCUsagePushURL},
		{"instance too long", with(map[string]string{envCCUsagePushURL: "https://dash.example.com/x", envCCUsagePushToken: tok,
			envCCUsagePushInstance: strings.Repeat("i", 65)}), envCCUsagePushInstance},
		{"token without URL", with(map[string]string{envCCUsagePushToken: tok}), envCCUsagePushToken},
		{"instance without URL", with(map[string]string{envCCUsagePushInstance: "lab"}), envCCUsagePushInstance},
	} {
		_, err := parseCCUsage(ccUsageEnv(bad.env))
		if err == nil || !strings.Contains(err.Error(), bad.names) {
			t.Errorf("%s: %v; want a refusal naming %s", bad.name, err, bad.names)
			continue
		}
		if strings.Contains(err.Error(), tok) {
			t.Errorf("%s: the refusal carries the token: %v", bad.name, err)
		}
	}
}
