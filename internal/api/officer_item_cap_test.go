package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestTwoParallelClaimsFromOneCappedInstanceLandOneClaim: one judgement
// instance makes two officer_claim calls at once (parallel tool calls), for
// two different items. Exactly one claim reaches HQ; the other is refused,
// naming the item the first one took. officer_claim's per-instance lock is
// what makes this true: without it, both read HQ's claimed list while it is
// still empty and both claim.
//
// HQ's Before hook holds each claimed-list read until a second read arrives
// (3 s at most), so without the lock the two reads always overlap; with it,
// the second read cannot start until the first claim is done, and the first
// read gives up waiting after 3 s.
func TestTwoParallelClaimsFromOneCappedInstanceLandOneClaim(t *testing.T) {
	r := newItemCapOfficerRig(t)
	ctx := context.Background()
	j1 := r.addNamed(offTJudge+"1", 971)
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	c := r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	var mu sync.Mutex
	reads := 0
	both := make(chan struct{})
	r.hq.Before = func(method, p string) {
		if method != http.MethodGet || !strings.HasSuffix(p, "/items") {
			return
		}
		mu.Lock()
		reads++
		if reads == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(3 * time.Second):
		}
	}
	r.hq.ResetRequests()
	outs := make([]string, 2)
	var wg sync.WaitGroup
	for i, it := range []string{a, c} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i] = r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+it+`"}`)
		}()
	}
	wg.Wait()
	if n := claimRequests(r); n != 1 {
		t.Fatalf("two parallel claims from one capped instance: %d claim requests reached HQ, want exactly 1 (results: %q)", n, outs)
	}
	var won, refused []int
	for i, out := range outs {
		if isToolErr(out) {
			refused = append(refused, i)
		} else {
			won = append(won, i)
		}
	}
	if len(won) != 1 || len(refused) != 1 {
		t.Fatalf("two parallel claims: results %q, want one claim and one refusal", outs)
	}
	winner := []string{a, c}[won[0]]
	if !strings.Contains(outs[refused[0]], winner) {
		t.Errorf("the refused parallel claim says %q, want it to name %s (the item the other call took)", outs[refused[0]], winner)
	}
	for _, it := range []string{a, c} {
		want := "open"
		if it == winner {
			want = "claimed"
		}
		if st := r.hq.Item(it).Status; st != want {
			t.Errorf("item %s is %s in HQ, want %s", it, st, want)
		}
	}
}

// TestACappedClaimIsRefusedWhenHQsClaimedListCannotBeRead: officer_claim from
// a capped judgement instance reads HQ's claimed list first. When that read
// fails (here a 503 on the items list only; HQ's claim routes still answer),
// the claim is refused, by id and claim-next alike, and no claim reaches HQ:
// fail closed. Positive control: once the list reads again, the same claim
// lands.
func TestACappedClaimIsRefusedWhenHQsClaimedListCannotBeRead(t *testing.T) {
	r := newItemCapOfficerRig(t)
	r.hq.ClaimNext = true
	ctx := context.Background()
	j1 := r.addNamed(offTJudge+"1", 981)
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	var listDown atomic.Bool
	listDown.Store(true)
	r.hq.Before = func(method, p string) {
		if listDown.Load() && method == http.MethodGet && strings.HasSuffix(p, "/items") {
			r.hq.SetStatus(http.StatusServiceUnavailable)
			return
		}
		r.hq.SetStatus(0)
	}
	r.hq.ResetRequests()
	const failClosed = "could not check which item you already hold"
	for _, args := range []string{`{"item_id":"` + a + `"}`, `{}`} {
		out := r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, args)
		if !isToolErr(out) || !strings.Contains(out, failClosed) {
			t.Fatalf("officer_claim %s with HQ's claimed list unreadable: %s, want the fail-closed refusal (%q)", args, out, failClosed)
		}
	}
	if n := claimRequests(r); n != 0 {
		t.Fatalf("with HQ's claimed list unreadable, %d claim request(s) reached HQ, want 0 (fail closed)", n)
	}
	if st := r.hq.Item(a).Status; st != "open" {
		t.Fatalf("item %s is %s after the refused claims, want open", a, st)
	}
	listDown.Store(false)
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+a+`"}`); isToolErr(out) {
		t.Fatalf("positive control: with the claimed list readable again the claim is refused: %s", out)
	}
	if st := r.hq.Item(a).Status; st != "claimed" {
		t.Fatalf("positive control: item %s is %s, want claimed", a, st)
	}
}
