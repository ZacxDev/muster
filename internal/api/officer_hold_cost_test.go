package api

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// EVERY HOLD REPORTS ITS OWN COST (civ-hq docs/OFFICERS.md "Per-hold cost"):
// whatever ends a proxied instance's hold — a complete of any outcome
// (needs_judgement's hand-on included), a release, or a lapsed lease the
// wake loop sees — sends POST …/items/{item}/hold-cost with the hold's cost
// and HQ's claim number, and HQ adds the holds of the item up. An item
// claimed more than once then carries its whole cost, which a complete's
// cost_usd (one hold's) cannot.
// ---------------------------------------------------------------------------

// holdCostsOf is the hold costs HQ recorded for item, in attempt order.
func holdCostsOf(hq *hqtest.Server, item string) []hqtest.HoldCostCall {
	all, _ := hq.HoldCosts()
	var out []hqtest.HoldCostCall
	for _, c := range all {
		if c.Item == item {
			out = append(out, c)
		}
	}
	return out
}

// wantCost fails unless HQ stores item's cost as usd from muster:llm-proxy.
func wantCost(t *testing.T, hq *hqtest.Server, item string, usd float64) {
	t.Helper()
	it := hq.Item(item)
	if it.CostUSD == nil || !nearUSD(*it.CostUSD, usd) || it.CostFrom == nil || *it.CostFrom != officers.CostFromLLMProxy {
		t.Errorf("HQ stored item %s's cost as %v from %v, want %v from %s", item, it.CostUSD, it.CostFrom, usd, officers.CostFromLLMProxy)
	}
}

