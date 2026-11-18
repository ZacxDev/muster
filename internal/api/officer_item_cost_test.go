package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// officer_complete CARRIES THE ITEM'S COST (civ-hq docs/OFFICERS.md "Item
// cost"): a routine instance's complete sends cost_usd = what its hold on the
// item booked through the LLM proxy, cost_from = muster:llm-proxy, in the one
// complete call. The cost is final by then: every request the hold made was
// booked before its response ended, and the complete is the hold's last act.
//
//   - An unknown cost (the meter did not see the whole hold) sends NEITHER
//     field: never a 0.
//   - A judgement instance's Claude Code session has no cost source muster
//     reads, so its complete sends none.
//   - A complete HQ refuses BECAUSE of the cost (an HQ from before item cost
//     refuses unknown fields) is sent again without it: the cost never fails
//     a complete.
// ---------------------------------------------------------------------------

const (
	costOff     = "triage:example-cost"
	costTok     = "tok-cost-officer-7d41" // a fixture, nobody's token
	costRoutine = "officer-triage-example-cost-r"
	costJudge   = "officer-triage-example-cost-"
)

// costProvider is a stub provider that answers each request with the next
// cost in its queue, as OpenRouter reports it (usage.cost in message_delta).
type costProvider struct {
	*httptest.Server
	mu    sync.Mutex
	costs []float64
}

func (p *costProvider) queue(costs ...float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.costs = append(p.costs, costs...)
}

func newCostProvider(t *testing.T) *costProvider {
	t.Helper()
	p := &costProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p.mu.Lock()
		if len(p.costs) == 0 {
			p.mu.Unlock()
			http.Error(w, "no cost queued", http.StatusInternalServerError)
			return
		}
		cost := p.costs[0]
		p.costs = p.costs[1:]
		p.mu.Unlock()
		ev := func(typ string, data any) {
			b, _ := json.Marshal(data)
			_, _ = io.WriteString(w, "event: "+typ+"\ndata: "+string(b)+"\n\n")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_cost", "model": "vendor/pinned-model",
			"usage": map[string]any{"input_tokens": 100, "output_tokens": 1}}})
		ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"output_tokens": 5, "cost": cost}})
		ev("message_stop", map[string]any{"type": "message_stop"})
	}))
	t.Cleanup(p.Server.Close)
	return p
}

type itemCostRig struct {
	s     *Server
	hq    *hqtest.Server
	up    *costProvider
	store *officerAgents
	logs  *bytes.Buffer
}

// newItemCostRig is one tiered officer with `llm` (routine ×2 on the proxy,
// judgement ×1) against the HQ fake and a stub provider.
func newItemCostRig(t *testing.T) *itemCostRig {
	t.Helper()
	return newItemCostRigWith(t, `"tiers":{"routine":{"model":"vendor/pinned-model","max_instances":2},"judgement":{"model":"opus","max_instances":1}},`+
		`"llm":{"daily_budget_usd":5}`)
}

