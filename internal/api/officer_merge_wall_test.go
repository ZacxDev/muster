package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/officership"
	"github.com/ZacxDev/muster/internal/provision"
)

// 🔴 muster's gates are the only wall against an officer App merging (no
// branch ruleset is assumed). A shepherd and an officer with no `ship` (a
// read-only auditor) are OFFERED no merge, tag or own-mark-ready tool, in any
// pool, and a direct call is refused twice over: by the tool layer, and by
// the stage gate under it.

var mergeTools = []string{officers.ToolMergePR, officers.ToolTagRelease, officers.ToolMarkReady, officers.ToolRolloutStatus}

// TestNoMergeToolIsOfferedToAShepherdOrAnAuditor enumerates ToolOffered — THE
// rule MCPToolDefsFor filters by — for a shepherd and a no-ship officer, each
// untiered and as its judgement and routine pools; the merge-stage officer is
// the control (it IS offered them).
func TestNoMergeToolIsOfferedToAShepherdOrAnAuditor(t *testing.T) {
	shepherd := &officers.Ship{Stage: officers.StageShepherd}
	tiers := &officers.Tiers{Routine: officers.TierPool{Model: "haiku", MaxInstances: 1}, Judgement: officers.TierPool{Model: "opus", MaxInstances: 1}}
	for _, o := range []officers.Officer{
		{ID: "shepherd:example-infra", Ship: shepherd},
		{ID: "shepherd:example-infra", Ship: shepherd, Tiers: tiers, Tier: officers.TierJudgement},
		{ID: "shepherd:example-infra", Ship: shepherd, Tiers: tiers, Tier: officers.TierRoutine},
		{ID: "audit:example-infra"},
		{ID: "audit:example-infra", Tiers: tiers, Tier: officers.TierJudgement},
		{ID: "audit:example-infra", Tiers: tiers, Tier: officers.TierRoutine},
	} {
		for _, tool := range mergeTools {
			if officers.ToolOffered(o, tool) {
				t.Errorf("%s (ship %v, tier %q) is offered %s", o.ID, o.Ship != nil, o.Tier, tool)
			}
		}
	}
	merger := officers.Officer{ID: "engineering:example-web", Ship: &officers.Ship{Stage: officers.StageMerge}}
	if !officers.ToolOffered(merger, officers.ToolMergePR) {
		t.Fatal("control: a merge-stage officer is not offered officer_merge_pr")
	}
}

// TestTheShepherdsToolListAndDirectCalls: the shepherd instance's actual MCP
// tool list holds none of the merge tools; a direct call of each is refused
// by the tool layer; and the stage gate refuses its runtime's MergePR,
// TagRelease and MarkReady by itself.
func TestTheShepherdsToolListAndDirectCalls(t *testing.T) {
	r := newShepAPIRig(t)
	got := officerToolNameSet(r.s.MCPToolDefsFor(context.Background(), r.inst))
	if !got[officers.ToolShepherdReady] {
		t.Fatal("control: the shepherd's tool list lacks its own tool, so it may be empty")
	}
	for _, tool := range mergeTools {
		if got[tool] {
			t.Errorf("the shepherd is offered %s", tool)
		}
		if out := r.s.dispatchAgentTool(context.Background(), r.inst, tool, `{"pr":5}`); !strings.Contains(out, "is not one of officer "+offShep+"'s tools") {
			t.Errorf("a direct %s call: %s, want the tool-layer refusal", tool, out)
		}
	}
	rt := r.s.officerRT.ships[offShep]
	// Each call must stop AT the stage gate; one that gets past it (and may
	// then fault on this fixture's missing pieces) is reported as such.
	stage := func(name string, call func() error) {
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = errors.New("got past the stage gate (then faulted)")
				}
			}()
			err = call()
		}()
		if rf, ok := officership.AsRefusal(err); !ok || rf.Gate != officership.GateStage {
			t.Errorf("%s on the shepherd's runtime: %v, want the stage gate", name, err)
		}
	}
	stage("MergePR", func() error { _, err := rt.gates.MergePR(context.Background(), 5); return err })
	stage("TagRelease", func() error {
		_, err := rt.gates.TagRelease(context.Background(), officership.TagOptions{})
		return err
	})
	stage("MarkReady", func() error { _, err := rt.gates.MarkReady(context.Background(), 5); return err })
}

// TestANoShipOfficersToolListAndDirectCalls: an officer with no `ship` (a
// read-only auditor) is offered none of the merge tools, and a direct call is
// refused.
func TestANoShipOfficersToolListAndDirectCalls(t *testing.T) {
	const id, tok = "audit:example-infra", "fixture-officer-token-audit-6d2a"
	hq := hqtest.New(map[string]string{tok: id})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: &officerProv{store: store, phase: map[string]provision.Phase{}}, Gateway: &repromptChat{},
		SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	o := officers.Officer{ID: id, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(tok)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession, NoShipResume: true}); err != nil {
		t.Fatal(err)
	}
	a := agents.Agent{ID: 971, Name: officers.InstanceName(id, 1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
	store.mu.Lock()
	store.rows = append(store.rows, a)
	store.mu.Unlock()
	got := officerToolNameSet(s.MCPToolDefsFor(context.Background(), a))
	if !got[officers.ToolClaim] {
		t.Fatal("control: the auditor's tool list lacks officer_claim, so it may be empty")
	}
	for _, tool := range append(mergeTools, officers.ToolShepherdReady, officers.ToolShepherdMergeAsk) {
		if got[tool] {
			t.Errorf("the no-ship officer is offered %s", tool)
		}
		if out := s.dispatchAgentTool(context.Background(), a, tool, `{"pr":5}`); !strings.Contains(out, "is not one of officer "+id+"'s tools") {
			t.Errorf("a direct %s call: %s, want the tool-layer refusal", tool, out)
		}
	}
}
