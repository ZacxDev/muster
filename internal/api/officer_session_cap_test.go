package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE JUDGEMENT-TIER DAILY SESSION CAP (tiers.judgement.max_sessions_per_day,
// officers plan §3): a judgement session is one item a tiered officer's
// judgement pool claims; at most N distinct items per officer per UTC day.
// The N+1th claim is refused before HQ (the item stays queued in HQ), and the
// wake loop does not wake the judgement pool for new work once the day's cap
// is spent. The routine pool, and an untiered officer, are not capped.
// ---------------------------------------------------------------------------

// newCappedOfficerRig is the rig over tiered officer T (judgement opus ×2
// with a cap of 2 sessions a day, routine haiku ×1) and untiered officer A.
//
// The cap is persisted (officer_judgement_sessions): the rig's session store
// is the PG one over the package's test database, emptied first. A second
// rig over the same database is a restarted server or another replica.
func newCappedOfficerRig(t *testing.T) *officerRig {
	t.Helper()
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_judgement_sessions RESTART IDENTITY`)
	return cappedRigOn(t, dsn)
}

// cappedRigOn is the capped rig over the database at dsn, as it stands.
func cappedRigOn(t *testing.T, dsn string) *officerRig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sessions := officers.NewPGSessionStore(pool)
	return newOfficerRigCfg(t, map[string]string{offATok: offA, offTTok: offT}, func(hqURL string) []officers.Officer {
		raw := `[{"id":"` + offA + `","hq_url":"` + hqURL + `","max_instances":2},` +
			`{"id":"` + offT + `","hq_url":"` + hqURL + `","tiers":{"routine":{"model":"haiku","max_instances":1},` +
			`"judgement":{"model":"opus","max_instances":2,"max_sessions_per_day":2}}}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offA): offATok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offT): offTTok,
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the capped rig's officers: %v", err)
		}
		return list
	}, func(c *OfficersConfig) { c.Sessions = sessions })
}

// claimsOf counts the claim requests HQ saw for item.
func claimsOf(r *officerRig, item string) int {
	n := 0
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/items/"+item+"/claim") {
			n++
		}
	}
	return n
}

