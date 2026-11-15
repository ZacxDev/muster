package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/jobs"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/provision"
)

// noAccountOfficers is a MUSTER_OFFICERS fixture with pools on the LLM proxy
// and pools on Claude:
//
//   - triage: routine on the proxy (the officer's `llm`), judgement on Claude;
//   - health: both tiers on the proxy (tiers.judgement.llm);
//   - release: both tiers on Claude;
//   - engineering: untiered, on Claude.
const noAccountOfficers = `[` +
	`{"id":"triage:example","hq_url":"http://hq.invalid/api/v1","llm":{"daily_budget_usd":5},` +
	`"tiers":{"routine":{"model":"vendor/pinned-model"},"judgement":{"model":"opus"}}},` +
	`{"id":"health:example","hq_url":"http://hq.invalid/api/v1","llm":{"daily_budget_usd":5},` +
	`"tiers":{"routine":{"model":"vendor/pinned-model"},"judgement":{"model":"vendor/judgement-model","llm":{"daily_budget_usd":10}}}},` +
	`{"id":"release:example","hq_url":"http://hq.invalid/api/v1",` +
	`"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}},` +
	`{"id":"engineering:example","hq_url":"http://hq.invalid/api/v1","model":"opus"}]`

// noAccountServer is a server with noAccountOfficers enabled and, when routed
// is non-nil, the chief on the LLM proxy under that route predicate.
func noAccountServer(t *testing.T, ext Extensions, routed func(context.Context, agents.Agent) (bool, error)) (*Server, *bytes.Buffer) {
	t.Helper()
	list, err := officers.Parse(noAccountOfficers, jlEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(logs, "", 0))
	ext.Agents, ext.Provisioner, ext.SessionLiveness = &officerAgents{}, stubProvisioner{}, stubLiveness{}
	ext.Kinds, ext.Connectors = stubKinds{pick: "acct-one"}, presenceStore{}
	s.UseExtensions(ext)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, LLMStore: nopLLMStore{}, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	if routed != nil {
		if err := s.EnableChiefLLM(ChiefLLM{Key: "sk-fake-chief", Routed: routed}); err != nil {
			t.Fatal(err)
		}
	}
	return s, logs
}

// TestAProxiedInstanceMarksNoSharedClaudeAccount is the no-account rule's
// officer half, from the configuration: MUSTER_OFFICERS (officers.Parse) →
// the registry → EnableOfficers → Server.NoClaudeAccount. An instance of a
// pool on the LLM proxy runs on no shared Claude account although its row
// names one, so its failed turn (an upstream provider's 429, passed back by
// the proxy) must mark and move no account:
//
//   - true for the routine pool on the proxy (the officer's `llm`) and for a
//     judgement pool on the proxy (tiers.judgement.llm);
//   - false for a pool on Claude, for an untiered officer, and for an
//     ordinary agent;
//   - it agrees with the usage report's account (instanceUsageAccount) for
//     every instance.
func TestAProxiedInstanceMarksNoSharedClaudeAccount(t *testing.T) {
	s, _ := noAccountServer(t, Extensions{}, nil)
	ctx := context.Background()
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
		if got := s.NoClaudeAccount(ctx, a); got != c.none {
			t.Errorf("%s: no-account rule %v, want %v", c.name, got, c.none)
		}
		p, _, ok := s.officerRT.reg.ForAgentName(c.name)
		if !ok {
			t.Fatalf("%s: not an officer instance", c.name)
		}
		if got := instanceUsageAccount(p, a); (got == "") != c.none {
			t.Errorf("%s: usage reported on account %q, want none only for a pool on the proxy", c.name, got)
		}
	}
	if s.NoClaudeAccount(ctx, agents.Agent{Name: "brave-otter", Kind: agents.KindClaudeCode, CCAccount: "acct-one"}) {
		t.Error("an ordinary agent is on no account")
	}
}

