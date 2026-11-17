package main

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// The chief sessions' spend door's configuration (chiefsessions.go). Each
// refusal is reached on its own; the values are pairwise distinct and none is
// the default (20).
func TestTheChiefSessionsSwitchAndItsRefusals(t *testing.T) {
	const sessKey, agentKey = "sk-or-v1-FAKE-sessions-3a4b5c", "sk-or-v1-FAKE-agents-7d8e9f"
	read := func(env map[string]string) (config, error) {
		c := config{AgentOpenRouterKey: env[envAgentOpenRouterKey]}
		err := c.applyChiefSessions(func(k string) string { return env[k] })
		return c, err
	}
	// Off: nothing set, nothing on.
	if c, err := read(map[string]string{}); err != nil || c.ChiefSessions.On {
		t.Fatalf("unset: %+v %v", c.ChiefSessions, err)
	}
	// On with its key: the default daily cap, no session cap.
	c, err := read(map[string]string{envChiefSessionsOpenRouter: "1", envChiefSessionsKey: sessKey, envAgentOpenRouterKey: agentKey})
	if err != nil || !c.ChiefSessions.On || c.ChiefSessions.Key != sessKey || c.ChiefSessions.DailyUSD != api.DefaultChiefSessionsDailyUSD ||
		c.ChiefSessions.SessionUSD != 0 {
		t.Fatalf("on, defaults: %+v %v", c.ChiefSessions, err)
	}
	if api.DefaultChiefSessionsDailyUSD != 20 {
		t.Fatalf("the default daily cap is %v, the operator decided 20", api.DefaultChiefSessionsDailyUSD)
	}
	// On with both caps.
	c, err = read(map[string]string{envChiefSessionsOpenRouter: "1", envChiefSessionsKey: sessKey, envChiefSessionsDailyUSD: "7.25",
		envChiefSessionsSessionUSD: "1.75"})
	if err != nil || c.ChiefSessions.DailyUSD != 7.25 || c.ChiefSessions.SessionUSD != 1.75 {
		t.Fatalf("on, caps: %+v %v", c.ChiefSessions, err)
	}
	for _, m := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"on without its key", map[string]string{envChiefSessionsOpenRouter: "1", envAgentOpenRouterKey: agentKey},
			"MUSTER_CHIEF_SESSIONS_OPENROUTER_KEY is unset"},
		{"the agents' key as its key", map[string]string{envChiefSessionsOpenRouter: "1", envChiefSessionsKey: agentKey,
			envAgentOpenRouterKey: agentKey}, "is the same key as MUSTER_AGENT_OPENROUTER_KEY"},
		{"the key with the switch off", map[string]string{envChiefSessionsKey: sessKey}, "is not 1: nothing reads it"},
		{"a cap with the switch off", map[string]string{envChiefSessionsDailyUSD: "7.25"}, "is not 1: nothing reads it"},
		{"an unknown switch value", map[string]string{envChiefSessionsOpenRouter: "yes", envChiefSessionsKey: sessKey},
			"invalid MUSTER_CHIEF_SESSIONS_OPENROUTER"},
		{"a zero daily cap", map[string]string{envChiefSessionsOpenRouter: "1", envChiefSessionsKey: sessKey, envChiefSessionsDailyUSD: "0"},
			"invalid MUSTER_CHIEF_SESSIONS_DAILY_USD"},
		{"a negative session cap", map[string]string{envChiefSessionsOpenRouter: "1", envChiefSessionsKey: sessKey,
			envChiefSessionsSessionUSD: "-3"}, "invalid MUSTER_CHIEF_SESSIONS_SESSION_USD"},
	} {
		t.Run(m.name, func(t *testing.T) {
			_, err := read(m.env)
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("err = %v, want one containing %q", err, m.want)
			}
			if strings.Contains(err.Error(), "sk-or-v1-FAKE") {
				t.Fatal("the refusal carries a key")
			}
		})
	}
}
