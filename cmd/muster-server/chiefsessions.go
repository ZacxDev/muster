package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/api"
)

// ---------------------------------------------------------------------------
// THE CHIEF'S OPENROUTER SESSIONS: THE SPEND DOOR'S CONFIGURATION
// (api.EnableChiefSessionsLLM; docs/chief.md "OpenRouter sessions, budget").
//
// 🔴 AN EXPLICIT SWITCH, MUSTER_CHIEF_SESSIONS_OPENROUTER=1, NOT THE KEY'S
// PRESENCE. With the key as the switch, "the feature is on and the key is
// unset" could not be said, so the one boot refusal that matters — a server
// that is meant to serve these sessions but has no key of their own — could
// never fire; and a key left in a Secret would silently open a door that
// spends money. So: the switch on needs the key (else boot is refused), and
// the key or a cap set with the switch off is refused too (a setting that
// does nothing).
//
// 🔴 MUSTER_AGENT_OPENROUTER_KEY NEVER STANDS IN, and the two may not be the
// same value: that key's credit limit is what production's agents and the
// chief run on, and these sessions must not spend it.
// ---------------------------------------------------------------------------

// chiefSessionsConfig is the door's settings.
type chiefSessionsConfig struct {
	On         bool
	Key        string
	DailyUSD   float64
	SessionUSD float64 // 0: no per-session cap
}

// applyChiefSessions reads the four variables. It runs after the struct
// literal has read MUSTER_AGENT_OPENROUTER_KEY.
func (c *config) applyChiefSessions(getenv func(string) string) error {
	sw := strings.TrimSpace(getenv(envChiefSessionsOpenRouter))
	key := strings.TrimSpace(getenv(envChiefSessionsKey))
	daily := strings.TrimSpace(getenv(envChiefSessionsDailyUSD))
	session := strings.TrimSpace(getenv(envChiefSessionsSessionUSD))
	switch sw {
	case "", "0":
		var set []string
		for _, v := range []struct{ name, val string }{{envChiefSessionsKey, key}, {envChiefSessionsDailyUSD, daily},
			{envChiefSessionsSessionUSD, session}} {
			if v.val != "" {
				set = append(set, v.name)
			}
		}
		if len(set) > 0 {
			return fmt.Errorf("%s is set but %s is not 1: nothing reads it. Set %s=1 to serve the chief's OpenRouter "+
				"sessions, or unset it", strings.Join(set, ", "), envChiefSessionsOpenRouter, envChiefSessionsOpenRouter)
		}
		return nil
	case "1":
	default:
		return fmt.Errorf("invalid %s %q: 1 (on) or unset", envChiefSessionsOpenRouter, sw)
	}
	cs := chiefSessionsConfig{On: true, Key: key, DailyUSD: api.DefaultChiefSessionsDailyUSD}
	if key == "" {
		return fmt.Errorf("%s=1 serves the chief's OpenRouter sessions on their own key, and %s is unset: set it (a "+
			"separate OpenRouter key; %s never stands in for it), or unset %s", envChiefSessionsOpenRouter,
			envChiefSessionsKey, envAgentOpenRouterKey, envChiefSessionsOpenRouter)
	}
	if key == c.AgentOpenRouterKey {
		return fmt.Errorf("%s is the same key as %s: the chief's OpenRouter sessions need a key of their own, so their "+
			"spend cannot use up the agents' credit limit", envChiefSessionsKey, envAgentOpenRouterKey)
	}
	if daily != "" {
		v, err := positiveUSD(daily)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %v", envChiefSessionsDailyUSD, daily, err)
		}
		cs.DailyUSD = v
	}
	if session != "" {
		v, err := positiveUSD(session)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %v (unset it for no per-session cap)", envChiefSessionsSessionUSD, session, err)
		}
		cs.SessionUSD = v
	}
	c.ChiefSessions = cs
	return nil
}

// positiveUSD parses a dollar amount above 0.
func positiveUSD(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, fmt.Errorf("want a dollar amount above 0, such as 20")
	}
	return v, nil
}