// TestARoutedChiefMarksNoSharedClaudeAccount is the rule's chief half, read
// through the agent gateway it is installed on (agentgateway.Gateway.AccountOf,
// the account noteFailure marks; cmd/muster-server installs
// Server.NoClaudeAccount there). The chief on the LLM route keeps its row's
// account (the spec builder places no token for it), so a provider's 429
// through the proxy must mark none:
//
//   - the routed chief: no account;
//   - control: the same chief when the route predicate says no, and when no
//     chief is on the proxy at all: its row's account, marked as before;
//   - a route that cannot be decided: no account (the safe side, logged).
func TestARoutedChiefMarksNoSharedClaudeAccount(t *testing.T) {
	chief := agents.Agent{ID: 9, Name: agents.ChiefName, Kind: agents.KindClaudeCode, CCAccount: "acct-one", OwnerID: 1}
	routeErr := errors.New("credential plane unreachable")
	for _, c := range []struct {
		name   string
		routed func(context.Context, agents.Agent) (bool, error)
		want   string
		logged bool
	}{
		{"routed", func(_ context.Context, a agents.Agent) (bool, error) { return a.ID == chief.ID, nil }, "", false},
		{"not routed (control)", func(context.Context, agents.Agent) (bool, error) { return false, nil }, "acct-one", false},
		{"no chief on the proxy (control)", nil, "acct-one", false},
		{"route undecided", func(context.Context, agents.Agent) (bool, error) { return false, routeErr }, "", true},
	} {
		s, logs := noAccountServer(t, Extensions{}, c.routed)
		gw, err := agentgateway.New(agentgateway.Config{Driver: outcomeEndpoint{provision.Endpoint{Scheme: "http", Host: "127.0.0.1", Port: 1}},
			Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel"})
		if err != nil {
			t.Fatal(err)
		}
		gw.UseNoAccount(s.NoClaudeAccount)
		if got := gw.AccountOf(context.Background(), chief); got != c.want {
			t.Errorf("%s: the gateway marks the chief's failures against %q, want %q", c.name, got, c.want)
		}
		if got := strings.Contains(logs.String(), routeErr.Error()); got != c.logged {
			t.Errorf("%s: route error logged %v, want %v:\n%s", c.name, got, c.logged, logs.String())
		}
	}
}

// TestAJobOnAWorkerOnTheLLMProxyIsNotFailedOver: failoverJob, for a job whose
// turn ended rate_limited on a worker on no shared Claude account (a proxied
// officer instance, the routed chief), does not call the account failover: a
// move would restart the pod and drop its claims onto an account it does not
// use either. Control: a worker on a Claude pool, and an ordinary agent, are
// handed to the failover as before.
func TestAJobOnAWorkerOnTheLLMProxyIsNotFailedOver(t *testing.T) {
	routed := func(_ context.Context, a agents.Agent) (bool, error) { return a.Name == agents.ChiefName, nil }
	for _, c := range []struct {
		name  string
		moved bool
	}{
		{"officer-triage-example-r1", false},
		{"officer-health-example-1", false},
		{agents.ChiefName, false},
		{"officer-triage-example-1", true},
		{"brave-otter", true},
	} {
		a := agents.Agent{ID: 7, Name: c.name, Kind: agents.KindClaudeCode, CCAccount: "acct-one", OwnerID: 1}
		ags := &failoverAgents{row: a}
		fo := &fakeFailover{ags: ags, targets: map[string]string{"acct-one": "acct-two"}}
		s, _ := noAccountServer(t, Extensions{AccountFailover: fo, Jobs: newMemJobs()}, routed)
		s.failoverJob(context.Background(), jobs.Job{ID: 1}, a,
			jobs.Outcome{Status: jobs.StatusFailed, Reason: ccpool.FailureRateLimited, Error: "provider 429"})
		want := map[bool]string{true: "acct-one:rate_limited", false: ""}[c.moved]
		if got := strings.Join(fo.calls, ","); got != want {
			t.Errorf("%s: the account failover was called with %q, want %q", c.name, got, want)
		}
	}
}
