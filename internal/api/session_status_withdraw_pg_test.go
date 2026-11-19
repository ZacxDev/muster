package api

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A PUSHED IDLE STOP IS WITHDRAWN THE MOMENT THE OWNER TAKES THE WINDOW BACK
// (episodeStatusMoved, on the hub's status hook MirrorStatusChanged), not on
// the next state pass. The operator's report: an answered prompt's push was
// withdrawn at once (episodeWaitsAnswered), but a pushed "Stalled" stayed up
// until the 30 s pass read the transcript and found the owner's message.
//
// Each test drives the hook ALONE — no state pass — unless it says otherwise.

// clearRetryRec is the fixture's stand-in for time.AfterFunc behind
// clearEpisodeLater: it records each deferred withdrawal and runs none until
// the test fires them, so no real timer outlives a test.
type clearRetryRec struct {
	mu    sync.Mutex
	waits []time.Duration
	fns   []func()
}

func (r *clearRetryRec) after(d time.Duration, f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits, r.fns = append(r.waits, d), append(r.fns, f)
}

// fire runs every recorded retry, and reports their waits.
func (r *clearRetryRec) fire() []time.Duration {
	r.mu.Lock()
	fns, waits := r.fns, r.waits
	r.fns, r.waits = nil, nil
	r.mu.Unlock()
	for _, f := range fns {
		f()
	}
	return waits
}

// holdClearRetries installs a clearRetryRec on f's server (newStateFixture
// does, for every state test).
func (f *stateFixture) holdClearRetries() *clearRetryRec {
	r := &clearRetryRec{}
	f.srv.clearRetry.mu.Lock()
	f.srv.clearRetry.after = r.after
	f.srv.clearRetry.mu.Unlock()
	f.retries = r
	return r
}

// about reports whether d is want within a millisecond: pushed_at is stored
// to the microsecond, the fixture's clock to the nanosecond.
func about(d, want time.Duration) bool { return d > want-time.Millisecond && d < want+time.Millisecond }

// resolvedCount is how many withdrawals tag was sent.
func (f *stateFixture) resolvedCount(tag string) int {
	n := 0
	for _, p := range f.pushes() {
		if p == "resolved "+tag {
			n++
		}
	}
	return n
}

// ownerTypes is the owner answering the fixture's pushed stop at the terminal:
// a message of their own after it opened, and the window busy on a new streak.
func (f *stateFixture) ownerTypes(t *testing.T, id int64) {
	t.Helper()
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
}

