package main

import (
	"strings"
	"testing"
)

// TestOfficersWithoutTheConnectorDoorRefuseToBoot: an officer instance reaches
// its tools only over muster's MCP server, which is the connect listener
// (MUSTER_AGENT_CONNECT_URL, with MUSTER_CONNECT_PORT and the connector store's
// key). Declared officers without it would boot instances with NO officer
// tools, silently — so the server refuses to start, naming what is missing.
// Control: the same officers with the connector door configured load.
func TestOfficersWithoutTheConnectorDoorRefuseToBoot(t *testing.T) {
	officersEnv := map[string]string{
		envDatabase: "postgres://unused", envStandalone: "1",
		envOfficers: `[{"id":"release:example-web","hq_url":"https://hq.example.invalid/api/v1"}]`,
		"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": "fixture-officer-token-cfg-1",
	}
	load := func(extra map[string]string) error {
		full := map[string]string{}
		for k, v := range officersEnv {
			full[k] = v
		}
		for k, v := range extra {
			full[k] = v
		}
		cfg, err := loadConfig(func(n string) string { return full[n] })
		if err != nil {
			return err
		}
		return cfg.validate()
	}
	err := load(nil)
	if err == nil || !strings.Contains(err.Error(), envOfficers) || !strings.Contains(err.Error(), envAgentConnectURL) {
		t.Fatalf("officers with no connector door: err = %v, want a refusal naming %s and %s", err, envOfficers, envAgentConnectURL)
	}
	if err := load(map[string]string{envConnectorEncKey: connectorKey(2), envConnectPort: "18191",
		envAgentConnectURL: "http://muster.example.test:18191"}); err != nil {
		t.Fatalf("control: officers WITH the connector door are refused: %v", err)
	}
}

// TestAnLLMOfficerIsRefusedAtBootUntilThePodRoutesToTheProxy: an officer that
// declares `llm` cannot be provisioned until the pod side exists (the
// claude-code entrypoint routing Claude Code to muster's proxy under
// CCD_LLM_ROUTE=muster): its routine instances would sit in error, holding
// their slots, retried every wake pass. So the server refuses to boot with
// one, naming the officer and the reason. Control: the same officer with a
// Claude routine model and no `llm` loads.
func TestAnLLMOfficerIsRefusedAtBootUntilThePodRoutesToTheProxy(t *testing.T) {
	load := func(officer string) error {
		env := map[string]string{
			envDatabase: "postgres://unused", envStandalone: "1",
			envOfficers: "[" + officer + "]",
			"MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB":          "fixture-officer-token-cfg-2",
			"MUSTER_OFFICER_OPENROUTER_KEY_TRIAGE_EXAMPLE_WEB": "fixture-officer-openrouter-key-cfg-2",
			envConnectorEncKey: connectorKey(2), envConnectPort: "18191",
			envAgentConnectURL: "http://muster.example.test:18191",
		}
		cfg, err := loadConfig(func(n string) string { return env[n] })
		if err != nil {
			return err
		}
		return cfg.validate()
	}
	llm := `{"id":"triage:example-web","hq_url":"https://hq.example.invalid/api/v1",` +
		`"tiers":{"routine":{"model":"deepseek/deepseek-v4.1-flash-1017","max_instances":2},"judgement":{"model":"opus","max_instances":1}},` +
		`"llm":{"daily_budget_usd":5}}`
	err := load(llm)
	if err == nil || !strings.Contains(err.Error(), "triage:example-web") || !strings.Contains(err.Error(), "CCD_LLM_ROUTE") ||
		!strings.Contains(err.Error(), "llm") {
		t.Fatalf("an llm officer before the pod route exists: err = %v, want a boot refusal naming the officer and CCD_LLM_ROUTE", err)
	}
	plain := `{"id":"triage:example-web","hq_url":"https://hq.example.invalid/api/v1",` +
		`"tiers":{"routine":{"model":"haiku","max_instances":2},"judgement":{"model":"opus","max_instances":1}}}`
	if err := load(plain); err != nil {
		t.Fatalf("control: the same tiered officer without llm is refused: %v", err)
	}
}
