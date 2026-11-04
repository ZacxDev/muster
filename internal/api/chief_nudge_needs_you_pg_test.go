package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// SESSION STATE STEP 3a end to end on a real Postgres
// (docs/design/session-state.md, "The Chief: nudge, leave or notify"): the
// chief answers nudge, leave or notify; a NEEDS-YOU session (the classifier
// says it asked) may be nudged with the owner's words, but only under its own
// live gate (MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS, ON TOP OF the old
// list); a nudge is handled only once SENT; a queued one is dropped when its
// streak or episode changes; shadow never quiets a stop; a failure is a
// notify; a leave quiets only a stop that asked nothing; a view-only runtime is
// never nudged; a waiting window is never nudged; an episode is judged at most
// once.
//
// 🔴 These tests use only names that exist at the base too, except the gate's
// knob, which goes through needsYouLive (chief_nudge_needs_you_gate_test.go) —
// so each REGRESSION test is shown red against the base by running this file
// there with a no-op needsYouLive. Outcome and decision spellings that are new
// in 3a are literals for the same reason.

// The classifier's three probabilities (needs_input, asks_permission,
// neither) for a stop that ASKED (ask score 0.91: needs-you) and for one that
// asked nothing.
var (
	jevAsked   = [3]float64{0.81, 0.10, 0.09}
	jevNothing = [3]float64{0.02, 0.03, 0.95}
	// jevPermission is a stop over the line labelled asks_permission (ask
	// score 0.93) — the one class of nudge the base ever sent live.
	jevPermission = [3]float64{0.05, 0.88, 0.07}
)

// newNeedsYouFixture is the state fixture with the classifier wired to
// answer p, and the operator live on the old list when live.
func newNeedsYouFixture(t *testing.T, p [3]float64, live bool) (*stateFixture, *jevFake) {
	t.Helper()
	f := newStateFixture(t, true)
	j := withJev(t, f.nudgeFixture)
	j.set(200, p[0], p[1], p[2])
	if live {
		f.live()
	}
	return f, j
}

// judge is one stop's whole cycle as production runs it: the state pass opens
// the episode, the chief's sweep decides, and the state pass settles the push.
func (f *stateFixture) judge(t *testing.T) {
	t.Helper()
	f.statePass(t)
	f.pass(t)
	f.statePass(t)
}

func (f *stateFixture) checks() int {
	c, _ := f.gw.seen()
	return len(c)
}

// TestANeedsYouNudgeIsLiveOnlyForAnOwnerOnBothLists (regression; operator
// decision 2026-11-04, "Separate shadow gate"): a nudge to a stop the
// classifier says ASKED — quoting the owner's own words — is queued for real
// only for an owner on BOTH MUSTER_CHIEF_NUDGE_LIVE_OWNERS and
// MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS, and then holds the stop's push.
// On the old list alone, on the new list alone or on neither it is SHADOW:
// recorded as would_send, nothing queued, nothing pasted, and the stop pushed.
// RED at the base: a needs_input stop's nudge was turned into an ask there.
func TestANeedsYouNudgeIsLiveOnlyForAnOwnerOnBothLists(t *testing.T) {
	for _, c := range []struct {
		name           string
		live, needsYou bool
		mode, outcome  string
		queued, pushes int
	}{
		{"both lists", true, true, chiefnudge.ModeLive, chiefnudge.OutcomeQueued, 1, 0},
		{"only the old list", true, false, chiefnudge.ModeShadow, chiefnudge.OutcomeWouldSend, 0, 1},
		{"only the new list", false, true, chiefnudge.ModeShadow, chiefnudge.OutcomeWouldSend, 0, 1},
		{"neither list", false, false, chiefnudge.ModeShadow, chiefnudge.OutcomeWouldSend, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newNeedsYouFixture(t, jevAsked, c.live)
			if c.needsYou {
				needsYouLive(f.nudgeFixture, authz.OperatorUserID)
			}
			queued0 := f.queuedRows(t)
			f.judge(t)
			d := f.one(t)
			if d.Decision != chiefnudge.DecisionNudge || d.Mode != c.mode || d.Outcome != c.outcome ||
				d.JevLabel != chiefnudge.LabelNeedsInput || d.QuoteText != "keep going until merged" {
				t.Fatalf("decision %+v\nwant nudge, mode %s, outcome %s", d, c.mode, c.outcome)
			}
			if q := f.queuedRows(t) - queued0; q != c.queued {
				t.Fatalf("send_queue rows written: %d, want %d", q, c.queued)
			}
			if got := f.episodePushes(); len(got) != c.pushes {
				t.Fatalf("pushes: %v, want %d", got, c.pushes)
			}
			if c.mode == chiefnudge.ModeShadow {
				f.tick(t)
				if p := f.pastes(); len(p) != 0 {
					t.Fatalf("a shadow needs-you nudge was pasted: %+v", p)
				}
			}
		})
	}
}

