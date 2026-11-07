package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// tiers.<pool>.fresh_session_per_item: ONE ITEM PER CONVERSATION. An instance
// of such a pool works each item in a NEW Claude Code conversation:
//
//   - every turn muster gives it — the wake loop's prompt and the kickoff
//     deliverer's first turn — carries agents.HeaderCCNewConversation, on
//     which ccd starts a new conversation (the old transcript kept) before it
//     pastes the turn; and the turn's text is the kickoff, never "as before";
//   - officer_claim holds it to one item per conversation: a second claim
//     while it holds one, and any claim once its hold has ended (complete,
//     release, ask), is refused before HQ until muster has started it a new
//     conversation.
// ---------------------------------------------------------------------------

// newFreshOfficerRig is the rig over tiered officer T — judgement opus ×2
// with fresh_session_per_item and NO max_item_minutes (so the one-claim
// refusal below cannot come from the wall-time cap's rule), routine haiku ×2
// without it — and untiered officer A.
func newFreshOfficerRig(t *testing.T) *officerRig {
	t.Helper()
	return newOfficerRigWith(t, map[string]string{offATok: offA, offTTok: offT}, func(hqURL string) []officers.Officer {
		raw := `[{"id":"` + offA + `","hq_url":"` + hqURL + `","max_instances":2},` +
			`{"id":"` + offT + `","hq_url":"` + hqURL + `","tiers":{"routine":{"model":"haiku","max_instances":2},` +
			`"judgement":{"model":"opus","max_instances":2,"fresh_session_per_item":true}}}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offA): offATok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offT): offTTok,
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the fresh rig's officers: %v", err)
		}
		return list
	})
}

// sentTo is the last re-prompt text sent to agent name, and its
// new-conversation header ("" when none); ok false when none was sent.
func (g *repromptChat) sentTo(name string) (text, newConv string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.sent) - 1; i >= 0; i-- {
		if rest, found := strings.CutPrefix(g.sent[i], name+": "); found {
			nc, _ := strings.CutPrefix(g.newConv[i], name+": ")
			return rest, nc, true
		}
	}
	return "", "", false
}

// TestAFreshInstanceWorksOneItemPerConversation: judgement instance 1 of a
// fresh pool claims A; its claim of C while it holds A is refused naming A.
// It completes A; its claim of C, and its claim-next, are then refused (this
// conversation has had its item), neither reaches HQ, and C stays open. The
// wake loop's next pass prompts it with the KICKOFF, on a turn carrying the
// new-conversation header "1"; after that, it claims C.
//
// ⚠ The routine pool (no key) is the INVARIANT half: it claims, completes
// and claims again in one conversation, and its re-prompt is "as before"
// with no header — green before and after this change.
func TestAFreshInstanceWorksOneItemPerConversation(t *testing.T) {
	r := newFreshOfficerRig(t)
	r.hq.ClaimNext = true
	ctx := context.Background()
	j1 := r.addNamed(offTJudge+"1", 981)
	ro := r.addNamed(offTRoutine+"1", 982)
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	c := r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	r1 := r.hq.AddTieredItem(offT, "gpu.check", "node-r1", officers.TierRoutine)
	r2 := r.hq.AddTieredItem(offT, "gpu.check", "node-r2", officers.TierRoutine)
	r.hq.AddTieredItem(offT, "gpu.check", "node-r3", officers.TierRoutine)
	call := func(ag agents.Agent, tool, args string) string {
		t.Helper()
		return r.s.dispatchAgentTool(ctx, ag, tool, args)
	}
	claim := func(ag agents.Agent, item string) string {
		t.Helper()
		return call(ag, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	}
	complete := func(ag agents.Agent, item string) {
		t.Helper()
		if out := call(ag, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"done","summary":"checked"}`); isToolErr(out) {
			t.Fatalf("%s completes %s: %s", ag.Name, item, out)
		}
	}

	if out := claim(j1, a); isToolErr(out) {
		t.Fatalf("j1's first claim: %s", out)
	}
	r.hq.ResetRequests()
	if out := claim(j1, c); !isToolErr(out) || !strings.Contains(out, a) {
		t.Fatalf("j1 holding %s claims %s: %s, want a refusal naming %s", a, c, out, a)
	}
	complete(j1, a)
	r.hq.ResetRequests()
	for _, args := range []string{`{"item_id":"` + c + `"}`, `{}`} {
		out := call(j1, officers.ToolClaim, args)
		if !isToolErr(out) || !strings.Contains(out, "fresh_session_per_item") || !strings.Contains(out, a) {
			t.Fatalf("j1, its item %s done in this conversation, claims %s: %s, want a refusal naming fresh_session_per_item and %s", a, args, out, a)
		}
	}
	if n := claimRequests(r); n != 0 {
		t.Fatalf("the refused claims reached HQ (%d claim requests)", n)
	}
	if st := r.hq.Item(c).Status; st != "open" {
		t.Fatalf("the refused item is %s, want it left open in HQ", st)
	}

	// INVARIANT: the routine pool keeps working the queue in one conversation.
	for _, it := range []string{r1, r2} {
		if out := claim(ro, it); isToolErr(out) {
			t.Fatalf("routine claims %s: %s", it, out)
		}
		complete(ro, it)
	}

	res := r.rec.Check(ctx, offT, true)
	r.s.WaitForOfficerReprompts()
	if len(res.ActErrs) != 0 || len(res.Decision.Reprompt) != 1 || res.Decision.Reprompt[0] != 1 {
		t.Fatalf("the pass after j1's item: %+v, want judgement instance 1 prompted", res)
	}
	// The pool as the runtime resolves it from the agent's name (Parse sorts
	// officers by id, so neither r.a nor r.b is T by position).
	judge, _, ok := r.s.officerRT.reg.ForAgentName(j1.Name)
	if !ok || judge.ID != offT || judge.Tier != officers.TierJudgement {
		t.Fatalf("instrument check: %s resolves to %+v (ok %v), want %s's judgement pool", j1.Name, judge, ok, offT)
	}
	text, nc, sent := r.chat.sentTo(j1.Name)
	if !sent || text != officers.KickoffText(judge, 1) || nc != "1" {
		t.Fatalf("j1's turn: sent=%v header=%q text:\n%s\n--- want the kickoff on a new-conversation turn (header \"1\")", sent, nc, text)
	}
	text, nc, sent = r.chat.sentTo(ro.Name)
	if !sent || !strings.HasPrefix(text, "New work for officer "+offT) || nc != "" {
		t.Fatalf("INVARIANT: the routine instance's turn: sent=%v header=%q text %q, want \"as before\" with no header", sent, nc, text)
	}

	if out := claim(j1, c); isToolErr(out) {
		t.Fatalf("j1, in a new conversation, claims %s: %s", c, out)
	}

	// 🔴 ITS NEXT ITEM DOES NOT WAIT OUT RepromptAfter. j1 finishes c a
	// minute after its prompt; a new item d is open. Its conversation has had
	// its item and it holds none, so the next pass prompts it at once — before,
	// the 5-minute rate limit held every fresh-pool item to ~12 an hour.
	complete(j1, c)
	d := r.hq.AddTieredItem(offT, "gpu.alert", "node-d", officers.TierJudgement)
	r.advance(time.Minute)
	if time.Minute >= officers.RepromptAfter {
		t.Fatal("instrument check: the advance must stay inside RepromptAfter")
	}
	res = r.rec.Check(ctx, offT, true)
	r.s.WaitForOfficerReprompts()
	if len(res.ActErrs) != 0 || len(res.Decision.Reprompt) != 1 || res.Decision.Reprompt[0] != 1 {
		t.Fatalf("the pass a minute after j1's prompt, its item done: %+v, want judgement instance 1 prompted at once", res)
	}
	if out := claim(j1, d); isToolErr(out) {
		t.Fatalf("j1, prompted into a new conversation, claims %s: %s", d, out)
	}
}

