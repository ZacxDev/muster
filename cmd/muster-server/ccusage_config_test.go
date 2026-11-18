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

// consumerEnv is a valid pool push plus kv.
func consumerEnv(kv map[string]string) map[string]string {
	out := map[string]string{envCCUsagePoll: "1", envCCUsagePushURL: "https://hq.example.com/api/v1/claude-pool/usage",
		envCCUsagePushToken: "push-token-SENTINEL-5678", envCCUsagePushInstance: "review-muster"}
	for k, v := range kv {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// TestCCConsumerUsageIsOffByDefault: unset is off, and the officers' muster's
// configuration — the poller and the pool push, no consumer — parses exactly
// as before, push included (the positive control: the new variable changes
// nothing when it is not set).
func TestCCConsumerUsageIsOffByDefault(t *testing.T) {
	c, err := parseCCUsage(ccUsageEnv(consumerEnv(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Consumer != (ccConsumerUsageConfig{}) {
		t.Fatalf("unset: %+v, want off", c.Consumer)
	}
	if c.Push != (ccUsagePushConfig{URL: "https://hq.example.com/api/v1/claude-pool/usage", Token: "push-token-SENTINEL-5678", Instance: "review-muster"}) || !c.On {
		t.Fatalf("the pool push changed: %+v", c)
	}
	if c, err := parseCCUsage(ccUsageEnv(nil)); err != nil || c.Consumer != (ccConsumerUsageConfig{}) {
		t.Fatalf("nothing set: %+v %v", c.Consumer, err)
	}
}

// TestCCConsumerUsageOnDerivesTheRouteFromThePoolPush: pr-review with a valid
// pool push is on, PUTting to the pool route's sibling.
func TestCCConsumerUsageOnDerivesTheRouteFromThePoolPush(t *testing.T) {
	c, err := parseCCUsage(ccUsageEnv(consumerEnv(map[string]string{envCCConsumerUsage: "pr-review"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.Consumer != (ccConsumerUsageConfig{Consumer: "pr-review", URL: "https://hq.example.com/api/v1/claude-pool/consumer-usage"}) {
		t.Fatalf("%+v", c.Consumer)
	}
}

// TestCCConsumerUsageRefusals: each refusal is its own error — an unknown
// consumer; set without the push URL; with a push URL that is not HQ's pool
// route — and none carries the token.
func TestCCConsumerUsageRefusals(t *testing.T) {
	const tok = "push-token-SENTINEL-5678"
	for _, c := range []struct {
		name string
		env  map[string]string
		want string // a phrase only this refusal has
	}{
		{"unknown consumer", consumerEnv(map[string]string{envCCConsumerUsage: "officers"}), `invalid MUSTER_CC_CONSUMER_USAGE "officers": want one of pr-review`},
		{"no push URL", map[string]string{envCCConsumerUsage: "pr-review"}, "MUSTER_CC_CONSUMER_USAGE is set but MUSTER_CC_USAGE_PUSH_URL is not"},
		{"no push URL, poller on", map[string]string{envCCConsumerUsage: "pr-review", envCCUsagePoll: "1"}, "MUSTER_CC_CONSUMER_USAGE is set but MUSTER_CC_USAGE_PUSH_URL is not"},
		{"not the pool route", consumerEnv(map[string]string{envCCConsumerUsage: "pr-review", envCCUsagePushURL: "https://hq.example.com/api/usage"}),
			"MUSTER_CC_CONSUMER_USAGE needs MUSTER_CC_USAGE_PUSH_URL to be HQ's pool route"},
	} {
		_, err := parseCCUsage(ccUsageEnv(c.env))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", c.name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), tok) {
			t.Errorf("%s: the refusal carries the token", c.name)
		}
	}
}