// TestTheJudgementSessionCapRefusesTheNPlusFirstSessionOfTheDay: with a cap
// of 2, the judgement pool claims two distinct items; the third is refused
// locally, naming the cap — no claim reaches HQ and the item stays open. A
// claim HQ refuses consumes nothing; re-claiming an item already counted today
// is allowed at the cap; the routine pool and an untiered officer are not
// capped; and the next UTC day the third item is claimable.
func TestTheJudgementSessionCapRefusesTheNPlusFirstSessionOfTheDay(t *testing.T) {
	r := newCappedOfficerRig(t)
	ctx := context.Background()
	j1 := agents.Agent{Name: offTJudge + "1"}
	j2 := agents.Agent{Name: offTJudge + "2"}
	routine := agents.Agent{Name: offTRoutine + "1"}
	flat := agents.Agent{Name: officers.InstanceName(offA, 1)}
	i1 := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	held := r.hq.AddTieredItem(offT, "gpu.alert", "node-h", officers.TierJudgement)
	i2 := r.hq.AddTieredItem(offT, "gpu.alert", "node-b", officers.TierJudgement)
	i3 := r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	rr := r.hq.AddTieredItem(offT, "gpu.check", "node-r", officers.TierRoutine)
	aItem := r.hq.AddItem(offA, "release.evaluate", "svc:v1")
	claim := func(a agents.Agent, item string) string {
		t.Helper()
		return r.s.dispatchAgentTool(ctx, a, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	}

	if out := claim(j1, i1); isToolErr(out) {
		t.Fatalf("session 1: %s", out)
	}
	// HQ refuses this one (another instance holds it): it is no session.
	r.hq.SetClaim(held, "someone-else", r.now().Add(time.Hour))
	if out := claim(j2, held); !isToolErr(out) || !strings.Contains(out, "409") {
		t.Fatalf("claim of a held item: %s, want HQ's 409", out)
	}
	if out := claim(j2, i2); isToolErr(out) {
		t.Fatalf("session 2 (after an HQ refusal, which must not count): %s", out)
	}

	r.hq.ResetRequests()
	out := claim(j1, i3)
	if !isToolErr(out) || !strings.Contains(out, "max_sessions_per_day") || !strings.Contains(out, "2") {
		t.Fatalf("session 3 of a cap of 2: %s, want a refusal naming the cap", out)
	}
	if n := claimsOf(r, i3); n != 0 {
		t.Fatalf("the refused session reached HQ (%d claims)", n)
	}
	if st := r.hq.Item(i3).Status; st != "open" {
		t.Fatalf("the refused item is %s, want it left open in HQ", st)
	}

	// Already counted today: j2 re-claims i2 (it holds it: HQ's 409 naming
	// it becomes a heartbeat) — allowed at the cap.
	if out := claim(j2, i2); isToolErr(out) || !strings.Contains(out, "already yours") {
		t.Fatalf("re-claim of a counted item at the cap: %s", out)
	}
	// Not capped: the routine pool, and an untiered officer.
	if out := claim(routine, rr); isToolErr(out) {
		t.Fatalf("routine claim at the judgement cap: %s", out)
	}
	if out := claim(flat, aItem); isToolErr(out) {
		t.Fatalf("an untiered officer's claim: %s", out)
	}

	// The next UTC day the cap is fresh.
	r.advance(24 * time.Hour)
	if out := claim(j1, i3); isToolErr(out) {
		t.Fatalf("session 1 of the next day: %s", out)
	}
}

// TestACappedJudgementPoolIsNotWokenForNewWork: once the day's cap is spent,
// a new judgement item does not wake the judgement pool (an instance woken for
// it could only be refused, a Claude session spent on nothing). An item already
// counted today and back in the queue (released) is still work. Control: the
// next UTC day the new item wakes it too.
func TestACappedJudgementPoolIsNotWokenForNewWork(t *testing.T) {
	r := newCappedOfficerRig(t)
	ctx := context.Background()
	j1 := agents.Agent{Name: offTJudge + "1"}
	i1 := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	i2 := r.hq.AddTieredItem(offT, "gpu.alert", "node-b", officers.TierJudgement)
	// Two sessions: i1 finished, i2 given back (open again, counted today).
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+i1+`"}`); isToolErr(out) {
		t.Fatal(out)
	}
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolComplete, `{"item_id":"`+i1+`","outcome":"done","summary":"ok"}`); isToolErr(out) {
		t.Fatal(out)
	}
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+i2+`"}`); isToolErr(out) {
		t.Fatal(out)
	}
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolRelease, `{"item_id":"`+i2+`"}`); isToolErr(out) {
		t.Fatal(out)
	}
	r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement) // new work, past the cap

	judges := func() int {
		n := 0
		for _, a := range r.store.created {
			if strings.HasPrefix(a.Name, offTJudge) && !strings.HasPrefix(a.Name, offTRoutine) {
				n++
			}
		}
		return n
	}
	if res := r.rec.Check(ctx, offT, true); res.Err != nil || len(res.ActErrs) != 0 {
		t.Fatalf("pass: %+v", res)
	}
	// Uncapped, two claimable items and two judgement slots would start two.
	if n := judges(); n != 1 {
		t.Fatalf("at the cap the pass started %d judgement instances, want 1 (for the counted item only)", n)
	}
	r.advance(24 * time.Hour)
	if res := r.rec.Check(ctx, offT, true); res.Err != nil || len(res.ActErrs) != 0 {
		t.Fatalf("pass: %+v", res)
	}
	if n := judges(); n != 2 {
		t.Fatalf("control: the next day the pass has started %d judgement instances in all, want 2", n)
	}
}

