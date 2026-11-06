package agentspec

import (
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/podagent"
)

// TestTheComposeChiefsAllowedToolsArePinned is the ledger of the tools the
// Compose chief runs without a prompt (operator decision: exactly these eight,
// reading the fleet and the board and routing to a session). It fails if the
// set GROWS or SHRINKS: a change is a change to the chief's permission posture
// and needs the same decision. The spec carries exactly that set and no HQ
// server (no blanket HQ approval on Compose); a pod spec carries no such list;
// and a session asking for its own list, or for the HQ server, is refused, so
// the ledger is the one source.
func TestTheComposeChiefsAllowedToolsArePinned(t *testing.T) {
	want := []string{"attention_list", "fleet_list", "fleet_report", "focus_search",
		"route_to_session", "sessions_overview", "task_get", "tasks_list"}
	if got := slices.Sorted(slices.Values(ComposeChiefAllowedMCPTools)); !slices.Equal(got, want) {
		t.Fatalf("the Compose chief's pre-allowed tools are %v, want exactly %v", got, want)
	}

	cfg := ccConfig()
	cfg.ConnectURL, cfg.ConnectPort = llmConnectURL, 18191
	cfg.ComposeContainer = true
	spec := mustBuild(t, composeChiefAgent(), cfg, Options{ClaudeCodeSession: ClaudeCodeSession{LLMRoute: true}})
	allow, ok := envValue(spec, podagent.EnvAllowMCPTools)
	if !ok {
		t.Fatalf("the Compose chief's spec pre-allows nothing (%s unset)", podagent.EnvAllowMCPTools)
	}
	if names := slices.Sorted(slices.Values(strings.Split(allow, ","))); !slices.Equal(names, want) {
		t.Fatalf("%s = %q, want exactly %v", podagent.EnvAllowMCPTools, allow, want)
	}
	if _, ok := envValue(spec, podagent.EnvHQMCP); ok {
		t.Fatalf("the Compose chief's spec carries %s: the HQ server's blanket approval is withdrawn on Compose", podagent.EnvHQMCP)
	}

	cfg.ComposeContainer = false
	if pod := mustBuild(t, composeChiefAgent(), cfg, Options{ClaudeCodeSession: ClaudeCodeSession{LLMRoute: true}}); func() bool {
		_, ok := envValue(pod, podagent.EnvAllowMCPTools)
		return ok
	}() {
		t.Error("control: a pod spec carries the Compose chief's allow list")
	}

	cfg.ComposeContainer = true
	for name, sess := range map[string]ClaudeCodeSession{
		"its own allow list": {LLMRoute: true, AllowMCPTools: []string{"task_comment"}},
		"the HQ server":      {LLMRoute: true, HQMCP: true},
	} {
		if _, err := Build(composeChiefAgent(), cfg, Options{ClaudeCodeSession: sess}); err == nil ||
			!strings.Contains(err.Error(), "ComposeChiefAllowedMCPTools alone") {
			t.Errorf("a Compose chief asking for %s: %v, want refused", name, err)
		}
	}
}
