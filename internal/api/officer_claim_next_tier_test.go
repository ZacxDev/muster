package api

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// claimBody is the body of the one claim-shaped POST (…/claim or
// …/claim-next) HQ saw since the last ResetRequests; the test fails when
// there is not exactly one.
func (r *officerRig) claimBody(t *testing.T, what string) map[string]any {
	t.Helper()
	var bodies []map[string]any
	for _, q := range r.hq.Requests() {
		if q.Method == "POST" && (strings.HasSuffix(q.Path, "/claim") || strings.HasSuffix(q.Path, "/claim-next")) {
			bodies = append(bodies, q.Body)
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("%s: %d claim POSTs reached HQ (%+v), want exactly one", what, len(bodies), r.hq.Requests())
	}
	return bodies[0]
}

// TestClaimNextSendsTheInstancesTier: officer_claim{} (claim-next) sends the
// calling instance's pool tier as the body's `tier` — "routine" from a
// routine instance, "judgement" from a judgement instance, and no tier key at
// all from an untiered officer's instance — literally, and the same value
// officer_claim{item_id} sends for that instance.
func TestClaimNextSendsTheInstancesTier(t *testing.T) {
	r := newTieredOfficerRig(t)
	r.hq.ClaimNext = true
	ctx := context.Background()
	routine := r.addNamed(offTRoutine+"1", 951)
	judge := r.addNamed(offTJudge+"1", 952)
	flat := r.addNamed(officers.InstanceName(offA, 1), 953)
	rItem := r.hq.AddTieredItem(offT, "gpu.check", "node-a", officers.TierRoutine)
	jItem := r.hq.AddTieredItem(offT, "gpu.alert", "node-b", officers.TierJudgement)
	aItem := r.hq.AddItem(offA, "release.evaluate", "svc:v1")
	sent := func(a agents.Agent, args string) (any, bool) {
		t.Helper()
		r.hq.ResetRequests()
		r.s.dispatchAgentTool(ctx, a, officers.ToolClaim, args)
		v, ok := r.claimBody(t, a.Name+" "+args)["tier"]
		return v, ok
	}
	for _, c := range []struct {
		a     agents.Agent
		item  string
		tier  string
		named bool
	}{
		{routine, rItem, "routine", true},
		{judge, jItem, "judgement", true},
		{flat, aItem, "", false},
	} {
		next, nextNamed := sent(c.a, `{}`)
		if nextNamed != c.named || (c.named && next != c.tier) {
			t.Errorf("%s: claim-next sent tier %v (key present %v), want %q (key present %v)", c.a.Name, next, nextNamed, c.tier, c.named)
		}
		// claim-by-id on an item the claim-next above may have taken: only the
		// body matters here, not HQ's answer.
		byID, byIDNamed := sent(c.a, `{"item_id":"`+c.item+`"}`)
		if byID != next || byIDNamed != nextNamed {
			t.Errorf("%s: claim-by-id sends tier %v (named %v), claim-next tier %v (named %v): want the same", c.a.Name, byID, byIDNamed, next, nextNamed)
		}
	}
}

// TestClaimNextTakesOnlyTheInstancesTier: with one officer's queue holding an
// URGENT judgement item and a low routine item, a routine instance's
// officer_claim{} takes the routine item (not the urgent judgement one that
// HQ would order first across tiers), and a judgement instance's takes the
// judgement item; each item ends claimed by the instance of its own tier.
func TestClaimNextTakesOnlyTheInstancesTier(t *testing.T) {
	r := newTieredOfficerRig(t)
	r.hq.ClaimNext = true
	ctx := context.Background()
	routine := r.addNamed(offTRoutine+"1", 961)
	judge := r.addNamed(offTJudge+"1", 962)
	jItem := r.hq.AddTieredItem(offT, "gpu.alert", "node-b", officers.TierJudgement)
	r.hq.SetPriority(jItem, "urgent")
	rItem := r.hq.AddTieredItem(offT, "gpu.check", "node-a", officers.TierRoutine)
	r.hq.SetPriority(rItem, "low")

	res := decodeClaim(t, r.s.dispatchAgentTool(ctx, routine, officers.ToolClaim, `{}`))
	if res.Item == nil || res.Item.ID != officers.ItemID(rItem) || res.Item.ClaimedBy != routine.Name {
		t.Fatalf("routine instance officer_claim{}: %+v, want routine item %s claimed by %s", res.Item, rItem, routine.Name)
	}
	if it := r.hq.Item(jItem); it.Status != "open" {
		t.Fatalf("the judgement item changed under a routine claim-next: %+v", it)
	}
	res = decodeClaim(t, r.s.dispatchAgentTool(ctx, judge, officers.ToolClaim, `{}`))
	if res.Item == nil || res.Item.ID != officers.ItemID(jItem) || res.Item.ClaimedBy != judge.Name {
		t.Fatalf("judgement instance officer_claim{}: %+v, want judgement item %s claimed by %s", res.Item, jItem, judge.Name)
	}
}