// newItemCostRigWith is newItemCostRig with the officer's tiers (and llm)
// fields as given.
func newItemCostRigWith(t *testing.T, fields string) *itemCostRig {
	t.Helper()
	r := &itemCostRig{hq: hqtest.New(map[string]string{costTok: costOff}), up: newCostProvider(t), logs: &bytes.Buffer{}}
	t.Cleanup(r.hq.Close)
	list, err := officers.Parse(`[{"id":"`+costOff+`","hq_url":"`+r.hq.BaseURL()+`",`+fields+`}]`, func(k string) string {
		switch {
		case k == officers.TokenEnvPrefix+officers.TokenEnvSuffix(costOff):
			return costTok
		case strings.HasPrefix(k, officers.LLMKeyEnvPrefix):
			return "fixture-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	r.store = &officerAgents{}
	for i, name := range []string{costRoutine + "1", costRoutine + "2", costJudge + "1"} {
		r.store.rows = append(r.store.rows, agents.Agent{ID: int64(4401 + i), Name: name, DisplayName: "officer", Kind: agents.KindClaudeCode,
			Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"})
	}
	r.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(r.logs, "", 0))
	r.s.UseExtensions(Extensions{Agents: r.store, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{},
		Kinds: stubKinds{pick: "acct-one"}, Connectors: &chiefLLMAudit{}})
	if _, err := r.s.EnableOfficers(OfficersConfig{Officers: list, LLMStore: nopLLMStore{}, LLMUpstream: r.up.URL + "/api",
		InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *itemCostRig) agent(t *testing.T, name string) agents.Agent {
	t.Helper()
	for _, a := range r.store.rows {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no agent %s", name)
	return agents.Agent{}
}

// llm makes one request through the proxy's route as agent a (past the
// connector door, which is officer_llm_pg_test.go's), answered at the cost
// queued for it.
func (r *itemCostRig) llm(t *testing.T, a agents.Agent, cost float64) {
	t.Helper()
	r.up.queue(cost)
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages?beta=true", strings.NewReader(llmClientBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), agentCtxKey{}, a))
	w := httptest.NewRecorder()
	r.s.serveLLMMessages(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("llm request as %s: %d %s", a.Name, w.Code, w.Body)
	}
}

func (r *itemCostRig) tool(t *testing.T, a agents.Agent, name, args string) string {
	t.Helper()
	return r.s.dispatchAgentTool(context.Background(), a, name, args)
}

func (r *itemCostRig) claim(t *testing.T, a agents.Agent, item string) {
	t.Helper()
	if out := r.tool(t, a, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("%s claims %s: %s", a.Name, item, out)
	}
}

// complete completes item as a and returns the raw bodies of the complete
// requests HQ saw for it.
func (r *itemCostRig) complete(t *testing.T, a agents.Agent, item, outcome string) []string {
	t.Helper()
	r.hq.ResetRequests()
	if out := r.tool(t, a, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"`+outcome+`","summary":"checked"}`); isToolErr(out) {
		t.Fatalf("%s completes %s: %s", a.Name, item, out)
	}
	var bodies []string
	for _, q := range completeRequests(r.hq) {
		bodies = append(bodies, string(q.Raw))
	}
	return bodies
}

// omitted asserts the one log line a complete sent without a cost writes,
// pinned whole: officer, instance, item, outcome and the reason.
func (r *itemCostRig) omitted(t *testing.T, inst, item, outcome, reason string) {
	t.Helper()
	want := "officers: item cost omitted officer=" + costOff + " instance=" + inst + " item=" + item + " outcome=" + outcome + " reason=" + reason + "\n"
	if n := strings.Count(r.logs.String(), want); n != 1 {
		t.Errorf("the omitted-cost line %q appears %d times, want once; logs:\n%s", want, n, r.logs)
	}
}

// bodyWith is the complete body muster sends for instance inst: cost, when
// not "", is the cost fields' literal JSON prefix.
func bodyWith(cost, inst, outcome string) string {
	return `{` + cost + `"instance":"` + inst + `","links":null,"outcome":"` + outcome + `","summary":"checked"}`
}

// TestARoutineCompleteCarriesTheItemsProxyCost: r1 holds item A and makes two
// requests (0.0137, 0.0291); r2 holds item B and makes one (0.0419) in
// between. A's complete sends cost_usd 0.0428 — its own requests, not B's —
// and B's sends 0.0419; both name muster:llm-proxy, and HQ stores each.
func TestARoutineCompleteCarriesTheItemsProxyCost(t *testing.T) {
	r := newItemCostRig(t)
	r1, r2 := r.agent(t, costRoutine+"1"), r.agent(t, costRoutine+"2")
	a := r.hq.AddTieredItem(costOff, "web.check", "node-a", officers.TierRoutine)
	b := r.hq.AddTieredItem(costOff, "web.check", "node-b", officers.TierRoutine)

	r.claim(t, r1, a)
	r.llm(t, r1, 0.0137)
	r.claim(t, r2, b)
	r.llm(t, r2, 0.0419)
	r.llm(t, r1, 0.0291)

	if got, want := r.complete(t, r1, a, "done"), bodyWith(`"cost_from":"muster:llm-proxy","cost_usd":0.0428,`, r1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("A's complete bodies\n got %q\nwant [%s]", got, want)
	}
	if got, want := r.complete(t, r2, b, "no_action"), bodyWith(`"cost_from":"muster:llm-proxy","cost_usd":0.0419,`, r2.Name, "no_action"); len(got) != 1 || got[0] != want {
		t.Errorf("B's complete bodies\n got %q\nwant [%s]", got, want)
	}
	if strings.Contains(r.logs.String(), "item cost omitted") {
		t.Errorf("a complete WITH its cost logged an omitted-cost line: %s", r.logs)
	}
	for item, want := range map[string]float64{a: 0.0428, b: 0.0419} {
		it := r.hq.Item(item)
		if it.CostUSD == nil || *it.CostUSD != want || it.CostFrom == nil || *it.CostFrom != officers.CostFromLLMProxy {
			t.Errorf("HQ stored item %s's cost as %v from %v, want %v from %s", item, it.CostUSD, it.CostFrom, want, officers.CostFromLLMProxy)
		}
	}
}

// TestAnUnknownItemCostIsOmittedNotZero: a hold the meter did not see whole —
// HQ's second claim of the item, or one that booked no request — completes
// with NEITHER cost field (not 0, not null), and HQ stores no cost.
func TestAnUnknownItemCostIsOmittedNotZero(t *testing.T) {
	r := newItemCostRig(t)
	r1, r2 := r.agent(t, costRoutine+"1"), r.agent(t, costRoutine+"2")

	// HQ's second claim: r1 claims and releases it, r2 claims it (attempts 2).
	second := r.hq.AddTieredItem(costOff, "web.check", "node-second", officers.TierRoutine)
	r.claim(t, r1, second)
	r.llm(t, r1, 0.0173)
	if out := r.tool(t, r1, officers.ToolRelease, `{"item_id":"`+second+`"}`); isToolErr(out) {
		t.Fatalf("release: %s", out)
	}
	r.claim(t, r2, second)
	r.llm(t, r2, 0.0259)
	if got, want := r.complete(t, r2, second, "done"), bodyWith("", r2.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("HQ's second claim: complete bodies\n got %q\nwant [%s]", got, want)
	}
	r.omitted(t, r2.Name, second, "done", "not_first_claim")

	// A first claim with no request booked: no evidence of a cost.
	none := r.hq.AddTieredItem(costOff, "web.check", "node-none", officers.TierRoutine)
	r.claim(t, r1, none)
	if got, want := r.complete(t, r1, none, "done"), bodyWith("", r1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("no request booked: complete bodies\n got %q\nwant [%s]", got, want)
	}
	r.omitted(t, r1.Name, none, "done", "no_requests")
	for _, item := range []string{second, none} {
		if it := r.hq.Item(item); it.CostUSD != nil || it.CostFrom != nil {
			t.Errorf("item %s: HQ stored cost %v from %v, want none", item, it.CostUSD, it.CostFrom)
		}
	}
}

// TestAnAlreadyYoursReclaimTheMeterDidNotHoldKnowsNoCost: r1 claims an item
// (HQ's first claim) and books a request; the meter then forgets the hold, as
// a restart of muster does. r1's claim of its own live item is muster's
// already-yours answer — a heartbeat whose item still reads attempts 1 — so
// the meter holds it again, but not whole: the request before the restart is
// not in it. The complete sends no cost, not the partial sum.
func TestAnAlreadyYoursReclaimTheMeterDidNotHoldKnowsNoCost(t *testing.T) {
	r := newItemCostRig(t)
	r1 := r.agent(t, costRoutine+"1")
	item := r.hq.AddTieredItem(costOff, "web.check", "node-restart", officers.TierRoutine)
	r.claim(t, r1, item)
	r.llm(t, r1, 0.0233)
	r.s.officerRT.llm.Ended(costOff, "routine", 1, item) // the meter forgets the hold (a restart)
	if out := r.tool(t, r1, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) || !strings.Contains(out, "already_yours") {
		t.Fatalf("r1 re-claims its own live item: %s, want muster's already-yours answer", out)
	}
	r.llm(t, r1, 0.0389)
	if got, want := r.complete(t, r1, item, "done"), bodyWith("", r1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("complete bodies\n got %q\nwant [%s]", got, want)
	}
	r.omitted(t, r1.Name, item, "done", "attempts_unknown")
}

// TestACostAboveHQsBoundIsOmitted: a hold that booked 12345.6789 USD (above
// HQ's 10000, which HQ refuses as a reporting error) completes with ONE
// request and no cost fields — not a costed complete HQ refuses and a retry.
func TestACostAboveHQsBoundIsOmitted(t *testing.T) {
	r := newItemCostRig(t)
	r1 := r.agent(t, costRoutine+"1")
	item := r.hq.AddTieredItem(costOff, "web.check", "node-huge", officers.TierRoutine)
	r.claim(t, r1, item)
	r.llm(t, r1, 12345.6789)
	if got, want := r.complete(t, r1, item, "done"), bodyWith("", r1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("complete bodies\n got %q\nwant [%s]", got, want)
	}
	r.omitted(t, r1.Name, item, "done", "over_bound")
}

// TestACompleteHQRefusesForItsCostStillCompletes: against an HQ from before
// item cost (it refuses the unknown fields, 400), the complete is sent again
// without the cost and the item is done; the instance sees success, and the
// log says the cost was dropped.
func TestACompleteHQRefusesForItsCostStillCompletes(t *testing.T) {
	r := newItemCostRig(t)
	r.hq.PreCost = true
	r1 := r.agent(t, costRoutine+"1")
	item := r.hq.AddTieredItem(costOff, "web.check", "node-old-hq", officers.TierRoutine)
	r.claim(t, r1, item)
	r.llm(t, r1, 0.0311)

	got := r.complete(t, r1, item, "done")
	want := []string{bodyWith(`"cost_from":"muster:llm-proxy","cost_usd":0.0311,`, r1.Name, "done"), bodyWith("", r1.Name, "done")}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("complete bodies\n got %q\nwant %q", got, want)
	}
	if st := r.hq.Item(item).Status; st != "done" {
		t.Errorf("the item is %s, want done", st)
	}
	if !strings.Contains(r.logs.String(), "without its cost") {
		t.Errorf("the log does not say the cost was dropped: %s", r.logs)
	}
}

// TestAJudgementOrHandedOnCompleteSendsNoCost: a judgement instance's session
// has no cost source muster reads, so its complete sends none; a routine
// needs_judgement complete hands the item on unfinished, so it sends none
// either (the item's whole cost is not known).
func TestAJudgementOrHandedOnCompleteSendsNoCost(t *testing.T) {
	r := newItemCostRig(t)
	r1, j1 := r.agent(t, costRoutine+"1"), r.agent(t, costJudge+"1")

	handed := r.hq.AddTieredItem(costOff, "web.check", "node-handed", officers.TierRoutine)
	r.claim(t, r1, handed)
	r.llm(t, r1, 0.0457)
	if got, want := r.complete(t, r1, handed, officers.OutcomeNeedsJudgement), bodyWith("", r1.Name, officers.OutcomeNeedsJudgement); len(got) != 1 || got[0] != want {
		t.Errorf("needs_judgement: complete bodies\n got %q\nwant [%s]", got, want)
	}
	r.omitted(t, r1.Name, handed, officers.OutcomeNeedsJudgement, "handed_on")

	judged := r.hq.AddTieredItem(costOff, "web.alert", "node-judged", officers.TierJudgement)
	r.claim(t, j1, judged)
	if got, want := r.complete(t, j1, judged, "done"), bodyWith("", j1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("judgement: complete bodies\n got %q\nwant [%s]", got, want)
	}
	r.omitted(t, j1.Name, judged, "done", "no_source")
}

// TestAJudgementProxyCompleteCarriesItsOwnCost: a judgement instance on the
// proxy (tiers.judgement.llm) has the proxy as its cost source, and its meter
// is its own: routine r1 and judgement #1 share a number, not a hold, so j1's
// complete sends exactly its own request's cost.
func TestAJudgementProxyCompleteCarriesItsOwnCost(t *testing.T) {
	r := newItemCostRigWith(t, `"tiers":{"routine":{"model":"vendor/pinned-model","max_instances":2},`+
		`"judgement":{"model":"vendor/judgement-model","max_instances":1,"llm":{"daily_budget_usd":9}}},"llm":{"daily_budget_usd":5}`)
	r1, j1 := r.agent(t, costRoutine+"1"), r.agent(t, costJudge+"1")
	routine := r.hq.AddTieredItem(costOff, "web.check", "node-r", officers.TierRoutine)
	judged := r.hq.AddTieredItem(costOff, "web.alert", "node-j", officers.TierJudgement)
	r.claim(t, r1, routine)
	r.claim(t, j1, judged)
	r.llm(t, r1, 0.0119)
	r.llm(t, j1, 0.0381)
	if got, want := r.complete(t, j1, judged, "done"), bodyWith(`"cost_from":"muster:llm-proxy","cost_usd":0.0381,`, j1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("judgement proxy complete bodies\n got %q\nwant [%s]", got, want)
	}
	if got, want := r.complete(t, r1, routine, "done"), bodyWith(`"cost_from":"muster:llm-proxy","cost_usd":0.0119,`, r1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("routine complete bodies\n got %q\nwant [%s]", got, want)
	}
}
