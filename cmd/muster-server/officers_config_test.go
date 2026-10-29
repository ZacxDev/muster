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
