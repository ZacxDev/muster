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
