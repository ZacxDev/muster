package api

import (
	"bytes"
	"log"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// noAccountGW is a gateway that takes the no-account rule
// (agentgateway.Gateway.UseNoAccount).
type noAccountGW struct {
	stubGateway
	rule func(agents.Agent) bool
}

func (g *noAccountGW) UseNoAccount(f func(agents.Agent) bool) { g.rule = f }

// TestAProxiedInstanceMarksNoSharedClaudeAccount is the no-account rule at its
// seam, from the configuration: MUSTER_OFFICERS (officers.Parse) → the registry
// → EnableOfficers → the agent gateway's no-account rule. An instance of a
// pool on the LLM proxy runs on no shared Claude account although its row
// names one, so its failed turn (an upstream provider's 429, passed back by
// the proxy) must mark and move no account:
//
//   - the rule is installed on the gateway;
//   - it answers true for the routine pool on the proxy (the officer's `llm`)
//     and for a judgement pool on the proxy (tiers.judgement.llm);
//   - it answers false for a pool on Claude, for an untiered officer, and for
//     an ordinary agent;
//   - it agrees with the accounts' attribution (officerPoolOf) and the usage
//     report's account (instanceUsageAccount) for every instance.
func TestAProxiedInstanceMarksNoSharedClaudeAccount(t *testing.T) {
	list, err := officers.Parse(`[`+
		`{"id":"triage:example","hq_url":"http://hq.invalid/api/v1","llm":{"daily_budget_usd":5},`+
		`"tiers":{"routine":{"model":"vendor/pinned-model"},"judgement":{"model":"opus"}}},`+
		`{"id":"health:example","hq_url":"http://hq.invalid/api/v1","llm":{"daily_budget_usd":5},`+
		`"tiers":{"routine":{"model":"vendor/pinned-model"},"judgement":{"model":"vendor/judgement-model","llm":{"daily_budget_usd":10}}}},`+
		`{"id":"release:example","hq_url":"http://hq.invalid/api/v1",`+
		`"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}},`+
		`{"id":"engineering:example","hq_url":"http://hq.invalid/api/v1","model":"opus"}]`,
		jlEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	gw := &noAccountGW{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{},
		Kinds: stubKinds{pick: "acct-one"}, Connectors: presenceStore{}, Gateway: gw})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, LLMStore: nopLLMStore{}, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	if gw.rule == nil {
		t.Fatal("the gateway was not given the no-account rule")
	}
	for _, c := range []struct {
		name string
		none bool
	}{
		{"officer-triage-example-r1", true},
		{"officer-triage-example-1", false},
		{"officer-health-example-r1", true},
		{"officer-health-example-1", true},
		{"officer-release-example-r1", false},
		{"officer-release-example-1", false},
		{"officer-engineering-example-1", false},
	} {
		a := agents.Agent{ID: 5, Name: c.name, Kind: agents.KindClaudeCode, CCAccount: "acct-one", OwnerID: 1}
		if got := gw.rule(a); got != c.none {
			t.Errorf("%s: no-account rule %v, want %v", c.name, got, c.none)
		}
		if _, _, onNone, ok := s.officerPoolOf(c.name); !ok || onNone != c.none {
			t.Errorf("%s: attribution's no-account answer %v (officer %v), want %v", c.name, onNone, ok, c.none)
		}
		p, _, ok := s.officerRT.reg.ForAgentName(c.name)
		if !ok {
			t.Fatalf("%s: not an officer instance", c.name)
		}
		if got := instanceUsageAccount(p, a); (got == "") != c.none {
			t.Errorf("%s: usage reported on account %q, want none only for a pool on the proxy", c.name, got)
		}
	}
	if gw.rule(agents.Agent{Name: "brave-otter", CCAccount: "acct-one"}) {
		t.Error("an ordinary agent is on no account")
	}
}