// TestAStalledNudgeStaysLiveOnTheOldListAlone — an INVARIANT GUARD (green at
// the base too): the separate gate is for NEEDS-YOU nudges only. A stop that
// asked nothing, for an owner on MUSTER_CHIEF_NUDGE_LIVE_OWNERS and not the
// needs-you list, is nudged live exactly as before: queued, its push held.
func TestAStalledNudgeStaysLiveOnTheOldListAlone(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	queued0 := f.queuedRows(t)
	f.judge(t)
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionNudge || d.Mode != chiefnudge.ModeLive || d.Outcome != chiefnudge.OutcomeQueued {
		t.Fatalf("decision %+v", d)
	}
	if q := f.queuedRows(t) - queued0; q != 1 {
		t.Fatalf("send_queue rows written: %d, want 1", q)
	}
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes while the nudge is queued: %v", got)
	}
}

// TestANeedsYouNudgeIsHandledOnlyOnceItsPasteIsSent (regression): live on
// both lists, the nudge holds the push while queued; the send queue pastes it;
// the sweep then records it sent; only then is the stop quiet (nudged), with
// no push. RED at the base: no needs-you nudge was queued at all.
func TestANeedsYouNudgeIsHandledOnlyOnceItsPasteIsSent(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevAsked, true)
	needsYouLive(f.nudgeFixture, authz.OperatorUserID)
	f.judge(t)
	e := f.oneEpisode(t)
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes while queued: %v", got)
	}
	if q := f.quietOf(t, e.ID); q != "" {
		t.Fatalf("a queued nudge already counted as handled: quiet %q", q)
	}
	f.tick(t)
	p := f.pastes()
	if len(p) != 1 || !strings.HasPrefix(p[0].text, chiefnudge.NudgeLabel) {
		t.Fatalf("pastes: %+v", p)
	}
	f.pass(t) // the reconcile: the queued row was sent
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeSent {
		t.Fatalf("decision after the paste: %+v", d)
	}
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes after the nudge was sent: %v", got)
	}
	if q := f.quietOf(t, e.ID); q != sessionstate.QuietNudged {
		t.Fatalf("quiet %q, want %q", q, sessionstate.QuietNudged)
	}
}

