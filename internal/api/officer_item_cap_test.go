package api

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// ONE LIVE CLAIM PER CAPPED JUDGEMENT INSTANCE. tiers.judgement.
// max_item_minutes caps one item, but the wake loop enforces it by stopping
// the holder's WHOLE instance; any second item the instance held would lose
// its work with it. So officer_claim refuses a claim (by id or claim-next)
// from an instance of a capped judgement pool that already holds a live claim
// on another item, before HQ: the item stays queued there.
// ---------------------------------------------------------------------------

// newItemCapOfficerRig is the rig over tiered officer T (judgement opus ×2
// with max_item_minutes 30, routine haiku ×2) and untiered officer A.
func newItemCapOfficerRig(t *testing.T) *officerRig {
	t.Helper()
	return newOfficerRigWith(t, map[string]string{offATok: offA, offTTok: offT}, func(hqURL string) []officers.Officer {
		raw := `[{"id":"` + offA + `","hq_url":"` + hqURL + `","max_instances":2},` +
			`{"id":"` + offT + `","hq_url":"` + hqURL + `","tiers":{"routine":{"model":"haiku","max_instances":2},` +
			`"judgement":{"model":"opus","max_instances":2,"max_item_minutes":30}}}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offA): offATok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offT): offTTok,
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the item-cap rig's officers: %v", err)
		}
		return list
	})
}

// claimRequests counts the claim requests (by id and claim-next) HQ saw.
func claimRequests(r *officerRig) int {
	n := 0
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/claim") || strings.HasSuffix(q.Path, "/claim-next") {
			n++
		}
	}
	return n
}

// TestACappedJudgementInstanceHoldsOneLiveClaimAtATime: judgement instance 1
// holds item A; its claim of item C, and its claim-next, are refused naming
// A, never reach HQ, and leave C open. Its re-claim of A (already yours) is
// allowed. Once A is completed, it claims C. Another judgement instance, the
// routine pool (no cap) and an untiered officer are not limited.
func TestACappedJudgementInstanceHoldsOneLiveClaimAtATime(t *testing.T) {
	r := newItemCapOfficerRig(t)
	r.hq.ClaimNext = true
	ctx := context.Background()
	j1 := r.addNamed(offTJudge+"1", 961)
	j2 := r.addNamed(offTJudge+"2", 962)
	routine := r.addNamed(offTRoutine+"1", 963)
	flat := r.addNamed(officers.InstanceName(offA, 1), 964)
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	c := r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	d := r.hq.AddTieredItem(offT, "gpu.alert", "node-d", officers.TierJudgement)
	r1 := r.hq.AddTieredItem(offT, "gpu.check", "node-r1", officers.TierRoutine)
	r2 := r.hq.AddTieredItem(offT, "gpu.check", "node-r2", officers.TierRoutine)
	f1 := r.hq.AddItem(offA, "release.evaluate", "svc:v1")
	f2 := r.hq.AddItem(offA, "release.evaluate", "svc:v2")
	call := func(ag agents.Agent, tool, args string) string {
		t.Helper()
		return r.s.dispatchAgentTool(ctx, ag, tool, args)
	}
	claim := func(ag agents.Agent, item string) string {
		t.Helper()
		return call(ag, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	}

	if out := claim(j1, a); isToolErr(out) {
		t.Fatalf("j1's first claim: %s", out)
	}
	r.hq.ResetRequests()
	if out := claim(j1, c); !isToolErr(out) || !strings.Contains(out, a) {
		t.Fatalf("j1 holding %s claims %s: %s, want a refusal naming %s", a, c, out, a)
	}
	if out := call(j1, officers.ToolClaim, `{}`); !isToolErr(out) || !strings.Contains(out, a) {
		t.Fatalf("j1 holding %s claims next: %s, want a refusal naming %s", a, out, a)
	}
	if n := claimRequests(r); n != 0 {
		t.Fatalf("the refused claims reached HQ (%d claim requests)", n)
	}
	if st := r.hq.Item(c).Status; st != "open" {
		t.Fatalf("the refused item is %s, want it left open in HQ", st)
	}
	if out := claim(j1, a); isToolErr(out) || !strings.Contains(out, "already yours") {
		t.Fatalf("j1 re-claiming its own %s: %s, want already yours", a, out)
	}
	// Another instance of the same pool holds its own one claim.
	if out := claim(j2, d); isToolErr(out) {
		t.Fatalf("j2's claim while j1 holds another item: %s", out)
	}
	// Not limited: the routine pool (no cap) and an untiered officer.
	for _, tc := range []struct {
		who   agents.Agent
		items []string
	}{{routine, []string{r1, r2}}, {flat, []string{f1, f2}}} {
		for _, it := range tc.items {
			if out := claim(tc.who, it); isToolErr(out) {
				t.Fatalf("%s's claim of %s (no cap on its pool): %s", tc.who.Name, it, out)
			}
		}
	}
	if out := call(j1, officers.ToolComplete, `{"item_id":"`+a+`","outcome":"done","summary":"checked"}`); isToolErr(out) {
		t.Fatalf("j1 completes %s: %s", a, out)
	}
	if out := claim(j1, c); isToolErr(out) {
		t.Fatalf("j1, holding nothing now, claims %s: %s", c, out)
	}
}
