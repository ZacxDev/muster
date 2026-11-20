package api

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// A STOP RESUMED FROM 'gone' IS RE-SHOWN SILENTLY (reshowEpisode) when its new
// episode opens, carrying the FIRST push's pushed_at. Its withdrawal must not
// overtake that re-show either: held from the re-show, not from pushed_at
// minutes back (clearHoldFrom). Audit of #887 round 1, 🟡1.

// resumedAndTyped is a pushed stop gone and back (re-shown), then, a second
// later, the owner typing into it and the status hook running. It returns the
// resumed episode's id and tag.
func resumedAndTyped(t *testing.T, f *stateFixture, before func(id int64)) (int64, string) {
	t.Helper()
	first := pushedStop(t, f)
	back := f.goneAndBack(t, first, nil)
	if got := f.reshows(); len(got) != 1 {
		t.Fatalf("precondition: re-shows %v, want the resumed stop re-shown once", got)
	}
	if before != nil {
		before(back.ID)
	}
	f.advance(time.Second)
	f.ownerTypes(t, back.ID)
	f.srv.MirrorStatusChanged(authz.OperatorUserID)
	if e := f.episodeByID(t, back.ID); e.EndedAt.IsZero() || e.EndReason != endOwner {
		t.Fatalf("after the owner typed: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endOwner)
	}
	return back.ID, "attention-episode-" + strconv.FormatInt(back.ID, 10)
}

// TestAResumedStopEndedDuringItsReshowIsHeldUntilTheReshowIsOut (regression):
// the owner types 1 s after the re-show. The hook ends the stop, but its
// `resolved` is held for the rest of the re-show's fan-out, then sent once by
// the retry. Red at 7e0d484ce: the hold was measured from the first push's
// pushed_at, so `resolved` went at once, racing the re-show.
func TestAResumedStopEndedDuringItsReshowIsHeldUntilTheReshowIsOut(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := resumedAndTyped(t, f, nil)
	if n := f.resolvedCount(tag); n != 0 || f.cleared(t, id) {
		t.Fatalf("withdrawn 1 s after its re-show (%d resolved, cleared %v): it could overtake the re-show", n, f.cleared(t, id))
	}
	f.advance(pushBroadcastTimeout - time.Second)
	f.retries.fire()
	if n := f.resolvedCount(tag); n != 1 || !f.cleared(t, id) {
		t.Fatalf("after the re-show's fan-out: %d resolved (cleared %v), want 1", n, f.cleared(t, id))
	}
	f.nextPass(t)
	if n := f.resolvedCount(tag); n != 1 {
		t.Fatalf("withdrawals after the backstop: %d, want exactly 1", n)
	}
}

// TestAResumedStopReshownByAnotherProcessIsHeldFromItsOpening (regression): the
// re-show was sent by another replica (or before a restart), so this process
// holds no stamp of it; the resumed episode's opened_at — when that process
// opened and re-showed it — holds the withdrawal instead.
func TestAResumedStopReshownByAnotherProcessIsHeldFromItsOpening(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := resumedAndTyped(t, f, func(id int64) {
		f.srv.clearRetry.forgetReshow(id) // another process re-showed it
		f.mu.Lock()
		now := f.clock
		f.mu.Unlock()
		if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE session_episodes SET opened_at = $2 WHERE id = $1`, id, now); err != nil {
			t.Fatal(err)
		}
	})
	if n := f.resolvedCount(tag); n != 0 || f.cleared(t, id) {
		t.Fatalf("withdrawn 1 s after another process re-showed it (%d resolved, cleared %v)", n, f.cleared(t, id))
	}
	f.advance(pushBroadcastTimeout)
	f.retries.fire()
	if n := f.resolvedCount(tag); n != 1 {
		t.Fatalf("after the fan-out: %d resolved, want 1", n)
	}
}

// TestAResumedStopEndedAfterItsReshowIsOutIsWithdrawnAtOnce — the other side:
// past the re-show's fan-out the withdrawal is not held.
func TestAResumedStopEndedAfterItsReshowIsOutIsWithdrawnAtOnce(t *testing.T) {
	f := newStateFixture(t, false)
	first := pushedStop(t, f)
	back := f.goneAndBack(t, first, nil)
	f.advance(pushBroadcastTimeout + time.Second)
	f.ownerTypes(t, back.ID)
	f.srv.MirrorStatusChanged(authz.OperatorUserID)
	if n := f.resolvedCount("attention-episode-" + strconv.FormatInt(back.ID, 10)); n != 1 {
		t.Fatalf("withdrawals 16 s after the re-show: %d, want 1 at once", n)
	}
}