// TestANeedsYouNudgeQueuedBeforeItsOwnerLeftTheListIsNotPasted (regression):
// the kill switch reaches a needs-you nudge already queued. The owner is taken
// off MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS (still on the old list): the
// paste is refused, the decision fails with the kill switch's words, and the
// stop pushes. RED at the base: nothing was queued.
func TestANeedsYouNudgeQueuedBeforeItsOwnerLeftTheListIsNotPasted(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevAsked, true)
	needsYouLive(f.nudgeFixture, authz.OperatorUserID)
	f.judge(t)
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeQueued {
		t.Fatalf("precondition: %+v", d)
	}
	needsYouLive(f.nudgeFixture) // off the needs-you list, on the old one still
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("pasted after the owner left the needs-you list: %+v", p)
	}
	f.pass(t)
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeFailed || d.Detail != SendQueueNudgeOff {
		t.Fatalf("decision: %+v", d)
	}
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAQueuedNudgeIsDroppedWhenItsEpisodeChanges (regression; F4): the stop
// the nudge was decided in ENDS and a new stop opens on the same streak before
// the paste. The paste is refused (the session moved on), the decision is
// superseded, and the new stop pushes. RED at the base: the paste went
// through into a stop nobody judged, and the stop stayed quiet.
func TestAQueuedNudgeIsDroppedWhenItsEpisodeChanges(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.judge(t)
	first := f.oneEpisode(t)
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeQueued {
		t.Fatalf("precondition: %+v", d)
	}
	ended, err := f.eps.End(asUser(authz.OperatorUserID), first.ID, sessionstate.EndGone, f.srv.now())
	if err != nil || !ended {
		t.Fatalf("end: %v %v", ended, err)
	}
	f.statePass(t) // the session is still stopped: a new episode opens
	if list := f.episodes(t); len(list) != 2 || !list[0].Open() || list[0].ID == first.ID {
		t.Fatalf("episodes: %+v", list)
	}
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("a nudge decided in an ended stop was pasted into the next: %+v", p)
	}
	f.pass(t)
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardSuperseded) {
		t.Fatalf("decision: %+v", d)
	}
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAQueuedNudgeIsDroppedWhenItsStreakChanges — an INVARIANT GUARD (the
// base drops it too): a new turn starts before the paste; the paste is
// refused and the decision superseded.
func TestAQueuedNudgeIsDroppedWhenItsStreakChanges(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.judge(t)
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 60_000 })
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("pasted into a newer streak: %+v", p)
	}
	f.pass(t)
	// The decision on the FIRST streak (the base also judges the new streak,
	// a separate rule: TestAnEpisodeIsJudgedAtMostOnce).
	var d chiefnudge.Decision
	for _, x := range f.decisions(t) {
		if x.StreakKey == "since:"+strconv.FormatInt(nudgeSince, 10) {
			d = x
		}
	}
	if d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardSuperseded) {
		t.Fatalf("decision: %+v", d)
	}
}

