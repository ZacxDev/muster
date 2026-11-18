package agentprovision

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE CHIEF'S BYPASS, AT THE SEAM (operator decision, PR #626): the
// claude-code chief runs every tool with no permission prompt and KEEPS public
// egress — the one exception to "no bypass without the egress lock"
// (agentspec.ClaudeCodeSession.EgressLocked). Read from BUILT specs; the
// variable and its value are spelled literally, so a mutant renaming either
// constant fails here.

// specBypassed: the pod's sessions start in bypassPermissions.
func specBypassed(t *testing.T, spec provision.Spec) bool {
	t.Helper()
	v, ok := envVal(spec, "CCD_PERMISSION_MODE")
	if ok && v != "bypassPermissions" {
		t.Errorf("CCD_PERMISSION_MODE=%q: not a value ccd seed accepts", v)
	}
	return ok
}

// specLocked: the pod's policy has neither the public-443 rule nor the
// cluster-DNS rule (agentspec claudeCodeNetworkFor's lock).
func specLocked(spec provision.Spec) bool {
	return len(spec.Network.PublicEgressTCPPorts) == 0 && spec.Network.NoClusterDNS
}

// TestBypassWithoutTheLockIsExactlyTheClaudeCodeChief asserts the three sets
// over the four agent shapes — the claude-code chief, a dispatched worker, an
// officer instance (a session source answering with the officer runtime's
// shape) and the Compose chief:
//
//   - bypassed (CCD_PERMISSION_MODE=bypassPermissions) = {chief, officer};
//   - egress-locked = {officer};
//   - so bypassed WITHOUT the lock = {chief}, exactly.
//
// The worker and the Compose chief are in neither. Each set fails if it GROWS
// or SHRINKS.
func TestBypassWithoutTheLockIsExactlyTheClaudeCodeChief(t *testing.T) {
	officer := ccRow("officer-triage-example-web-1")
	var bypassed, locked, unlockedBypass []string
	for _, shape := range []string{"chief", "worker", "officer", "compose-chief"} {
		cfg := ccSpecConfig()
		row := ccRow(agents.ChiefName)
		switch shape {
		case "worker":
			row = ccRow("plain-worker")
		case "officer":
			row = officer
		case "compose-chief":
			cfg.ComposeContainer = true
		}
		a := adapterWith(t, cfg, row)
		a.UseClaudeCodeSession(func(ag agents.Agent) (agentspec.ClaudeCodeSession, bool) {
			if ag.Name == officer.Name {
				return agentspec.ClaudeCodeSession{AllowMCPTools: []string{"officer_brief"}, BypassPermissions: true,
					DisableAutoMemory: true}, true
			}
			return agentspec.ClaudeCodeSession{}, false
		})
		spec, err := a.buildSpec(context.Background(), row)
		if err != nil {
			t.Fatalf("%s: %v", shape, err)
		}
		b, l := specBypassed(t, spec), specLocked(spec)
		if b {
			bypassed = append(bypassed, shape)
		}
		if l {
			locked = append(locked, shape)
		}
		if b && !l {
			unlockedBypass = append(unlockedBypass, shape)
		}
	}
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"bypassed", bypassed, []string{"chief", "officer"}},
		{"egress-locked", locked, []string{"officer"}},
		{"bypassed without the lock", unlockedBypass, []string{"chief"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s specs = %v, want exactly %v", c.name, c.got, c.want)
		}
	}
}

// TestTheChiefsBypassIsRefusedForAnyOtherAgent: a session source that asks for
// ChiefBypass on an agent that is not the chief (a worker, an officer) is
// refused at build — the exception cannot be widened by a second caller.
// "chief-02" is a near-miss slug agents.IsChiefName rejects (a leading 0).
// Control: the same source answering for the chief builds.
func TestTheChiefsBypassIsRefusedForAnyOtherAgent(t *testing.T) {
	for _, name := range []string{"plain-worker", "officer-triage-example-web-1", agents.ChiefName + "-02"} {
		row := ccRow(name)
		a := adapterWith(t, ccSpecConfig(), row)
		a.UseClaudeCodeSession(func(agents.Agent) (agentspec.ClaudeCodeSession, bool) {
			return agentspec.ClaudeCodeSession{ChiefBypass: true}, true
		})
		if _, err := a.buildSpec(context.Background(), row); err == nil ||
			!strings.Contains(err.Error(), "only the claude-code chief may have") {
			t.Errorf("%s asking for the chief's bypass: %v, want refused", name, err)
		}
	}
	row := ccRow(agents.ChiefName)
	if _, err := adapterWith(t, ccSpecConfig(), row).buildSpec(context.Background(), row); err != nil {
		t.Fatalf("control: the chief: %v", err)
	}
}