// TestTheOwnerTypingIntoAStalledWindowWithdrawsItsPushAtOnce (regression): a
// stall is pushed; the owner types an answer at the terminal and the host
// reports the window busy. The status hook alone — NO pass — ends the episode
// 'owner' and sends its `resolved`, recording it cleared. Red at the base,
// where the hook was the ask log's alone: nothing ended until the next pass.
func TestTheOwnerTypingIntoAStalledWindowWithdrawsItsPushAtOnce(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.advance(pushBroadcastTimeout + time.Second) // past its push's fan-out
	f.ownerTypes(t, id)
	f.srv.MirrorStatusChanged(authz.OperatorUserID) // the hook only: no pass
	e := f.episodeByID(t, id)
	if e.EndedAt.IsZero() || e.EndReason != endOwner {
		t.Fatalf("after the owner typed: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endOwner)
	}
	if p := f.pushes(); len(p) != 2 || p[1] != "resolved "+tag {
		t.Fatalf("pushes with no pass run: %v, want the push then its withdrawal at once", p)
	}
	if !f.cleared(t, id) {
		t.Fatal("cleared_at not set by the hook's end")
	}
}

// TestAChiefNudgeOrAPasteMovingTheWindowIsNotTheOwner — an INVARIANT GUARD for
// the hook: muster's own words making the window busy (a chief nudge, a routed
// paste from the send queue) are not the owner acting. The read runs (a
// positive control on the reads) and the episode and its push stay.
func TestAChiefNudgeOrAPasteMovingTheWindowIsNotTheOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		text func(f *stateFixture) string
	}{
		{"a chief nudge", func(*stateFixture) string {
			quoted := chiefnudge.Msg{Role: "user", Text: nudgeOwnerText, ID: nudgeOwnerID, At: time.Now().Add(-10 * time.Minute)}
			return chiefnudge.NudgeText("Operator", quoted, "keep going until merged", time.UTC)
		}},
		{"a routed paste", func(f *stateFixture) string {
			body := "from the release thread: tag v2 once CI is green"
			if _, err := f.queue.Enqueue(asUser(authz.OperatorUserID), sendqueue.Item{Target: sendqueue.Target{Host: "ws", Session: "main", Window: "@1"},
				Body: body, StepUpAt: time.Now(), RouteID: "route-hook"}); err != nil {
				t.Fatal(err)
			}
			return body
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStateFixture(t, false)
			id, tag := f.pushedStall(t)
			f.advance(pushBroadcastTimeout + time.Second)
			f.say(chiefnudge.Msg{Role: "user", Text: tc.text(f), ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
			f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
			before := f.readCount()
			f.srv.MirrorStatusChanged(authz.OperatorUserID)
			if f.readCount() == before {
				t.Fatal("the hook never read the transcript: this test checked nothing")
			}
			if e := f.episodeByID(t, id); !e.Open() {
				t.Fatalf("muster's own words ended the episode: %+v", e)
			}
			if f.resolvedCount(tag) != 0 {
				t.Fatalf("pushes: %v", f.pushes())
			}
		})
	}
}

// TestTheOwnersMessageFromBeforeTheStopIsNotTheOwnerActingOnTheHook — an
// INVARIANT GUARD: the window moves with the owner's newest message the one
// from before the stop opened; the read runs and the episode stays.
func TestTheOwnersMessageFromBeforeTheStopIsNotTheOwnerActingOnTheHook(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.advance(pushBroadcastTimeout + time.Second)
	f.say(chiefnudge.Msg{Role: "user", Text: "and then tag it", ID: "line:8000", At: f.openedAt(t, id).Add(-time.Second)})
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	before := f.readCount()
	f.srv.MirrorStatusChanged(authz.OperatorUserID)
	if f.readCount() == before {
		t.Fatal("the hook never read the transcript: this test checked nothing")
	}
	if e := f.episodeByID(t, id); !e.Open() || f.resolvedCount(tag) != 0 {
		t.Fatalf("an owner message from before the stop ended it: %+v, pushes %v", e, f.pushes())
	}
}

// TestATranscriptBehindTheStatusIsReadAgainByThePass (regression for the
// record flag): the host reports the window busy before the owner's line is
// readable. The hook's read finds nothing and must NOT record the streak as
// read — the pass that follows reads it again, finds the owner's message and
// ends the episode. Recording the negative (record dropped) leaves it open.
func TestATranscriptBehindTheStatusIsReadAgainByThePass(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.advance(pushBroadcastTimeout + time.Second)
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	before := f.readCount()
	f.srv.MirrorStatusChanged(authz.OperatorUserID)
	if f.readCount() == before {
		t.Fatal("the hook never read the transcript: this test checked nothing")
	}
	if e := f.episodeByID(t, id); !e.Open() {
		t.Fatalf("ended with no owner line yet: %+v", e)
	}
	// The line lands; the window is still busy on the same streak.
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.advance(5 * time.Second)
	f.statePass(t)
	e := f.episodeByID(t, id)
	if e.EndedAt.IsZero() || e.EndReason != endOwner {
		t.Fatalf("the pass after a lagging transcript: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endOwner)
	}
	if f.resolvedCount(tag) != 1 {
		t.Fatalf("pushes: %v", f.pushes())
	}
}

// TestAStopEndedMomentsAfterItsPushIsWithdrawnByTheRetry (regression): the
// owner types 4 s after the push. The hook ends the episode at once but holds
// its `resolved` (it could overtake its push: clearEpisode), scheduling a retry
// for the rest of pushBroadcastTimeout — 11 s — which withdraws it; the pass's
// backstop after it sends nothing more. And the other order: the backstop
// first, then the retry — still ONE withdrawal.
func TestAStopEndedMomentsAfterItsPushIsWithdrawnByTheRetry(t *testing.T) {
	for _, backstopFirst := range []bool{false, true} {
		t.Run("backstop first "+strconv.FormatBool(backstopFirst), func(t *testing.T) {
			f := newStateFixture(t, false)
			id, tag := f.pushedStall(t)
			f.advance(4 * time.Second)
			f.ownerTypes(t, id)
			f.srv.MirrorStatusChanged(authz.OperatorUserID)
			if e := f.episodeByID(t, id); e.EndedAt.IsZero() || e.EndReason != endOwner {
				t.Fatalf("after the owner typed: ended %v reason %q", e.EndedAt, e.EndReason)
			}
			if p := f.pushes(); len(p) != 1 || f.cleared(t, id) {
				t.Fatalf("4 s after the push: pushes %v cleared %v, want the withdrawal held", p, f.cleared(t, id))
			}
			// The pass's backstop inside the window holds it too, and schedules
			// no second retry.
			f.statePass(t)
			if p := f.pushes(); len(p) != 1 {
				t.Fatalf("a pass inside the window withdrew it: %v", p)
			}
			f.advance(pushBroadcastTimeout - 4*time.Second)
			if backstopFirst {
				f.statePass(t)
				if f.resolvedCount(tag) != 1 {
					t.Fatalf("the backstop: %v", f.pushes())
				}
			}
			waits := f.retries.fire()
			if len(waits) != 1 || !about(waits[0], pushBroadcastTimeout-4*time.Second) {
				t.Fatalf("retries scheduled: %v, want one of %v", waits, pushBroadcastTimeout-4*time.Second)
			}
			if f.resolvedCount(tag) != 1 || !f.cleared(t, id) {
				t.Fatalf("after the retry: pushes %v cleared %v, want one withdrawal", f.pushes(), f.cleared(t, id))
			}
			f.advance(30 * time.Second)
			f.statePass(t)
			if n := f.resolvedCount(tag); n != 1 {
				t.Fatalf("withdrawals: %d, want exactly 1 (pushes %v)", n, f.pushes())
			}
		})
	}
}

// TestARetryFiringEarlyReschedulesItself: a retry whose clock still reads
// inside the window does not clear (that could overtake the push) — it
// schedules itself again for the remainder.
func TestARetryFiringEarlyReschedulesItself(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.advance(4 * time.Second)
	f.ownerTypes(t, id)
	f.srv.MirrorStatusChanged(authz.OperatorUserID)
	f.advance(5 * time.Second) // 9 s after the push: still inside
	if waits := f.retries.fire(); len(waits) != 1 {
		t.Fatalf("retries: %v", waits)
	}
	if f.resolvedCount(tag) != 0 {
		t.Fatalf("a retry 9 s after the push withdrew it: %v", f.pushes())
	}
	f.advance(10 * time.Second)
	if waits := f.retries.fire(); len(waits) != 1 || !about(waits[0], 6*time.Second) {
		t.Fatalf("the rescheduled retry: %v, want one of 6s", waits)
	}
	if f.resolvedCount(tag) != 1 || !f.cleared(t, id) {
		t.Fatalf("pushes %v cleared %v", f.pushes(), f.cleared(t, id))
	}
}

// TestTheHookAndThePassEndOneStopOnce: the hook ends the stop; the pass that
// follows (same moved window, owner line present) sends no second withdrawal.
func TestTheHookAndThePassEndOneStopOnce(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.advance(pushBroadcastTimeout + time.Second)
	f.ownerTypes(t, id)
	f.srv.MirrorStatusChanged(authz.OperatorUserID)
	f.statePass(t)
	f.srv.MirrorStatusChanged(authz.OperatorUserID)
	f.advance(30 * time.Second)
	f.statePass(t)
	if n := f.resolvedCount(tag); n != 1 {
		t.Fatalf("withdrawals: %d, want exactly 1 (pushes %v)", n, f.pushes())
	}
	if e := f.episodeByID(t, id); e.EndReason != endOwner {
		t.Fatalf("end reason %q", e.EndReason)
	}
}

// TestAWaitingEpisodeIsNotTheStatusHooks: a pushed WAITING episode (the
// waiting hook's) with an owner message after it opened and its window moved
// to idle is not ended by the status hook — that is episodeWaitsAnswered's —
// and the hook reads no transcript for it.
func TestAWaitingEpisodeIsNotTheStatusHooks(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedWait(t)
	f.advance(pushBroadcastTimeout + time.Second)
	f.say(chiefnudge.Msg{Role: "user", Text: "go ahead", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.statusSince(mirror.StatusIdle, nudgeSince+600_000, 0)
	before := f.readCount()
	f.srv.EpisodeStatusMoved(authz.OperatorUserID)
	if e := f.episodeByID(t, id); !e.Open() || f.resolvedCount(tag) != 0 {
		t.Fatalf("the status hook ended a waiting episode: %+v, pushes %v", e, f.pushes())
	}
	if f.readCount() != before {
		t.Fatal("the status hook read a transcript for a waiting episode")
	}
}

// TestTheStatusHookStillRunsTheAskLog: the composed hook (MirrorStatusChanged,
// what cmd/muster-server installs) still feeds the ask log — its two named
// opencode asks are logged through it, as through AskLogMirrorChanged.
func TestTheStatusHookStillRunsTheAskLog(t *testing.T) {
	pool := askLogPool(t)
	_, s, _ := askLogOpenCodeServer(t, asklog.NewPG(pool))
	s.MirrorStatusChanged(1)
	rows := asksOf(t, s, 1)
	if len(rows) != 2 {
		t.Fatalf("%d rows through the composed hook, want 2: %+v", len(rows), rows)
	}
}