// 🔴 ONE ITEM PER CONVERSATION, HOWEVER THE HOLD ENDS. The conversation is
// marked when its claim SUCCEEDS, so a hold that ends without the instance's
// own complete/release/ask still leaves it spent:
//
//   - (i) HQ lets j1's lease on a lapse (heartbeats missed during a long
//     read): its claims of c, by id and claim-next, are refused naming a;
//   - (ii) j2's lease on b lapses and its complete then FAILS (lease_lost):
//     refused the same way, naming b.
//
// Neither refused claim reaches HQ, and c stays open.
func TestAFreshConversationNeverClaimsASecondItemHoweverItsHoldEnded(t *testing.T) {
	r := newFreshOfficerRig(t)
	r.hq.ClaimNext = true
	ctx := context.Background()
	j1 := r.addNamed(offTJudge+"1", 981)
	j2 := r.addNamed(offTJudge+"2", 982)
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	b := r.hq.AddTieredItem(offT, "gpu.alert", "node-b", officers.TierJudgement)
	c := r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	call := func(ag agents.Agent, tool, args string) string {
		t.Helper()
		return r.s.dispatchAgentTool(ctx, ag, tool, args)
	}
	refusedNaming := func(ag agents.Agent, held, why string) {
		t.Helper()
		r.hq.ResetRequests()
		for _, args := range []string{`{"item_id":"` + c + `"}`, `{}`} {
			out := call(ag, officers.ToolClaim, args)
			if !isToolErr(out) || !strings.Contains(out, "fresh_session_per_item") || !strings.Contains(out, held) {
				t.Fatalf("%s: %s, its conversation having claimed %s, claims %s: %s, want a refusal naming fresh_session_per_item and %s",
					why, ag.Name, held, args, out, held)
			}
		}
		if n := claimRequests(r); n != 0 {
			t.Fatalf("%s: the refused claims reached HQ (%d claim requests)", why, n)
		}
	}

	if out := call(j1, officers.ToolClaim, `{"item_id":"`+a+`"}`); isToolErr(out) {
		t.Fatalf("j1's first claim: %s", out)
	}
	r.hq.SetClaim(a, j1.Name, r.now().Add(-time.Minute))
	refusedNaming(j1, a, "(i) lease expired")

	if out := call(j2, officers.ToolClaim, `{"item_id":"`+b+`"}`); isToolErr(out) {
		t.Fatalf("j2's first claim: %s", out)
	}
	r.hq.SetClaim(b, j2.Name, r.now().Add(-time.Minute))
	if out := call(j2, officers.ToolComplete, `{"item_id":"`+b+`","outcome":"done","summary":"checked"}`); !isToolErr(out) || !strings.Contains(out, "lease_lost") {
		t.Fatalf("instrument check: j2's complete of %s on a lapsed lease: %s, want HQ's lease_lost", b, out)
	}
	refusedNaming(j2, b, "(ii) complete failed")

	if st := r.hq.Item(c).Status; st != "open" {
		t.Fatalf("the refused item is %s, want it left open in HQ", st)
	}
}

