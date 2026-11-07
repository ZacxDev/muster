package agentprovision

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/provision"
)

func envVal(spec provision.Spec, name string) (string, bool) {
	for _, e := range spec.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// TestAClaudeCodeSessionSourceReachesTheBuiltSpec: the adapter asks its
// session source (internal/api's officer runtime) for each claude-code agent,
// and the BUILT spec of the agent it answers for carries the pre-allowed tool
// names and the model; an agent it does not answer for (ok=false) carries
// neither variable. Read from the built spec, which is what the pod receives.
func TestAClaudeCodeSessionSourceReachesTheBuiltSpec(t *testing.T) {
	officer := ccRow("officer-release-example-web-1")
	worker := ccRow("plain-worker")
	a := adapterWith(t, ccSpecConfig(), officer)
	a.UseClaudeCodeSession(func(ag agents.Agent) (agentspec.ClaudeCodeSession, bool) {
		if ag.Name != officer.Name {
			return agentspec.ClaudeCodeSession{}, false
		}
		return agentspec.ClaudeCodeSession{AllowMCPTools: []string{"officer_brief", "officer_claim"}, Model: "opus"}, true
	})
	spec, err := a.buildSpec(context.Background(), officer)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := envVal(spec, podagent.EnvAllowMCPTools); !ok || v != "officer_brief,officer_claim" {
		t.Fatalf("%s = %q (present %v), want officer_brief,officer_claim", podagent.EnvAllowMCPTools, v, ok)
	}
	if v, ok := envVal(spec, podagent.EnvModel); !ok || v != "opus" {
		t.Fatalf("%s = %q (present %v), want opus", podagent.EnvModel, v, ok)
	}
	wspec, err := a.buildSpec(context.Background(), worker)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{podagent.EnvAllowMCPTools, podagent.EnvModel} {
		if v, ok := envVal(wspec, n); ok {
			t.Errorf("a non-officer agent's spec carries %s=%q", n, v)
		}
	}
	// No source installed: nobody gets them.
	b := adapterWith(t, ccSpecConfig(), officer)
	bspec, err := b.buildSpec(context.Background(), officer)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envVal(bspec, podagent.EnvAllowMCPTools); ok {
		t.Error("with no session source, an officer-named agent still got an allow list")
	}
}

// TestAnOfficerSessionsBypassReachesTheBuiltSpecAndATaskAgentsDoesNot: a
// session source answering BypassPermissions (the officer runtime, for an
// officer instance) puts CCD_PERMISSION_MODE=bypassPermissions in THAT
// agent's built spec — what its pod's `ccd seed` reads — and a task agent the
// source does not answer for carries no such variable (its posture unchanged).
func TestAnOfficerSessionsBypassReachesTheBuiltSpecAndATaskAgentsDoesNot(t *testing.T) {
	officer := ccRow("officer-triage-example-web-1")
	worker := ccRow("plain-worker")
	a := adapterWith(t, ccSpecConfig(), officer)
	a.UseClaudeCodeSession(func(ag agents.Agent) (agentspec.ClaudeCodeSession, bool) {
		if ag.Name != officer.Name {
			return agentspec.ClaudeCodeSession{}, false
		}
		return agentspec.ClaudeCodeSession{AllowMCPTools: []string{"officer_brief"}, BypassPermissions: true}, true
	})
	spec, err := a.buildSpec(context.Background(), officer)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := envVal(spec, podagent.EnvPermissionMode); !ok || v != "bypassPermissions" {
		t.Fatalf("officer: %s = %q (present %v), want bypassPermissions", podagent.EnvPermissionMode, v, ok)
	}
	wspec, err := a.buildSpec(context.Background(), worker)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := envVal(wspec, podagent.EnvPermissionMode); ok {
		t.Fatalf("a task agent's spec carries %s=%q", podagent.EnvPermissionMode, v)
	}
}

// TestAnOfficerSessionDisablesAutoMemoryAndNoOtherAgentsDoes: a session source
// answering DisableAutoMemory (the officer runtime, for every officer
// instance) puts CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 and
// CLAUDE_CODE_DISABLE_ORG_MEMORY=1 in THAT agent's built spec
// — the pod env the CLI inherits through ccd and tmux — and neither a task
// agent the source does not answer for nor a session the source answers
// WITHOUT it (the chief's shape: HQMCP) carries either variable (⚠ that half
// is an INVARIANT guard).
func TestAnOfficerSessionDisablesAutoMemoryAndNoOtherAgentsDoes(t *testing.T) {
	officer := ccRow("officer-triage-example-web-1")
	chiefish := ccRow("chief-shaped")
	worker := ccRow("plain-worker")
	a := adapterWith(t, ccSpecConfig(), officer)
	a.UseClaudeCodeSession(func(ag agents.Agent) (agentspec.ClaudeCodeSession, bool) {
		switch ag.Name {
		case officer.Name:
			return agentspec.ClaudeCodeSession{AllowMCPTools: []string{"officer_brief"}, BypassPermissions: true, DisableAutoMemory: true}, true
		case chiefish.Name:
			return agentspec.ClaudeCodeSession{HQMCP: true}, true
		}
		return agentspec.ClaudeCodeSession{}, false
	})
	spec, err := a.buildSpec(context.Background(), officer)
	if err != nil {
		t.Fatal(err)
	}
	// Both of the CLI's memory switches: auto-memory, and org memory — a
	// separate gate that does not read the auto-memory variable.
	memoryVars := []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CODE_DISABLE_ORG_MEMORY"}
	for _, name := range memoryVars {
		if v, ok := envVal(spec, name); !ok || v != "1" {
			t.Errorf("officer: %s = %q (present %v), want 1", name, v, ok)
		}
	}
	for _, other := range []agents.Agent{worker, chiefish} {
		ospec, err := a.buildSpec(context.Background(), other)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range memoryVars {
			if v, ok := envVal(ospec, name); ok {
				t.Errorf("INVARIANT: %s's spec carries %s=%q", other.Name, name, v)
			}
		}
	}
}
