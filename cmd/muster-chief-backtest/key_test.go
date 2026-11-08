package main

import (
	"strings"
	"testing"
)

// TestTheBacktestRefusesProductionsKey: no BT_OPENROUTER_KEY is refused, and
// so is one equal to MUSTER_AGENT_OPENROUTER_KEY; a dedicated key passes.
func TestTheBacktestRefusesProductionsKey(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := backtestKey(env(map[string]string{"MUSTER_AGENT_OPENROUTER_KEY": "sk-or-v1-FAKE-prod"})); err == nil ||
		!strings.Contains(err.Error(), "BT_OPENROUTER_KEY is empty") {
		t.Fatalf("unset: %v", err)
	}
	if _, err := backtestKey(env(map[string]string{"BT_OPENROUTER_KEY": "sk-or-v1-FAKE-prod", "MUSTER_AGENT_OPENROUTER_KEY": " sk-or-v1-FAKE-prod "})); err == nil ||
		!strings.Contains(err.Error(), "same key as MUSTER_AGENT_OPENROUTER_KEY") {
		t.Fatalf("production's key: %v", err)
	}
	if k, err := backtestKey(env(map[string]string{"BT_OPENROUTER_KEY": "sk-or-v1-FAKE-bt", "MUSTER_AGENT_OPENROUTER_KEY": "sk-or-v1-FAKE-prod"})); err != nil || k != "sk-or-v1-FAKE-bt" {
		t.Fatalf("a dedicated key: %q %v", k, err)
	}
}
