package api

import (
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// AN EPISODE'S NOTIFICATION IS WITHDRAWN THE MOMENT IT ENDS (endEpisode), not
// on the next state pass. The operator's report: a waiting prompt answered at
// the terminal kept its native notification up 5-35 s (about 17 s median) —
// the waiting hook ended the episode at once (episodeWaitsAnswered), but only
// the next pass's clearEndedEpisodes sent the `resolved` push.
//
// 🔴 These tests name only what exists at the base too (the fixture, the hub
// hook, the pass, the clock), so each REGRESSION test is shown red against the
// base by running it there.

// pushedWait opens a waiting window's episode from the hub hook and pushes it
// once its grace has passed, returning it with its notification tag.
func (f *stateFixture) pushedWait(t *testing.T) (id int64, tag string) {
	t.Helper()
	f.statusSince(mirror.StatusWaiting, nudgeSince+500_000, 5*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	tag = "attention-episode-" + strconv.FormatInt(e.ID, 10)
	f.pastGrace(t)
	if got := f.pushes(); len(got) != 1 || got[0] != "attention "+tag {
		t.Fatalf("the waiting push: %v", got)
	}
	return e.ID, tag
}

// TestAPromptAnsweredAtTheTerminalIsWithdrawnAtOnce (regression): a waiting
// window's episode is pushed; the owner answers the prompt at the terminal and
// the host reports the window busy. The hub hook alone — NO state pass — ends
// the episode 'owner' AND sends its `resolved` withdrawal, recording it
// cleared. Red at the base: the hook ended it, but the withdrawal waited for
// the next pass, so pushes held only the original notification.
func TestAPromptAnsweredAtTheTerminalIsWithdrawnAtOnce(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedWait(t)
	f.advance(pushBroadcastTimeout + time.Second) // past its push's send: see the moments-after test
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID) // the hook only: no pass
	got := f.episodeByID(t, id)
	if got.EndedAt.IsZero() || got.EndReason != endOwner {
		t.Fatalf("after the prompt was answered: ended %v reason %q, want %q", got.EndedAt, got.EndReason, endOwner)
	}
	if p := f.pushes(); len(p) != 2 || p[1] != "resolved "+tag {
		t.Fatalf("pushes with no pass run: %v, want the push then its withdrawal at once", p)
	}
	if !f.cleared(t, id) {
		t.Fatal("cleared_at not set by the hook's end")
	}
}

// TestAWithdrawalAtTheEndIsNotSentAgainByThePass (regression for the backstop
// staying single): after the hook withdrew the answered prompt's notification,
// the state passes that follow (clearEndedEpisodes, deferred on every one) and
// a second hook nudge send no second `resolved` for it.
func TestAWithdrawalAtTheEndIsNotSentAgainByThePass(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedWait(t)
	f.advance(pushBroadcastTimeout + time.Second) // past its push's send: see the moments-after test
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	for i := 0; i < 3; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	}
	resolved := 0
	for _, p := range f.pushes() {
		if p == "resolved "+tag {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("withdrawals of episode %d: %d, want exactly 1 (pushes %v)", id, resolved, f.pushes())
	}
}

// TestEndEpisodeWithdrawsAPushedEpisodeAtOnce (regression, endEpisode
// directly — every end reason goes through it): a pushed episode ended by
// endEpisode, with no pass, is withdrawn and recorded cleared; ending it again
// (End reports it already ended), the pass's clearEndedEpisodes backstop, and
// a clear from a copy read before the end's (the clear claim, MarkCleared)
// send nothing more.
func TestEndEpisodeWithdrawsAPushedEpisodeAtOnce(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedWait(t)
	f.advance(pushBroadcastTimeout + time.Second)
	ctx := asUser(authz.OperatorUserID)
	ep := f.episodeByID(t, id) // open: read whole, pushed_at included
	if ep.PushedAt.IsZero() || !ep.Open() {
		t.Fatalf("the fixture's episode is not open and pushed: %+v", ep)
	}
	f.srv.endEpisode(ctx, ep, "gone", f.srv.now())
	if p := f.pushes(); len(p) != 2 || p[1] != "resolved "+tag {
		t.Fatalf("pushes after endEpisode: %v, want the withdrawal at once", p)
	}
	if !f.cleared(t, id) {
		t.Fatal("cleared_at not set by endEpisode")
	}
	f.srv.endEpisode(ctx, ep, "gone", f.srv.now())
	f.srv.clearEndedEpisodes(ctx, authz.OperatorUserID)
	// The backstop racing the end: a copy read (ended, pushed, not yet
	// cleared) before the end claimed its clear. The claim refuses it.
	stale := ep
	stale.EndedAt, stale.EndReason = f.srv.now(), "gone"
	f.srv.clearEpisode(ctx, stale)
	if p := f.pushes(); len(p) != 2 {
		t.Fatalf("pushes after a second end, the backstop and a stale clear: %v, want no second withdrawal", p)
	}
}

// TestAnEpisodeEndedMomentsAfterItsPushIsWithdrawnByTheBackstop (regression,
// audit round 1): goNotify sends each push on its own goroutine, unordered per
// tag, and a device closes a `resolved` tag only if it shows it NOW — so a
// withdrawal sent moments after its push can land first, close nothing, and
// leave the push up for good (cleared_at set, the backstop never re-sends).
// An episode ended within pushBroadcastTimeout of its push is therefore NOT
// withdrawn at once, nor by a pass inside that window; it is left uncleared,
// and the first pass after it withdraws it exactly once. Red at d38006b71:
// the hook sent `resolved` 1 s after the push.
func TestAnEpisodeEndedMomentsAfterItsPushIsWithdrawnByTheBackstop(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedWait(t)
	f.advance(time.Second)
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	if got := f.episodeByID(t, id); got.EndedAt.IsZero() || got.EndReason != endOwner {
		t.Fatalf("after the prompt was answered: ended %v reason %q, want %q", got.EndedAt, got.EndReason, endOwner)
	}
	if p := f.pushes(); len(p) != 1 {
		t.Fatalf("pushes 1 s after the push: %v, want no withdrawal yet (it could overtake its push)", p)
	}
	if f.cleared(t, id) {
		t.Fatal("cleared_at set with no withdrawal sent: the backstop would never send it")
	}
	f.advance(5 * time.Second) // a pass still inside the window holds it too
	f.statePass(t)
	if p := f.pushes(); len(p) != 1 {
		t.Fatalf("pushes from a pass 6 s after the push: %v, want none yet", p)
	}
	f.advance(pushBroadcastTimeout)
	f.statePass(t)
	if p := f.pushes(); len(p) != 2 || p[1] != "resolved "+tag {
		t.Fatalf("pushes after the window: %v, want the backstop's withdrawal", p)
	}
	if !f.cleared(t, id) {
		t.Fatal("cleared_at not set by the backstop")
	}
	for i := 0; i < 2; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if p := f.pushes(); len(p) != 2 {
		t.Fatalf("pushes after more passes: %v, want exactly one withdrawal", p)
	}
}
