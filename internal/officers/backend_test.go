package officers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// narrowed is a backend that advertises caps and implements ONLY the core
// verbs (embedding the interface hides every optional method).
type narrowed struct {
	officers.Backend
	caps officers.Capabilities
}

func (n narrowed) Capabilities() officers.Capabilities { return n.caps }

// hqNarrowed is HQ — every optional method still implemented — advertising
// only caps: the "implements but does not advertise" side of Serves.
type hqNarrowed struct {
	*officers.Client
	caps officers.Capabilities
}

func (n hqNarrowed) Capabilities() officers.Capabilities { return n.caps }

func hqClient() *officers.Client {
	return officers.NewClient(officers.Officer{ID: "release:example-web", HQURL: "http://hq.invalid/api/v1"}.WithToken("fixture-tok"), nil)
}

// TestHQServesEveryCapabilityItAdvertises: HQ advertises every capability
// muster knows (it has always been offered every tool its officer's shape
// allows) and implements the interface of each — the relationship
// backendtest asserts of every backend.
func TestHQServesEveryCapabilityItAdvertises(t *testing.T) {
	c := hqClient()
	caps := c.Capabilities()
	if caps.Contract != officers.ContractV1 {
		t.Fatalf("contract %q, want %q", caps.Contract, officers.ContractV1)
	}
	all := officers.AllCapabilities()
	if len(all) < 16 {
		t.Fatalf("AllCapabilities lists %d, want every one (>= 16): %v", len(all), all)
	}
	for _, v := range all {
		if !caps.Has(v) {
			t.Errorf("HQ does not advertise %s", v)
		}
	}
	if u := officers.Unimplemented(c); len(u) != 0 {
		t.Fatalf("HQ advertises without implementing: %v", u)
	}
	// positive control: a backend that advertises what it does not implement
	if u := officers.Unimplemented(narrowed{c, officers.NewCapabilities(officers.CapObs, officers.CapTiers)}); len(u) != 1 || u[0] != officers.CapObs {
		t.Fatalf("Unimplemented(narrowed obs+tiers) = %v, want [obs] (tiers is a core feature, no interface)", u)
	}
}

// TestServesNeedsTheCapabilityAndTheInterface: Serves hands out an optional
// verb only when the backend both advertises it and implements it.
func TestServesNeedsTheCapabilityAndTheInterface(t *testing.T) {
	c := hqClient()
	if _, err := officers.Serves[officers.ObsBackend](c, officers.CapObs); err != nil {
		t.Fatalf("HQ obs: %v", err)
	}
	for name, b := range map[string]officers.Backend{
		"not advertised":  hqNarrowed{c, officers.NewCapabilities()},
		"not implemented": narrowed{c, officers.NewCapabilities(officers.CapObs)},
	} {
		_, err := officers.Serves[officers.ObsBackend](b, officers.CapObs)
		if !errors.Is(err, officers.ErrNotServed) || !strings.Contains(err.Error(), officers.CapObs) {
			t.Errorf("%s: Serves = %v, want ErrNotServed naming %s", name, err, officers.CapObs)
		}
	}
}

// TestToolServedIsShapeAndCapability: a tool is served only when the
// officer's shape offers it AND its backend serves the tool's capability;
// a core tool needs no capability, and an unclassified name is never served.
func TestToolServedIsShapeAndCapability(t *testing.T) {
	rel := officers.Officer{ID: "release:example-web"}
	tri := officers.Officer{ID: "triage:example-web"}
	all := hqClient().Capabilities()
	none := officers.NewCapabilities()
	cases := []struct {
		o    officers.Officer
		caps officers.Capabilities
		tool string
		want bool
	}{
		{rel, all, officers.ToolReleaseEvaluation, true},
		{rel, none, officers.ToolReleaseEvaluation, false},                                                     // backend lacks x-captain
		{tri, all, officers.ToolReleaseEvaluation, false},                                                      // shape lacks it
		{rel, none, officers.ToolClaim, true},                                                                  // core
		{rel, officers.NewCapabilities(officers.CapObs), officers.ToolObsQuery, true},                          // exactly the cap
		{rel, officers.NewCapabilities(officers.CapObsRaw), officers.ToolObsQuery, false},                      // a different cap
		{rel, officers.NewCapabilities(officers.CapAsks), officers.ToolAskPerson, true},                        //
		{rel, all, "officer_not_a_tool", false},                                                                // unclassified
		{officers.Officer{ID: "gpu-health:g", Tier: officers.TierJudgement}, none, officers.ToolObsRaw, false}, //
	}
	for _, c := range cases {
		if got := officers.ToolServed(c.o, c.caps, c.tool); got != c.want {
			t.Errorf("ToolServed(%s, %v, %s) = %v, want %v", c.o.ID, c.caps.Verbs, c.tool, got, c.want)
		}
	}
}