// 🔴 A CONVERSATION IS FREED ONLY WHEN ccd CONFIRMS THE NEW ONE TOOK THE TURN
// (the 103 naming its session, agents.TurnAcceptance.NewConversation):
//
//   - while the wake loop's send is in flight and ccd then refuses it (409
//     busy: the old turn is still running), the OLD conversation's claim is
//     refused — it was cleared before the send, and that window let it
//     through;
//   - after the refusal, still refused;
//   - a ccd that ACCEPTS the turn without naming a new conversation (an image
//     that predates the header: the turn ran in the old conversation) frees
//     nothing, is logged, and is sent no further turns.
func TestAFreshConversationIsFreedOnlyWhenCCDConfirmsANewOne(t *testing.T) {
	r := newFreshOfficerRig(t)
	ctx := context.Background()
	j1 := r.addNamed(offTJudge+"1", 981)
	a := r.hq.AddTieredItem(offT, "gpu.alert", "node-a", officers.TierJudgement)
	c := r.hq.AddTieredItem(offT, "gpu.alert", "node-c", officers.TierJudgement)
	claimC := func() string {
		return r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+c+`"}`)
	}
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolClaim, `{"item_id":"`+a+`"}`); isToolErr(out) {
		t.Fatalf("j1's first claim: %s", out)
	}
	if out := r.s.dispatchAgentTool(ctx, j1, officers.ToolComplete, `{"item_id":"`+a+`","outcome":"done","summary":"checked"}`); isToolErr(out) {
		t.Fatalf("j1 completes %s: %s", a, out)
	}

	var during string
	r.chat.setReply(func(context.Context, string) error {
		during = claimC()
		return &agents.RuntimeError{Status: http.StatusConflict, Type: "busy", Message: "the session is mid-turn"}
	})
	r.rec.Check(ctx, offT, true)
	r.s.WaitForOfficerReprompts()
	if r.chat.count() != 1 {
		t.Fatalf("instrument check: %d sends, want the one re-prompt", r.chat.count())
	}
	if !isToolErr(during) || !strings.Contains(during, a) {
		t.Fatalf("the old conversation's claim while the refused send was in flight: %s, want refused naming %s", during, a)
	}
	if out := claimC(); !isToolErr(out) || !strings.Contains(out, a) {
		t.Fatalf("after ccd refused the turn: %s, want refused naming %s", out, a)
	}

	r.chat.setReply(func(ctx context.Context, _ string) error {
		agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{}) // an old ccd: accepted, no new conversation named
		return nil
	})
	r.advance(officers.RepromptAfter + time.Second)
	r.rec.Check(ctx, offT, true)
	r.s.WaitForOfficerReprompts()
	if r.chat.count() != 2 {
		t.Fatalf("instrument check: %d sends, want the stale ccd's re-prompt as the second", r.chat.count())
	}
	if out := claimC(); !isToolErr(out) || !strings.Contains(out, a) {
		t.Fatalf("after a ccd accepted the turn without a new conversation: %s, want refused naming %s", out, a)
	}
	if !strings.Contains(r.logs.String(), "did not confirm a new conversation") {
		t.Fatalf("a stale ccd's turn was not logged; log:\n%s", r.logs.String())
	}
	r.advance(officers.RepromptAfter + time.Second)
	r.rec.Check(ctx, offT, true)
	r.s.WaitForOfficerReprompts()
	if n := r.chat.count(); n != 2 {
		t.Fatalf("a ccd known not to start new conversations was prompted again (%d sends)", n)
	}
}

