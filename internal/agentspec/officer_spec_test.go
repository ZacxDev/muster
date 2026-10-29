package agentspec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestAnOfficerInstancesPodSpecCarriesNoOfficerToken builds the pod spec of an
// officer instance agent exactly as the officer runtime creates it — a
// claude-code agent named officers.InstanceName, whose pending first turn is
// officers.KickoffText — and asserts the officer's HQ token appears nowhere in
// it: env, secrets, files (contents included), args, init, sidecar.
//
// ⚠ AN INVARIANT GUARD, NOT REGRESSION COVERAGE. Nothing on the spec path is
// ever handed the officer token (it lives in muster-server's officers.Officer
// and is spent by officers.Client alone), so no prior code violated this. It
// pins the property so a future change that routes an officer's credential
// into its pod — the design the security model forbids — reddens here. The
// positive control below proves the scan can see a value that IS in the spec.
func TestAnOfficerInstancesPodSpecCarriesNoOfficerToken(t *testing.T) {
	const tok = "fixture-officer-token-specscan-5be1"
	o := officers.Officer{ID: "release:example-web", HQURL: "https://hq.example.com/api/v1", MaxInstances: 1}.WithToken(tok)
	a := ccAgent()
	a.Name = officers.InstanceName(o.ID, 1)
	a.DisplayName = "officer " + o.ID + " #1"
	a.NoteText = officers.KickoffText(o, 1)
	a.PendingNote = a.NoteText
	spec := mustBuild(t, a, ccConfig(), Options{})

	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var files strings.Builder
	for _, f := range spec.Files {
		files.Write(f.Content)
	}
	if spec.Sidecar != nil {
		for _, f := range spec.Sidecar.Files {
			files.Write(f.Content)
		}
	}
	haystack := string(b) + files.String() + fmt.Sprintf("%+v", spec)
	// Positive control: the agent's own Claude account token IS in the spec
	// (as a secret), so the scan can find a credential that is there.
	if !strings.Contains(haystack, ccTokenBeta) {
		t.Fatal("positive control failed: the scan does not see the account token the spec carries — it proves nothing")
	}
	if strings.Contains(haystack, tok) {
		t.Fatal("the officer token reached the officer instance's pod spec")
	}
	if strings.Contains(officers.KickoffText(o, 1), tok) {
		t.Fatal("the officer token is in the kickoff text")
	}
}

// TestAnEgressLockedSessionLosesThePublicRuleAndSkipsNonessentialTraffic: the
// spec half of the officer egress lock. A session with BypassPermissions (an
// officer instance) gets a Network with NO public port — DNS and the connect
// port only — and CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1. Every other
// claude-code agent keeps public 443 and no such variable, INCLUDING an agent
// whose job lanes bypass (MUSTER_AGENT_CC_BYPASS_POOLS), which is a different
// mechanism and out of this lock's scope.
func TestAnEgressLockedSessionLosesThePublicRuleAndSkipsNonessentialTraffic(t *testing.T) {
	cfg := ccConfig()
	cfg.ConnectURL, cfg.ConnectPort = "http://muster.example.test:18191", 18191

	locked := mustBuild(t, ccAgent(), cfg, Options{ClaudeCodeSession: ClaudeCodeSession{BypassPermissions: true}})
	if want := (provision.Network{Isolate: true, ControllerEgressTCPPorts: []int{18191}}); !reflect.DeepEqual(locked.Network, want) {
		t.Errorf("locked network = %+v, want %+v", locked.Network, want)
	}
	if v, ok := envValue(locked, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"); !ok || v != "1" {
		t.Errorf("locked pod: CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = %q (set %v), want \"1\"", v, ok)
	}

	jobBypass := cfg
	cc := *cfg.ClaudeCode
	cc.BypassPools = []string{"reviewers"}
	jobBypass.ClaudeCode = &cc
	pooled := ccAgent()
	pooled.WorkerPool = "reviewers"
	for name, spec := range map[string]provision.Spec{
		"task agent":           mustBuild(t, ccAgent(), cfg, Options{}),
		"session, no bypass":   mustBuild(t, ccAgent(), cfg, Options{ClaudeCodeSession: ClaudeCodeSession{AllowMCPTools: []string{"officer_claim"}}}),
		"job-lane bypass pool": mustBuild(t, pooled, jobBypass, Options{}),
	} {
		if want := (provision.Network{Isolate: true, PublicEgressTCPPorts: []int{443}, ControllerEgressTCPPorts: []int{18191}}); !reflect.DeepEqual(spec.Network, want) {
			t.Errorf("%s: network = %+v, want %+v", name, spec.Network, want)
		}
		if _, ok := envValue(spec, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"); ok {
			t.Errorf("%s: carries CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", name)
		}
	}
	if _, ok := envValue(mustBuild(t, pooled, jobBypass, Options{}), EnvCCDJobPermissionMode); !ok {
		t.Fatal("positive control: the pooled agent is not actually job-lane bypassed, so its case proves nothing")
	}
}
