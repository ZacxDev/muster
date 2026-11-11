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
	// Interval is MUSTER_CC_USAGE_POLL_INTERVAL (ccusage.DefaultInterval, 15m,
	// by default), from ccusage.MinInterval to ccusage.MaxInterval.
	Interval time.Duration
	// Push is the optional pusher's; Push.URL "" is off.
	Push ccUsagePushConfig
}

// ccUsagePushConfig is the usage pusher's (MUSTER_CC_USAGE_PUSH_*): after each
// poll cycle it PUTs the readings to URL with Token as a bearer, labelled
// Instance.
type ccUsagePushConfig struct {
	URL      string
	Token    string
	Instance string
}

// parseCCUsage reads the poller's configuration. The interval is refused
// without the switch: a knob that does nothing reads as if it did something.
func parseCCUsage(getenv func(string) string) (ccUsageConfig, error) {
	var err error
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
	if c.Push, err = parseCCUsagePush(getenv, c.On); err != nil {
		return ccUsageConfig{}, err
	}
	return c, nil
}

// parseCCUsagePush reads the pusher's configuration. Boot is refused when the
// URL is set with the poller off (it would push nothing, ever), without a
// token, or not https (plain http only to a loopback host, for tests); and
// when the token or the instance is set without the URL (a knob that does
// nothing). The token never appears in a refusal.
func parseCCUsagePush(getenv func(string) string, pollerOn bool) (ccUsagePushConfig, error) {
	u := strings.TrimSpace(getenv(envCCUsagePushURL))
	tok := strings.TrimSpace(getenv(envCCUsagePushToken))
	inst := strings.TrimSpace(getenv(envCCUsagePushInstance))
	if u == "" {
		for _, v := range []struct{ name, val string }{{envCCUsagePushToken, tok}, {envCCUsagePushInstance, inst}} {
			if v.val != "" {
				return ccUsagePushConfig{}, fmt.Errorf("%s is set but %s is not: it does nothing without the push URL", v.name, envCCUsagePushURL)
			}
		}
		return ccUsagePushConfig{}, nil
	}
	if !pollerOn {
		return ccUsagePushConfig{}, fmt.Errorf("%s is set but %s is not: with the poller off there are no readings to push", envCCUsagePushURL, envCCUsagePoll)
	}
	if err := ccusage.CheckPushURL(u); err != nil {
		return ccUsagePushConfig{}, fmt.Errorf("invalid %s: %v", envCCUsagePushURL, err)
	}
	if tok == "" {
		return ccUsagePushConfig{}, fmt.Errorf("%s is set but %s is not: the push needs its bearer token", envCCUsagePushURL, envCCUsagePushToken)
	}
	instance, err := ccusage.PushInstance(inst, getenv(envPublicURL))
	if err != nil {
		return ccUsagePushConfig{}, fmt.Errorf("invalid %s: %v", envCCUsagePushInstance, err)
	}
	return ccUsagePushConfig{URL: u, Token: tok, Instance: instance}, nil
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