// TestAFreshPoolsTurnContextAsksForANewConversation: OfficerTurnContext — the
// one rule the wake loop's prompt and the kickoff deliverer share — routes a
// turn of a fresh pool's instance with HeaderCCNewConversation "1", and leaves
// every other agent's turn without a route: the routine pool's instance, an
// untiered officer's, and an ordinary agent (⚠ that half is an INVARIANT
// guard).
func TestAFreshPoolsTurnContextAsksForANewConversation(t *testing.T) {
	r := newFreshOfficerRig(t)
	ctx := context.Background()
	j1 := r.addNamed(offTJudge+"1", 991)
	header := func(a agents.Agent) (string, bool) {
		rt, ok := agents.TurnRouteFrom(r.s.OfficerTurnContext(ctx, a))
		if !ok {
			return "", false
		}
		return rt.Header.Get(agents.HeaderCCNewConversation), true
	}
	if v, ok := header(j1); !ok || v != "1" {
		t.Fatalf("a fresh pool's instance: route %v header %q, want \"1\"", ok, v)
	}
	for _, a := range []agents.Agent{
		r.addNamed(offTRoutine+"1", 992),
		r.addNamed(officers.InstanceName(offA, 1), 993),
		{ID: officerOrdinaryID, Name: "brave-otter"},
	} {
		if v, ok := header(a); ok {
			t.Fatalf("INVARIANT: %s's turn got a route (header %q); want none", a.Name, v)
		}
	}
}
