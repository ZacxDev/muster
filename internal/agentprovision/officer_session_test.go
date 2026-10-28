package agentprovision

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
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
	a.UseClaudeCodeSession(func(ag agents.Agent) ([]string, string, bool) {
		if ag.Name != officer.Name {
			return nil, "", false
		}
		return []string{"officer_brief", "officer_claim"}, "opus", true
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