// TestCompleteDroppingCostWorksOnAnyBackend: the cost retry is the
// contract's, not HQ's client's — it runs over the Backend interface.
func TestCompleteDroppingCostWorksOnAnyBackend(t *testing.T) {
	b := &countingComplete{}
	_, refused, err := officers.CompleteDroppingCost(context.Background(), b, "1", "i", officers.Completion{Outcome: "done", Summary: "s", Cost: &officers.ItemCost{USD: 1, From: "x"}})
	if err != nil || refused == nil || len(b.costs) != 2 || !b.costs[0] || b.costs[1] {
		t.Fatalf("err %v refused %v sent-with-cost %v: want a 400 on the costed complete, then one without the cost", err, refused, b.costs)
	}
}

type countingComplete struct {
	officers.Backend
	costs []bool
}

func (c *countingComplete) Complete(_ context.Context, _ officers.ItemID, _ string, done officers.Completion) ([]byte, error) {
	c.costs = append(c.costs, done.Cost != nil)
	if done.Cost != nil {
		return nil, &officers.HQError{Status: 400, Body: []byte(`{"code":"invalid"}`)}
	}
	return []byte(`{}`), nil
}

// TestThePushersGoThroughServes: the pools pusher (as the transcript and
// usage ones) reaches the backend's optional verb only through Serves, so a
// backend that does not serve pools answers ErrNotServed and nothing is sent.
func TestThePushersGoThroughServes(t *testing.T) {
	o := officers.Officer{ID: "release:example-web", HQURL: "http://hq.invalid/api/v1"}.WithToken("fixture-tok")
	b := officers.NewBackends(func(o officers.Officer) officers.Backend {
		return narrowed{officers.NewClient(o, nil), officers.NewCapabilities(officers.CapPools)}
	})
	err := officers.NewBackendPoolsPoster(b).PutPools(context.Background(), o, officers.PoolsReport{})
	if !errors.Is(err, officers.ErrNotServed) {
		t.Fatalf("pools to a backend that does not implement them: %v, want ErrNotServed", err)
	}
}

// TestABuiltinOfficerIsRefusedFailClosed: `backend` is hq (default) or
// builtin; a builtin officer declaring anything only an external HQ gives
// meaning to is refused naming each such field, and until the built-in
// backend is wired a clean builtin officer is refused too.
func TestABuiltinOfficerIsRefusedFailClosed(t *testing.T) {
	const id = "marketing:example"
	tok := officers.TokenEnvPrefix + officers.TokenEnvSuffix(id)
	parse := func(fields string, env map[string]string) error {
		_, err := officers.Parse(`[{"id":"`+id+`"`+fields+`}]`, func(k string) string { return env[k] }, "muster-agent-")
		return err
	}
	withTok := map[string]string{tok: "fixture-tok"}
	if err := parse(`,"hq_url":"https://hq.example.com/api/v1"`, withTok); err != nil {
		t.Fatalf("no backend (hq by default): %v", err)
	}
	if err := parse(`,"backend":"hq","hq_url":"https://hq.example.com/api/v1"`, withTok); err != nil {
		t.Fatalf(`backend "hq": %v`, err)
	}
	for _, c := range []struct {
		name, fields string
		env          map[string]string
		want         []string
	}{
		{"unknown backend", `,"backend":"hq2","hq_url":"https://hq.example.com/api/v1"`, withTok, []string{`backend "hq2" is not`}},
		{"hq_url", `,"backend":"builtin","hq_url":"https://hq.example.com/api/v1"`, nil, []string{"refuses hq_url"}},
		{"token", `,"backend":"builtin"`, withTok, []string{"refuses " + tok}},
		{"ship", `,"backend":"builtin","ship":{"stage":"draft"}`, nil, []string{"refuses ship"}},
		{"ci_logs", `,"backend":"builtin","ci_logs":{}`, nil, []string{"refuses ci_logs"}},
		{"transcript", `,"backend":"builtin","transcript":{"enabled":true}`, nil, []string{"refuses transcript"}},
		{"all at once", `,"backend":"builtin","hq_url":"https://x.example","transcript":{"enabled":true}`, withTok, []string{"hq_url", tok, "transcript"}},
		{"clean, not wired", `,"backend":"builtin"`, nil, []string{"is not available in this build yet"}},
	} {
		err := parse(c.fields, c.env)
		if err == nil {
			t.Errorf("%s: parsed, want a refusal", c.name)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %v, want it to name %q", c.name, err, w)
			}
		}
	}
}
