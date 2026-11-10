package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/ccusage"
)

// The usage poller's variables (envCCUsagePoll, envCCUsagePollInterval) are
// spelled in config.go's const block, as every variable this binary reads.

// ccUsageConfig is the Claude account usage poller's configuration
// (internal/ccusage).
type ccUsageConfig struct {
	// On reads each pooled account's utilization every Interval
	// (MUSTER_CC_USAGE_POLL=1; off by default: it calls the provider's usage
	// endpoint with each account's token).
	On bool
	// Interval is MUSTER_CC_USAGE_POLL_INTERVAL (ccusage.DefaultInterval, 5m,
	// by default), from ccusage.MinInterval to ccusage.MaxInterval.
	Interval time.Duration
}

// parseCCUsage reads the poller's configuration. The interval is refused
// without the switch: a knob that does nothing reads as if it did something.
func parseCCUsage(getenv func(string) string) (ccUsageConfig, error) {
	c := ccUsageConfig{On: envFlag(getenv, envCCUsagePoll), Interval: ccusage.DefaultInterval}
	if v := strings.TrimSpace(getenv(envCCUsagePollInterval)); v != "" {
		if !c.On {
			return ccUsageConfig{}, fmt.Errorf("%s is set but %s is not: the interval does nothing with the poller off", envCCUsagePollInterval, envCCUsagePoll)
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < ccusage.MinInterval || d > ccusage.MaxInterval {
			return ccUsageConfig{}, fmt.Errorf("invalid %s %q: want a duration from %s to %s", envCCUsagePollInterval, v, ccusage.MinInterval, ccusage.MaxInterval)
		}
		c.Interval = d
	}
	return c, nil
}

// ccUsageRefusal is why this configuration cannot run the poller, or "": it
// reads the pool's tokens (the claude-code kind) and keeps its readings in the
// database.
func (c config) ccUsageRefusal(hasDB bool) string {
	if !c.CCUsage.On {
		return ""
	}
	if !c.claudeCodeEnabled() {
		return envCCUsagePoll + " needs the claude-code kind (" + envAgentKinds + "): there is no account pool to read"
	}
	if !hasDB {
		return envCCUsagePoll + " needs a database (DATABASE_URL): the readings are kept there"
	}
	return ""
}