// TestAnEpisodeIsJudgedAtMostOnce (regression; "Per idle episode, at most
// once"): the chief leaves a stop that asked nothing (a quiet row). A busy blip
// shorter than sessionstate.EndAfter keeps the stop, and the session stops
// again on a new streak: the chief is NOT asked again, and the stop — now
// unjudged on its new streak — is pushed. RED at the base: the chief was asked
// again, said leave again, and the stop stayed quiet.
func TestAnEpisodeIsJudgedAtMostOnce(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.judge(t)
	e := f.oneEpisode(t)
	if q := f.quietOf(t, e.ID); q != sessionstate.QuietLeft || f.checks() != 1 {
		t.Fatalf("precondition: quiet %q, %d chief call(s)", q, f.checks())
	}
	f.status(mirror.StatusBusy, 10*time.Second)
	f.statePass(t)
	f.state(func(s *mirror.AgentState) {
		s.Status, s.Since, s.Observed = mirror.StatusIdle, nudgeSince+60_000, f.srv.now().Add(-20*time.Minute)
	})
	f.mirror.mu.Lock()
	f.mirror.windows[0].Status = mirror.StatusIdle
	f.mirror.mu.Unlock()
	f.statePass(t)
	f.pass(t)
	if n := f.checks(); n != 1 {
		t.Fatalf("the chief was asked %d times about one stop", n)
	}
	if n := len(f.decisions(t)); n != 1 {
		t.Fatalf("decisions: %d", n)
	}
	if got := f.oneEpisode(t); got.ID != e.ID {
		t.Fatalf("the blip ended the stop: %+v", got)
	}
	f.advance(episodeChiefWait + time.Minute)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestALeaveOnAStopThatAskedIsANotify (regression; decision 12, "Push it
// anyway"): the chief, live, says leave a stop the classifier says asked. It is
// recorded as a NOTIFY (nothing raised: no notice, no question) and the stop
// pushes. RED at the base: it was recorded as an ask.
func TestALeaveOnAStopThatAskedIsANotify(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevAsked, true)
	needsYouLive(f.nudgeFixture, authz.OperatorUserID)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	asks0 := f.askRows(t)
	f.judge(t)
	d := f.one(t)
	if d.Decision != "notify" || d.Outcome != "notified" || d.Question != "" || len(d.Options) != 0 ||
		!strings.Contains(d.Reason, "the chief said leave it") {
		t.Fatalf("decision %+v", d)
	}
	if got := f.audited(t, d.ID); len(got) != 1 || got[0] != "chief_nudge.notified" {
		t.Fatalf("audit %v", got)
	}
	if a := f.askRows(t); a != asks0 {
		t.Fatalf("a notice was raised: %d → %d", asks0, a)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestALeaveOnAStopThatAskedNothingStaysQuiet — an INVARIANT GUARD: leave
// still quiets a stop that asked nothing (a quiet row), in 3a's shape too.
func TestALeaveOnAStopThatAskedNothingStaysQuiet(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.judge(t)
	if d := f.one(t); d.Decision != chiefnudge.DecisionLeave || d.Outcome != chiefnudge.OutcomeLeft {
		t.Fatalf("decision %+v", d)
	}
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes: %v", got)
	}
	if q := f.quietOf(t, f.oneEpisode(t).ID); q != sessionstate.QuietLeft {
		t.Fatalf("quiet %q", q)
	}
}

// TestTheChiefIsAskedForNudgeLeaveOrNotifyAndNoQuestion (regression): with
// session state on, the stall check asks for nudge, leave or notify — no
// question, no options — and, for a stop that asked, says a nudge must quote
// the owner's words that ANSWER it. An "ask" answer is read as a notify. RED
// at the base: the check asked for a question.
func TestTheChiefIsAskedForNudgeLeaveOrNotifyAndNoQuestion(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevAsked, true)
	f.gw.set(nudgeAskReply)
	f.judge(t)
	f.gw.mu.Lock()
	prompts := append([]string(nil), f.gw.prompt...)
	f.gw.mu.Unlock()
	if len(prompts) != 1 {
		t.Fatalf("prompts: %d", len(prompts))
	}
	p := prompts[0]
	for _, want := range []string{`{"decision":"nudge"|"leave"|"notify"`, "write no question and no summary",
		"already ANSWERS the question it asked"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	for _, not := range []string{`"question"`, `"ask"`, "give the question"} {
		if strings.Contains(p, not) {
			t.Errorf("the prompt still asks for %q", not)
		}
	}
	if d := f.one(t); d.Decision != "notify" || d.Outcome != "notified" || d.Question != "" {
		t.Fatalf("an ask answer: %+v", d)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAFailedCheckIsANotifyNeverALeave — an INVARIANT GUARD: live, on a stop
// that asked nothing (the one a leave could quiet), a chief call that fails
// and an answer that cannot be read each push the stop.
func TestAFailedCheckIsANotifyNeverALeave(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*stateFixture)
		guard string
	}{
		{"the call failed", func(f *stateFixture) { f.gw.mu.Lock(); f.gw.fails = 1; f.gw.mu.Unlock() }, chiefnudge.GuardChiefError},
		{"the answer is unreadable", func(f *stateFixture) { f.gw.set("I think you should leave it.") }, chiefnudge.GuardInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newNeedsYouFixture(t, jevNothing, true)
			c.setup(f)
			f.judge(t)
			if d := f.one(t); d.Outcome != chiefnudge.Suppressed(c.guard) {
				t.Fatalf("decision %+v", d)
			}
			if got := f.episodePushes(); len(got) != 1 {
				t.Fatalf("pushes: %v", got)
			}
		})
	}
}

// TestAViewOnlyRuntimeIsNeverNudged (regression; default D3): an opencode
// pane the chief would nudge — on a stop that asked, the owner on both lists —
// is recorded as a notify saying why (view_only), nothing is queued, and the
// stop pushes. RED at the base: it was recorded as an ask.
func TestAViewOnlyRuntimeIsNeverNudged(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevAsked, true)
	needsYouLive(f.nudgeFixture, authz.OperatorUserID)
	f.mirror.mu.Lock()
	f.mirror.windows = []mirror.Window{{Session: "oc", ID: "@2", Index: 2, Active: true, Runtime: mirror.RuntimeOpenCode,
		AgentSession: ocSID, Status: mirror.StatusIdle, StatusSince: nudgeSince}}
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeOpenCode, Status: mirror.StatusIdle, Since: nudgeSince,
		Observed: f.srv.now().Add(-20 * time.Minute)}
	f.mirror.mu.Unlock()
	queued0 := f.queuedRows(t)
	f.judge(t)
	d := f.one(t)
	if d.Key != "opencode:"+ocSID || d.Decision != "notify" || d.Outcome != "notified" ||
		!strings.HasPrefix(d.Reason, "view_only: ") || d.Message != "" {
		t.Fatalf("decision %+v", d)
	}
	if q := f.queuedRows(t) - queued0; q != 0 {
		t.Fatalf("queued %d for a view-only pane", q)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestAWaitingWindowIsNeverNudged — an INVARIANT GUARD: a window waiting on
// a prompt (StatusWaiting), the owner on both lists and the classifier saying
// it asked, is pushed at once and never put to the chief or nudged.
func TestAWaitingWindowIsNeverNudged(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevAsked, true)
	needsYouLive(f.nudgeFixture, authz.OperatorUserID)
	f.status(mirror.StatusWaiting, 20*time.Minute)
	queued0 := f.queuedRows(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.judge(t)
	if n := f.checks(); n != 0 {
		t.Fatalf("a waiting window was put to the chief %d time(s)", n)
	}
	if n := len(f.decisions(t)); n != 0 {
		t.Fatalf("decisions: %+v", f.decisions(t))
	}
	if q := f.queuedRows(t) - queued0; q != 0 {
		t.Fatalf("queued %d for a waiting window", q)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestACancelledOrCappedNudgePushes — an INVARIANT GUARD: a queued nudge whose
// row is cancelled, and a nudge the hourly cap refuses, each push the stop.
func TestACancelledOrCappedNudgePushes(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		f, _ := newNeedsYouFixture(t, jevNothing, true)
		f.judge(t)
		d := f.one(t)
		if d.Outcome != chiefnudge.OutcomeQueued || d.SendQueueID == 0 {
			t.Fatalf("precondition: %+v", d)
		}
		if _, err := f.queue.Cancel(asUser(authz.OperatorUserID), d.SendQueueID, time.Now()); err != nil {
			t.Fatal(err)
		}
		f.pass(t)
		if d := f.one(t); d.Outcome != chiefnudge.OutcomeFailed {
			t.Fatalf("decision: %+v", d)
		}
		f.statePass(t)
		if got := f.episodePushes(); len(got) != 1 {
			t.Fatalf("pushes: %v", got)
		}
	})
	t.Run("capped", func(t *testing.T) {
		f, _ := newNeedsYouFixture(t, jevNothing, true)
		f.cfg.HourlyCap = 1
		if _, ok, err := f.nudges.Insert(asUser(authz.OperatorUserID), chiefnudge.Decision{Kind: chiefnudge.KindMirror,
			Key: "synthetic-other-session", StreakKey: "since:1", Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeLive,
			Outcome: chiefnudge.OutcomeSent}, ""); err != nil || !ok {
			t.Fatalf("seed: %v %v", ok, err)
		}
		f.judge(t)
		var d chiefnudge.Decision
		for _, x := range f.decisions(t) {
			if x.Key == nudgeSID {
				d = x
			}
		}
		if d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardHourlyCap) {
			t.Fatalf("decision: %+v", d)
		}
		if got := f.episodePushes(); len(got) != 1 {
			t.Fatalf("pushes: %v", got)
		}
	})
}