// TestClaimNextCountsAgainstTheJudgementSessionCap: officer_claim with no
// item_id (HQ's claim-next picks the item) is a judgement session too. With a
// cap of 2: an empty queue's "nothing to claim" consumes nothing; two
// claim-nexts take two items; the third is refused before HQ — no claim-next
// request leaves muster and the item stays open. The next UTC day it is
// claimable.
func TestClaimNextCountsAgainstTheJudgementSessionCap(t *testing.T) {
	r := newCappedOfficerRig(t)
	r.hq.ClaimNext = true
	ctx := context.Background()
	j1 := agents.Agent{Name: offTJudge + "1"}
	next := func() string {
		t.Helper()
		return r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{}`)
	}
	if out := next(); isToolErr(out) || !strings.Contains(out, `"claimed":false`) {
		t.Fatalf("claim-next on an empty queue: %s, want nothing to claim", out)
	}
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	b := r.hq.AddTieredItem(offT, "gpu.alert", "node-b", officers.TierJudgement)
	c := r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	for _, want := range []string{a, b} {
		if out := next(); isToolErr(out) || !strings.Contains(out, `"id":"`+want+`"`) {
			t.Fatalf("claim-next (session for %s): %s", want, out)
		}
	}
	r.hq.ResetRequests()
	out := next()
	if !isToolErr(out) || !strings.Contains(out, "max_sessions_per_day") {
		t.Fatalf("claim-next past the cap: %s, want a refusal naming the cap", out)
	}
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/claim-next") {
			t.Fatalf("the refused claim-next reached HQ: %s %s", q.Method, q.Path)
		}
	}
	if st := r.hq.Item(c).Status; st != "open" {
		t.Fatalf("item %s is %s, want it left open in HQ", c, st)
	}
	for _, done := range []string{a, b} {
		if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolComplete, `{"item_id":"`+done+`","outcome":"done","summary":"ok"}`); isToolErr(out) {
			t.Fatal(out)
		}
	}
	r.advance(24 * time.Hour)
	if out := next(); isToolErr(out) || !strings.Contains(out, `"id":"`+c+`"`) {
		t.Fatalf("claim-next the next day: %s, want item %s", out, c)
	}
}

// TestTheJudgementSessionCapSurvivesARestart: two sessions spent through one
// server are still spent for a NEW server on the same database (a restart, or
// the other replica in a rollout): its third claim of the day is refused
// before HQ, and its wake loop does not wake the judgement pool for the new
// item. A refused HQ claim gives its slot back across servers too.
func TestTheJudgementSessionCapSurvivesARestart(t *testing.T) {
	first := newCappedOfficerRig(t)
	ctx := context.Background()
	j1 := agents.Agent{Name: offTJudge + "1"}
	for _, subj := range []string{"node-a", "node-b"} {
		it := first.hq.AddTieredItem(offT, "gpu.alert", subj, officers.TierJudgement)
		if out := first.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+it+`"}`); isToolErr(out) {
			t.Fatalf("first server, claim %s: %s", subj, out)
		}
		if out := first.s.dispatchAgentTool(ctx, j1, officers.ToolComplete, `{"item_id":"`+it+`","outcome":"done","summary":"ok"}`); isToolErr(out) {
			t.Fatal(out)
		}
	}
	second := cappedRigOn(t, dbtest.DSN(t))
	second.clock = first.now()
	// The second server's fake HQ numbers its items from 1 again: spend ids
	// 1 and 2 on finished items, so the new item is not one counted above.
	for _, subj := range []string{"done-1", "done-2"} {
		second.hq.Complete(second.hq.AddTieredItem(offT, "gpu.alert", subj, officers.TierJudgement))
	}
	i3 := second.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	if out := second.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+i3+`"}`); !isToolErr(out) || !strings.Contains(out, "max_sessions_per_day") {
		t.Fatalf("a restarted server's third session of a cap of 2: %s, want the cap's refusal", out)
	}
	if n := claimsOf(second, i3); n != 0 {
		t.Fatalf("the refused session reached HQ (%d claims)", n)
	}
	if res := second.rec.Check(ctx, offT, true); res.Err != nil || len(res.ActErrs) != 0 {
		t.Fatalf("pass: %+v", res)
	}
	if len(second.store.created) != 0 {
		t.Fatalf("a restarted server woke the capped judgement pool: created %v", second.store.created)
	}
}