func nearUSD(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// TestEveryHoldOfAnItemReportsItsCost: three items, each worked by two holds.
//   - handed on: routine r1 (0.0457) completes needs_judgement; judgement
//     #1, on the proxy, (0.0381) completes done. HQ records attempt 1 =
//     0.0457 and attempt 2 = 0.0381, and the item costs 0.0838.
//   - released: r1 (0.0173) releases; r2 (0.0259) completes. 0.0432.
//   - lapsed: r1 (0.0311) holds it until its lease lapses; r2 claims it
//     (0.0127) and completes; the wake loop's next read no longer shows r1
//     holding it, which reports r1's hold. 0.0438, whichever came last.
//
// Each complete's own cost_usd is still sent only for HQ's first claim: none
// of these second holds sends one.
func TestEveryHoldOfAnItemReportsItsCost(t *testing.T) {
	r := newItemCostRigWith(t, `"tiers":{"routine":{"model":"vendor/pinned-model","max_instances":2},`+
		`"judgement":{"model":"vendor/judgement-model","max_instances":1,"llm":{"daily_budget_usd":9}}},"llm":{"daily_budget_usd":5}`)
	r1, r2, j1 := r.agent(t, costRoutine+"1"), r.agent(t, costRoutine+"2"), r.agent(t, costJudge+"1")

	handed := r.hq.AddTieredItem(costOff, "web.check", "node-handed", officers.TierRoutine)
	r.claim(t, r1, handed)
	r.llm(t, r1, 0.0457)
	r.complete(t, r1, handed, officers.OutcomeNeedsJudgement)
	r.claim(t, j1, handed)
	r.llm(t, j1, 0.0381)
	if got, want := r.complete(t, j1, handed, "done"), bodyWith("", j1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("the judgement hold's complete bodies\n got %q\nwant [%s]: a second claim's complete carries no item cost", got, want)
	}
	if got := holdCostsOf(r.hq, handed); len(got) != 2 ||
		got[0] != (hqtest.HoldCostCall{Item: handed, Attempt: 1, Instance: r1.Name, USD: 0.0457, From: officers.CostFromLLMProxy}) ||
		got[1] != (hqtest.HoldCostCall{Item: handed, Attempt: 2, Instance: j1.Name, USD: 0.0381, From: officers.CostFromLLMProxy}) {
		t.Errorf("handed on: HQ recorded holds %+v, want r1's 0.0457 (attempt 1) and j1's 0.0381 (attempt 2)", got)
	}
	wantCost(t, r.hq, handed, 0.0838)

	released := r.hq.AddTieredItem(costOff, "web.check", "node-released", officers.TierRoutine)
	r.claim(t, r1, released)
	r.llm(t, r1, 0.0173)
	if out := r.tool(t, r1, officers.ToolRelease, `{"item_id":"`+released+`"}`); isToolErr(out) {
		t.Fatalf("release: %s", out)
	}
	r.claim(t, r2, released)
	r.llm(t, r2, 0.0259)
	r.complete(t, r2, released, "done")
	if got := holdCostsOf(r.hq, released); len(got) != 2 || got[0].Attempt != 1 || got[0].USD != 0.0173 || got[1].Attempt != 2 || got[1].USD != 0.0259 {
		t.Errorf("released: HQ recorded holds %+v, want 0.0173 (attempt 1) and 0.0259 (attempt 2)", got)
	}
	wantCost(t, r.hq, released, 0.0432)

	lapsed := r.hq.AddTieredItem(costOff, "web.check", "node-lapsed", officers.TierRoutine)
	r.claim(t, r1, lapsed)
	r.llm(t, r1, 0.0311)
	r.hq.SetClaim(lapsed, r1.Name, time.Now().Add(-time.Minute)) // r1's lease lapses
	r.claim(t, r2, lapsed)
	r.llm(t, r2, 0.0127)
	r.complete(t, r2, lapsed, "done")
	if got := holdCostsOf(r.hq, lapsed); len(got) != 1 || got[0].Attempt != 2 {
		t.Fatalf("lapsed, before the wake loop's read: HQ recorded holds %+v, want only r2's (attempt 2)", got)
	}
	if it := r.hq.Item(lapsed); it.CostUSD != nil {
		t.Errorf("lapsed, r1's hold unreported: HQ shows cost %v, want none (not known whole)", *it.CostUSD)
	}
	o, err := r.s.officerRT.reg.Get(costOff)
	if err != nil {
		t.Fatal(err)
	}
	// the wake loop's read: HQ shows no routine instance holding anything
	llmSync(r.s.officerRT.llm, r.s.officerRT.reg, r.s.llmHoldsEnded)(o, nil, time.Now())
	r.s.flushHoldCosts()
	if got := holdCostsOf(r.hq, lapsed); len(got) != 2 || got[0] != (hqtest.HoldCostCall{Item: lapsed, Attempt: 1, Instance: r1.Name, USD: 0.0311, From: officers.CostFromLLMProxy}) {
		t.Errorf("lapsed, after the wake loop's read: HQ recorded holds %+v, want r1's 0.0311 as attempt 1", got)
	}
	wantCost(t, r.hq, lapsed, 0.0438)
}

// TestAHoldWhoseCostIsUnknownReportsNothing: a judgement instance with no
// cost source (its Claude Code session), and a routine hold that booked no
// request, end without a report — one omitted line each, naming why — so
// HQ never has every hold of the item and shows no cost for it.
func TestAHoldWhoseCostIsUnknownReportsNothing(t *testing.T) {
	r := newItemCostRig(t) // judgement is NOT on the proxy here
	r1, r2, j1 := r.agent(t, costRoutine+"1"), r.agent(t, costRoutine+"2"), r.agent(t, costJudge+"1")

	handed := r.hq.AddTieredItem(costOff, "web.check", "node-judged", officers.TierRoutine)
	r.claim(t, r1, handed)
	r.llm(t, r1, 0.0457)
	r.complete(t, r1, handed, officers.OutcomeNeedsJudgement)
	r.claim(t, j1, handed)
	r.complete(t, j1, handed, "done")
	if got := holdCostsOf(r.hq, handed); len(got) != 1 || got[0].Attempt != 1 {
		t.Errorf("HQ recorded holds %+v, want only r1's: the judgement session has no cost source", got)
	}
	if it := r.hq.Item(handed); it.CostUSD != nil {
		t.Errorf("an item whose judgement hold has no cost: HQ shows %v, want none", *it.CostUSD)
	}

	none := r.hq.AddTieredItem(costOff, "web.check", "node-none", officers.TierRoutine)
	r.claim(t, r2, none)
	r.complete(t, r2, none, "done")
	if got := holdCostsOf(r.hq, none); len(got) != 0 {
		t.Errorf("a hold with no request: HQ recorded %+v, want nothing (never a 0)", got)
	}
	want := "officers: hold cost omitted officer=" + costOff + " instance=" + r2.Name + " item=" + none + " attempt=1 reason=no_requests\n"
	if n := strings.Count(r.logs.String(), want); n != 1 {
		t.Errorf("the omitted line %q appears %d times, want once; logs:\n%s", want, n, r.logs)
	}
}

// TestAHoldCostAgainstAnHQWithoutTheRoute: an HQ from before per-hold cost
// answers the report 404; the complete it follows has succeeded (the item is
// done, its first-claim cost_usd stored as ever) and the log says the hold's
// cost was not recorded. The report never fails the tool call.
func TestAHoldCostAgainstAnHQWithoutTheRoute(t *testing.T) {
	r := newItemCostRig(t)
	r.hq.HoldCostAbsent = true
	r1 := r.agent(t, costRoutine+"1")
	item := r.hq.AddTieredItem(costOff, "web.check", "node-old-hq", officers.TierRoutine)
	r.claim(t, r1, item)
	r.llm(t, r1, 0.0311)
	if got, want := r.complete(t, r1, item, "done"), bodyWith(`"cost_from":"muster:llm-proxy","cost_usd":0.0311,`, r1.Name, "done"); len(got) != 1 || got[0] != want {
		t.Errorf("complete bodies\n got %q\nwant [%s]", got, want)
	}
	if st := r.hq.Item(item).Status; st != "done" {
		t.Errorf("the item is %s, want done", st)
	}
	wantCost(t, r.hq, item, 0.0311)
	if _, calls := r.hq.HoldCosts(); calls != 0 {
		t.Errorf("the absent route counted %d requests, want 0 (it answered before the route)", calls)
	}
	if want := "officers: hold cost not recorded officer=" + costOff + " instance=" + r1.Name + " item=" + item + " attempt=1 cost_usd=0.031100: "; !strings.Contains(r.logs.String(), want) {
		t.Errorf("the log does not say the hold's cost was not recorded (%q):\n%s", want, r.logs)
	}
}

// TestAHoldCostReportNeverBlocksTheWakeLoopsRead: the wake loop's read
// (llmSync, under officers.Config.Observed, which must not block) that ends
// a lapsed hold hands its report to the background: with HQ's route stalled,
// the read still returns at once, and the report lands once HQ answers.
// Red when the report is sent on the read's own goroutine: the read waits
// for HQ (up to the 10 s report timeout).
func TestAHoldCostReportNeverBlocksTheWakeLoopsRead(t *testing.T) {
	r := newItemCostRig(t)
	stall := make(chan struct{})
	var once sync.Once
	unstall := func() { once.Do(func() { close(stall) }) }
	t.Cleanup(unstall)
	r.hq.Before = func(_, path string) {
		if strings.HasSuffix(path, "/hold-cost") {
			<-stall
		}
	}
	r1 := r.agent(t, costRoutine+"1")
	item := r.hq.AddTieredItem(costOff, "web.check", "node-stalled", officers.TierRoutine)
	r.claim(t, r1, item)
	r.llm(t, r1, 0.0293)
	r.hq.SetClaim(item, r1.Name, time.Now().Add(-time.Minute)) // r1's lease lapses
	o, err := r.s.officerRT.reg.Get(costOff)
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	go func() {
		defer close(read)
		llmSync(r.s.officerRT.llm, r.s.officerRT.reg, r.s.llmHoldsEnded)(o, nil, time.Now())
	}()
	select {
	case <-read:
	case <-time.After(3 * time.Second):
		unstall()
		<-read
		t.Fatal("the wake loop's read waited on HQ's hold-cost route: a report must be sent in the background")
	}
	if got := holdCostsOf(r.hq, item); len(got) != 0 {
		t.Fatalf("with HQ stalled, HQ recorded %+v already, want nothing yet", got)
	}
	unstall()
	r.s.flushHoldCosts()
	if got := holdCostsOf(r.hq, item); len(got) != 1 ||
		got[0] != (hqtest.HoldCostCall{Item: item, Attempt: 1, Instance: r1.Name, USD: 0.0293, From: officers.CostFromLLMProxy}) {
		t.Errorf("after HQ answered: HQ recorded %+v, want r1's 0.0293 as attempt 1", got)
	}
}