// TestTheSweepWaitsForTheStopsEpisode (regression): with session state on, a
// stop with no open episode yet is not put to the chief (the state pass opens
// it within its interval); once it has one, the decision is taken in it. RED
// at the base: the chief was asked before the episode existed, and the row
// named no episode.
func TestTheSweepWaitsForTheStopsEpisode(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.pass(t)
	if n := f.checks(); n != 0 {
		t.Fatalf("the chief was asked %d time(s) before the stop had an episode", n)
	}
	f.statePass(t)
	f.pass(t)
	e := f.oneEpisode(t)
	var id int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT coalesce(episode_id, 0) FROM chief_nudges WHERE owner_id = $1`,
		authz.OperatorUserID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != e.ID {
		t.Fatalf("the decision names episode %d, want %d", id, e.ID)
	}
}

// TestTheDatabaseHoldsOneDecisionPerEpisode — the claim behind "at most
// once": a second decision for the same episode, on another streak, is not
// recorded (inserted false, no error).
func TestTheDatabaseHoldsOneDecisionPerEpisode(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.judge(t)
	e := f.oneEpisode(t)
	var id int64
	_ = f.pool.QueryRow(authz.System(context.Background()), `SELECT coalesce(episode_id, 0) FROM chief_nudges WHERE owner_id = $1`,
		authz.OperatorUserID).Scan(&id)
	if id != e.ID {
		t.Fatalf("precondition: the decision names episode %d, want %d", id, e.ID)
	}
	_, err := f.pool.Exec(asUser(authz.OperatorUserID), `INSERT INTO chief_nudges (session_kind, session_key, streak_key, decision, mode, outcome, episode_id)
		VALUES ('mirror', $1, 'since:2', 'leave', 'shadow', 'left', $2)`, nudgeSID, e.ID)
	if err == nil {
		t.Fatal("a second decision for one episode was recorded")
	}
	var unique interface{ SQLState() string }
	if !errors.As(err, &unique) || unique.SQLState() != "23505" {
		t.Fatalf("err %v, want a unique violation", err)
	}
}

// TestAnAsksPermissionNudgeIsNeedsYou (regression; operator decision on #433
// round 0: asks_permission nudges count as needs-you): a stop over the line
// labelled asks_permission is needs-you, so its nudge is SHADOW with only the
// old list — recorded, not pasted, the stop pushed — and live only with both.
// RED at the base for the old-list case: there it was sent live.
func TestAnAsksPermissionNudgeIsNeedsYou(t *testing.T) {
	for _, c := range []struct {
		name           string
		needsYou       bool
		mode, outcome  string
		queued, pushes int
	}{
		{"only the old list", false, chiefnudge.ModeShadow, chiefnudge.OutcomeWouldSend, 0, 1},
		{"both lists", true, chiefnudge.ModeLive, chiefnudge.OutcomeQueued, 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newNeedsYouFixture(t, jevPermission, true)
			if c.needsYou {
				needsYouLive(f.nudgeFixture, authz.OperatorUserID)
			}
			queued0 := f.queuedRows(t)
			f.judge(t)
			d := f.one(t)
			if d.Decision != chiefnudge.DecisionNudge || d.JevLabel != chiefnudge.LabelAsksPermission || d.Mode != c.mode || d.Outcome != c.outcome {
				t.Fatalf("decision %+v\nwant an asks_permission nudge, mode %s, outcome %s", d, c.mode, c.outcome)
			}
			if q := f.queuedRows(t) - queued0; q != c.queued {
				t.Fatalf("send_queue rows written: %d, want %d", q, c.queued)
			}
			if got := f.episodePushes(); len(got) != c.pushes {
				t.Fatalf("pushes: %v, want %d", got, c.pushes)
			}
			if c.mode == chiefnudge.ModeShadow {
				f.tick(t)
				if p := f.pastes(); len(p) != 0 {
					t.Fatalf("a shadow asks_permission nudge was pasted: %+v", p)
				}
			}
		})
	}
}

// TestAStopWhoseClassifierFailedKeepsTheOldList — an INVARIANT GUARD (the
// operator's decision on #433 round 0: the classifier-failure scoping stays as
// built): a stop whose classifier failed is NOT needs-you for the gate, so its
// nudge is live on the old list alone; and a leave on it cannot quiet it — the
// leave rule counts a failed classifier as asked (sessionstate.Asked), so the
// stop pushes. The decision itself is recorded as the chief's leave: the
// conversion to notify is for a stop the classifier SAYS asked.
func TestAStopWhoseClassifierFailedKeepsTheOldList(t *testing.T) {
	failed := func(t *testing.T) *stateFixture {
		f, j := newNeedsYouFixture(t, jevNothing, true)
		j.set(502, 0, 0, 0)
		return f
	}
	t.Run("a nudge is live on the old list", func(t *testing.T) {
		f := failed(t)
		queued0 := f.queuedRows(t)
		f.judge(t)
		d := f.one(t)
		if d.Decision != chiefnudge.DecisionNudge || d.JevError != "http-502" || d.Mode != chiefnudge.ModeLive || d.Outcome != chiefnudge.OutcomeQueued {
			t.Fatalf("decision %+v", d)
		}
		if q := f.queuedRows(t) - queued0; q != 1 {
			t.Fatalf("send_queue rows written: %d, want 1", q)
		}
	})
	t.Run("a leave pushes", func(t *testing.T) {
		f := failed(t)
		f.gw.set(`{"decision":"leave","reason":"it is done"}`)
		f.judge(t)
		if d := f.one(t); d.Decision != chiefnudge.DecisionLeave || d.JevError != "http-502" {
			t.Fatalf("decision %+v", d)
		}
		if got := f.episodePushes(); len(got) != 1 {
			t.Fatalf("pushes: %v", got)
		}
		if q := f.quietOf(t, f.oneEpisode(t).ID); q != "" {
			t.Fatalf("quiet %q", q)
		}
	})
}

// TestASessionStateOffPermissionNudgeHasNoNewGate (round 0, D1; regression
// against 6060249f, where the needs-you gate leaked into state off): with
// MUSTER_SESSION_STATE unset, an asks_permission stop's nudge for an owner on
// the old list alone is sent live, exactly as before step 3a.
func TestASessionStateOffPermissionNudgeHasNoNewGate(t *testing.T) {
	f := newNudgeFixture(t, true)
	j := withJev(t, f)
	j.set(200, jevPermission[0], jevPermission[1], jevPermission[2])
	queued0 := f.queuedRows(t)
	f.pass(t)
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionNudge || d.JevLabel != chiefnudge.LabelAsksPermission || d.Mode != chiefnudge.ModeLive ||
		d.Outcome != chiefnudge.OutcomeQueued || d.EpisodeID != 0 {
		t.Fatalf("decision %+v", d)
	}
	if q := f.queuedRows(t) - queued0; q != 1 {
		t.Fatalf("send_queue rows written: %d, want 1", q)
	}
	// …and the paste gate lets it through (no needs-you gate at the paste).
	f.tick(t)
	if p := f.pastes(); len(p) != 1 || !strings.HasPrefix(p[0].text, chiefnudge.NudgeLabel) {
		t.Fatalf("pastes with session state off: %+v", p)
	}
}
