package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RE-ASK (chief_nudge.go reaskNudge): an ask-stall whose chief answer gave
// no question is asked ONCE more, in a lane of its own, for the question alone;
// only when that second answer has none either is it the generic ask. And the
// chief's reason is on the row, with how the re-ask went.

const (
	nudgeLeaveReply = `{"decision":"leave","reason":"it is done"}`
	nudgeReaskReply = `{"decision":"ask","reason":"three things pending","question":"It is waiting on you to review the draft and post the thread: done, or later?","options":["Done","Later"]}`
	nudgeReaskQ     = "It is waiting on you to review the draft and post the thread: done, or later?"
)

// prompts is every stall check's prompt, in order.
func (g *nudgeGateway) prompts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.prompt...)
}

// answer scripts the next stall checks' answers.
func (g *nudgeGateway) answer(a ...nudgeAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answers = append(g.answers, a...)
}

// rowReason is decision id's reason read straight from chief_nudges.
func (f *nudgeFixture) rowReason(t *testing.T, id int64) string {
	t.Helper()
	var r string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT reason FROM chief_nudges WHERE id = $1`, id).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestChiefNudgeAnAskStallTheChiefWouldLeaveIsAskedOnceMore: the chief says
// leave on an ask-stall, giving no question; it is asked once more, in a lane
// of its own, and the ask raised (live) carries ITS question and options — not
// the generic ask. Exactly two chief calls.
func TestChiefNudgeAnAskStallTheChiefWouldLeaveIsAskedOnceMore(t *testing.T) {
	f := newNudgeFixture(t, true)
	j := withJev(t, f)
	j.set(200, 0.90, 0.05, 0.05)
	f.gw.answer(nudgeAnswer{reply: nudgeLeaveReply}, nudgeAnswer{reply: nudgeReaskReply})
	f.gw.set(nudgeLeaveReply) // anything past the two scripted answers
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeAsked || d.Question != nudgeReaskQ ||
		strings.Join(d.Options, "|") != "Done|Later" || chiefnudge.IsGenericAsk(d.Options) {
		t.Fatalf("the re-asked ask: %+v", d)
	}
	a, err := f.attn.Get(asUser(authz.OperatorUserID), d.AskID)
	if err != nil || !strings.Contains(a.Question, nudgeReaskQ) || strings.Contains(a.Question, "Its last turn looks like") ||
		strings.Join(a.Options, "|") != "Done|Later" {
		t.Fatalf("the attention ask: %+v %v", a, err)
	}
	const want = "classifier: needs_input, ask score 0.95 — the default is an ask (the chief said leave it; re-ask: gave a question); it is done"
	if got := f.rowReason(t, d.ID); got != want {
		t.Fatalf("reason on the row:\n got %q\nwant %q", got, want)
	}
	checks, _ := f.gw.seen()
	if len(checks) != 2 {
		t.Fatalf("chief calls: %d, want 2: %v", len(checks), checks)
	}
	if checks[0] == checks[1] {
		t.Fatalf("the re-ask reused the stall check's lane: %v", checks)
	}
	p := f.gw.prompts()
	if !strings.Contains(p[1], "stall check, second turn") || !strings.Contains(p[1], "CI is green on the PR. Shall I merge it?") ||
		!strings.Contains(p[1], "write ONE question") || strings.Contains(p[1], nudgeOwnerText) {
		t.Fatalf("the re-ask's prompt:\n%s", p[1])
	}
	if !strings.Contains(p[0], "write ONE question") {
		t.Fatalf("the stall check's prompt lacks the several-items guidance:\n%s", p[0])
	}
}

// TestChiefNudgeAReaskInShadowIsRecordedAsTheLiveAskWouldBe: shadow re-asks
// too, so its row shows the question live would raise.
func TestChiefNudgeAReaskInShadowIsRecordedAsTheLiveAskWouldBe(t *testing.T) {
	f := newNudgeFixture(t, false)
	j := withJev(t, f)
	j.set(200, 0.90, 0.05, 0.05)
	asks0 := f.askRows(t)
	f.gw.answer(nudgeAnswer{reply: nudgeLeaveReply}, nudgeAnswer{reply: nudgeReaskReply})
	f.pass(t)
	d := f.one(t)
	if d.Outcome != chiefnudge.OutcomeWouldAsk || d.Question != nudgeReaskQ || strings.Join(d.Options, "|") != "Done|Later" ||
		!strings.Contains(d.Reason, "; re-ask: gave a question)") {
		t.Fatalf("shadow re-ask: %+v", d)
	}
	if a := f.askRows(t); a != asks0 {
		t.Fatalf("shadow raised an ask: %d→%d", asks0, a)
	}
	if checks, _ := f.gw.seen(); len(checks) != 2 {
		t.Fatalf("chief calls: %d, want 2", len(checks))
	}
}

// TestChiefNudgeAReaskWithNoQuestionIsTheGenericAsk: every way the second
// answer can fail to give a question ends in today's generic ask, after
// exactly two calls, with the reason saying why.
func TestChiefNudgeAReaskWithNoQuestionIsTheGenericAsk(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    nudgeAnswer
		why  string
	}{
		{"error", nudgeAnswer{err: errors.New("nudgeGateway: the provider is down")}, "the chief's call failed (failed; see the agent's transcript)"},
		{"timeout", nudgeAnswer{err: context.DeadlineExceeded}, "the chief's call failed (the turn outlasted its deadline)"},
		{"leave again", nudgeAnswer{reply: nudgeLeaveReply}, "it said leave"},
		{"invalid", nudgeAnswer{reply: "I think it is fine."}, "its answer was not a decision"},
		{"empty", nudgeAnswer{reply: ""}, "its answer was not a decision"},
		{"blank question", nudgeAnswer{reply: `{"decision":"ask","reason":"x","question":"   ","options":["a","b"]}`}, "its ask had no question"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNudgeFixture(t, false)
			j := withJev(t, f)
			j.set(200, 0.90, 0.05, 0.05)
			f.gw.answer(nudgeAnswer{reply: nudgeLeaveReply}, tc.a)
			f.gw.set(nudgeReaskReply) // a THIRD call would get a question: it must not happen
			f.pass(t)
			d := f.one(t)
			if d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeWouldAsk ||
				d.Question != chiefnudge.GenericAskQuestion("") || !chiefnudge.IsGenericAsk(d.Options) {
				t.Fatalf("not the generic ask: %+v", d)
			}
			want := "classifier: needs_input, ask score 0.95 — the default is an ask (the chief said leave it; re-ask: no question (" +
				tc.why + ") — generic ask); it is done"
			if got := f.rowReason(t, d.ID); got != want {
				t.Fatalf("reason on the row:\n got %q\nwant %q", got, want)
			}
			if checks, _ := f.gw.seen(); len(checks) != 2 {
				t.Fatalf("chief calls: %d, want 2: %v", len(checks), checks)
			}
		})
	}
}

// TestChiefNudgeNoReaskWhenTheAskNeedsNone: a leave under the threshold is
// left, and an ask-stall whose leave carries a question uses it — one call
// each, no re-ask.
func TestChiefNudgeNoReaskWhenTheAskNeedsNone(t *testing.T) {
	f := newNudgeFixture(t, false)
	j := withJev(t, f)
	j.set(200, 0.10, 0.05, 0.85)
	f.gw.set(nudgeLeaveReply)
	f.pass(t)
	if d := f.one(t); d.Decision != chiefnudge.DecisionLeave || d.Outcome != chiefnudge.OutcomeLeft || f.rowReason(t, d.ID) != "it is done" {
		t.Fatalf("under the threshold: %+v", d)
	}
	if checks, _ := f.gw.seen(); len(checks) != 1 {
		t.Fatalf("chief calls under the threshold: %d, want 1", len(checks))
	}

	j.set(200, 0.90, 0.05, 0.05)
	f.gw.set(`{"decision":"leave","reason":"it is done","question":"Publish the draft now?","options":["Yes","No"]}`)
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 60_000 })
	f.pass(t)
	if d := f.decisions(t)[0]; d.Question != "Publish the draft now?" || strings.Contains(d.Reason, "re-ask") {
		t.Fatalf("an ask-stall leave with a question: %+v", d)
	}
	if checks, _ := f.gw.seen(); len(checks) != 2 {
		t.Fatalf("chief calls after the second pass: %d, want 2 (one each)", len(checks))
	}
}

// TestChiefNudgeTheChiefsReasonIsOnEveryRow: the chief's reason is written to
// chief_nudges.reason whatever became of the decision — a shadow nudge, a
// shadow ask, a nudge the quote guard refused. (An invariant guard: muster
// wrote it before the re-ask; the re-ask's own annotation is pinned above.)
func TestChiefNudgeTheChiefsReasonIsOnEveryRow(t *testing.T) {
	f := newNudgeFixture(t, false)
	step := func(reply, outcome, reason string) {
		t.Helper()
		f.gw.set(reply)
		f.state(func(s *mirror.AgentState) { s.Since++ })
		f.pass(t)
		d := f.decisions(t)[0]
		if d.Outcome != outcome || f.rowReason(t, d.ID) != reason {
			t.Fatalf("want %s with reason %q: %+v", outcome, reason, d)
		}
	}
	step(nudgeReply(nudgeOwnerID, "keep going until merged", "merge the pull request"), chiefnudge.OutcomeWouldSend, "CI is green and the owner said to merge")
	step(nudgeAskReply, chiefnudge.OutcomeWouldAsk, "it asked which branch")
	step(nudgeReply(nudgeOwnerID, "this phrase is not the owner's", "merge the pull request"), chiefnudge.Suppressed(chiefnudge.GuardQuote), "CI is green and the owner said to merge")
	step(nudgeLeaveReply, chiefnudge.OutcomeLeft, "it is done")
}
