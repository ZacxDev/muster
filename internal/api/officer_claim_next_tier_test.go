//go:build muster419

// Guarded by the build tag muster419 until #419 (officer tiers: Officer.Tier,
// newTieredOfficerRig, hqtest AddTieredItem) is on main: it does not compile
// before. When #419 merges and this branch is updated onto main, DELETE the
// build line above and pass o.Tier to ClaimNext in the officer_claim{}
// branch; this test then goes red until that is done.

package api

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestClaimNextSendsTheSameTierAsClaimByID: for a routine instance, a
// judgement instance and an untiered officer's instance, officer_claim{}
// (claim-next) sends exactly the `tier` that officer_claim{item_id} sends for
// the same instance — the same value, or no tier key for both.
func TestClaimNextSendsTheSameTierAsClaimByID(t *testing.T) {
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
		reqs := r.hq.Requests()
		if len(reqs) == 0 {
			t.Fatalf("%s %s reached HQ with nothing", a.Name, args)
		}
		v, ok := reqs[0].Body["tier"]
		return v, ok
	}
	for _, c := range []struct {
		a    agents.Agent
		item string
	}{{routine, rItem}, {judge, jItem}, {flat, aItem}} {
		byID, byIDNamed := sent(c.a, `{"item_id":"`+c.item+`"}`)
		next, nextNamed := sent(c.a, `{}`)
		if byID != next || byIDNamed != nextNamed {
			t.Errorf("%s: claim-by-id sends tier %v (named %v), claim-next tier %v (named %v): want the same", c.a.Name, byID, byIDNamed, next, nextNamed)
		}
	}
}
