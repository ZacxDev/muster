package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// #433 ROUND 1 (session-state step 3a, last fixes): the needs-you gate at an
// agent's turn; a nudge queued before the deploy (no episode) under session
// state; a nudge still queued when its stop was pushed is never pasted; and an
// old stop's decision never quiets a new stop on the same streak.

// queuedAgentNudge records, for a fresh worker with a stalled conversation, a
// LIVE agent nudge whose turn could not start (outcome queued, no send_queue
// row — what actNudge leaves when a turn began that instant), classified p and
// taken in episode (0: none). The sweep's reconcile retries it
// (sendNudgeTurn). The mirrored window is removed so nothing else is judged.
func queuedAgentNudge(t *testing.T, f *nudgeFixture, p [3]float64, episode int64) (chiefnudge.Decision, agents.Agent) {
	t.Helper()
	f.srv.nudgeReadHook = nil
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "r1worker-"+suffix, "worker", "r1-worker-tok-"+suffix)
	sess, _ := f.stallAgent(t, worker, authz.OperatorUserID)
	var last int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT max(id) FROM chat_messages WHERE session_id = $1`, sess.ID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	label := chiefnudge.LabelNeither
	switch {
	case p[0] >= p[1] && p[0] >= p[2]:
		label = chiefnudge.LabelNeedsInput
	case p[1] >= p[2]:
		label = chiefnudge.LabelAsksPermission
	}
	d, ok, err := f.nudges.Insert(asUser(authz.OperatorUserID), chiefnudge.Decision{Kind: chiefnudge.KindAgent,
		Key: chiefnudge.AgentKey(worker.ID, sess.ID), AgentID: worker.ID, ChatSessionID: sess.ID, StreakKey: "msg:" + strconv.FormatInt(last, 10),
		Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeLive, Outcome: chiefnudge.OutcomeQueued,
		Message:  chiefnudge.NudgeLabel + ` Your owner said earlier in this session: "keep going until merged". Continue.`,
		JevLabel: label, JevNeedsInput: p[0], JevAsksPermission: p[1], JevNeither: p[2], JevAskScore: p[0] + p[1], EpisodeID: episode}, "")
	if err != nil || !ok {
		t.Fatalf("seed the decision: %v %v", ok, err)
	}
	return d, worker
}

// nudgeByID is decision id as it stands now.
func (f *nudgeFixture) nudgeByID(t *testing.T, id int64) chiefnudge.Decision {
	t.Helper()
	d, err := f.nudges.Get(asUser(authz.OperatorUserID), id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestTheNeedsYouGateHoldsAtAnAgentsTurn (round 1, 🟡 1): a live needs-you
// nudge to an agent whose turn could not start is retried by the sweep's
// reconcile. On both lists the retry SENDS it (the positive control: the path
// is reached and ends in a turn); with the owner taken off the needs-you list
// before the retry, it fails with the kill switch's words and no turn starts.
func TestTheNeedsYouGateHoldsAtAnAgentsTurn(t *testing.T) {
	t.Run("on both lists: sent", func(t *testing.T) {
		f := newNudgeFixture(t, true)
		needsYouLive(f, authz.OperatorUserID)
		d, worker := queuedAgentNudge(t, f, jevAsked, 4242)
		f.pass(t)
		if got := f.nudgeByID(t, d.ID); got.Outcome != chiefnudge.OutcomeSent {
			t.Fatalf("decision %+v", got)
		}
		waitFor(t, func() bool { _, turns := f.gw.seen(); return len(turns) > 0 })
		if _, turns := f.gw.seen(); len(turns) != 1 || !strings.HasPrefix(turns[0], worker.Name+" "+chiefnudge.NudgeLabel) {
			t.Fatalf("turns %v", turns)
		}
	})
	t.Run("taken off the needs-you list: not sent", func(t *testing.T) {
		f := newNudgeFixture(t, true)
		needsYouLive(f, authz.OperatorUserID)
		d, _ := queuedAgentNudge(t, f, jevAsked, 4242)
		needsYouLive(f) // off the needs-you list before the retry; still on the old one
		f.pass(t)
		if got := f.nudgeByID(t, d.ID); got.Outcome != chiefnudge.OutcomeFailed || got.Detail != SendQueueNudgeOff {
			t.Fatalf("a needs-you agent nudge off its list: %+v", got)
		}
		time.Sleep(200 * time.Millisecond)
		if _, turns := f.gw.seen(); len(turns) != 0 {
			t.Fatalf("turns %v", turns)
		}
	})
}

// TestANudgeQueuedBeforeTheDeployIsHeldToTheNeedsYouRule (round 1, 🟢 2): a
// queued live nudge with NO episode — what the image before step 3a queued —
// for a stop the classifier says asked is, with session state on, held to the
// needs-you rule at its send: an owner on the old list alone does not get it.
// The control: the same nudge for a stop that asked nothing is still sent.
// RED at 8620057e: sent on the old list alone.
func TestANudgeQueuedBeforeTheDeployIsHeldToTheNeedsYouRule(t *testing.T) {
	for _, c := range []struct {
		name    string
		p       [3]float64
		outcome string
	}{
		{"asked: held to the needs-you list", jevAsked, chiefnudge.OutcomeFailed},
		{"asked nothing: sent", jevNothing, chiefnudge.OutcomeSent},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newNeedsYouFixture(t, jevNothing, true)
			d, _ := queuedAgentNudge(t, f.nudgeFixture, c.p, 0)
			f.pass(t)
			if got := f.nudgeByID(t, d.ID); got.Outcome != c.outcome {
				t.Fatalf("decision %+v, want %s", got, c.outcome)
			}
		})
	}
}

// TestANudgeStillQueuedWhenItsStopWasPushedIsNeverPasted (round 1, step-2 gap
// a): a live nudge not sent within episodeNudgeHold lets its stop push; after
// that the send queue must not paste it too — the owner was told, so the
// nudge is dropped (superseded). RED at 8620057e: pasted after the push.
func TestANudgeStillQueuedWhenItsStopWasPushedIsNeverPasted(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.judge(t)
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeQueued {
		t.Fatalf("precondition: %+v", d)
	}
	f.advance(episodeNudgeHold + time.Minute)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes after the hold: %v", got)
	}
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("pasted after its stop was pushed: %+v", p)
	}
	f.pass(t)
	if d := f.one(t); d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardSuperseded) {
		t.Fatalf("decision %+v", d)
	}
}

// TestAnOldStopsDecisionNeverQuietsANewStop (round 1, step-2 gap b): the chief
// left a stop that asked nothing (quiet). The stop ENDS on the owner's action
// ('owner': the owner answered, so any stop after it is new — though, read a
// pass late, it can end carrying the same streak), and a NEW stop opens on
// the SAME streak. The old stop's leave must not quiet it: it pushes (once the
// chief's wait has passed — it is never judged again, its streak being
// decided). RED at 8620057e: no push. (Until #635 this test ended the first
// stop 'gone'; a stop back from 'gone' on its streak is now the SAME stop and
// keeps the chief's decision — TestAQuietedStopBackFromGoneStaysQuiet.)
func TestAnOldStopsDecisionNeverQuietsANewStop(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.judge(t)
	first := f.oneEpisode(t)
	if q := f.quietOf(t, first.ID); q != sessionstate.QuietLeft {
		t.Fatalf("precondition: quiet %q", q)
	}
	if ended, err := f.eps.End(asUser(authz.OperatorUserID), first.ID, sessionstate.EndOwner, f.srv.now()); err != nil || !ended {
		t.Fatalf("end: %v %v", ended, err)
	}
	f.statePass(t)
	f.advance(episodeChiefWait + time.Minute)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 || list[0].ID == first.ID || !list[0].Open() || list[0].StreakKey != first.StreakKey {
		t.Fatalf("episodes: %+v", list)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes for the new stop: %v", got)
	}
	if q := f.quietOf(t, list[0].ID); q != "" {
		t.Fatalf("the new stop is quiet %q", q)
	}
}
