package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE STATUS HOOK IS COALESCED PER OWNER AND ITS READS ARE BOUNDED PER STREAK
// (episodeStatusMoved; audit of #887 round 1, 🟡2), and it acts only on a
// PUSHED stop whose window MOVED (🟢).

func (f *stateFixture) statusRunCount() int {
	f.srv.statusRuns.mu.Lock()
	defer f.srv.statusRuns.mu.Unlock()
	return f.srv.statusRuns.runs
}

// TestConcurrentStatusTriggersCollapseIntoOneTrailingRun: while one run of the
// hook is inside its transcript read, nine more triggers for the same owner
// land. They collapse into ONE trailing run (2 runs in all), and the trailing
// run does not read the same streak again inside the negative TTL (1 read).
func TestConcurrentStatusTriggersCollapseIntoOneTrailingRun(t *testing.T) {
	f := newStateFixture(t, false)
	f.pushedStall(t)
	f.advance(pushBroadcastTimeout + time.Second)
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0) // moved, with no owner line: a negative read
	// Only the FIRST read blocks (until released); any other read — a run that
	// was not coalesced — goes straight through, so the count below can see it.
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	inner := f.srv.nudgeReadHook
	f.srv.nudgeReadHook = func(ctx context.Context, c chiefnudge.Session) (nudgeRead, error) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return inner(ctx, c)
	}
	runs0, reads0 := f.statusRunCount(), f.readCount()
	done := make(chan struct{})
	go func() { f.srv.EpisodeStatusMoved(authz.OperatorUserID); close(done) }()
	<-entered
	var wg sync.WaitGroup
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.srv.EpisodeStatusMoved(authz.OperatorUserID) }()
	}
	wg.Wait() // each returned at once: a run was in progress
	close(release)
	<-done
	if got := f.statusRunCount() - runs0; got != 2 {
		t.Fatalf("runs for 10 concurrent triggers: %d, want 2 (one, then one trailing)", got)
	}
	if got := f.readCount() - reads0; got != 1 {
		t.Fatalf("transcript reads: %d, want 1 (the trailing run is inside the negative TTL)", got)
	}
}

// TestALaggingTranscriptEndsOnTheNextTriggerAfterTheTTL: the hook reads before
// the owner's line is readable. A trigger inside the negative TTL reads
// nothing; the first one after it reads, finds the line and ends the stop.
func TestALaggingTranscriptEndsOnTheNextTriggerAfterTheTTL(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.advance(pushBroadcastTimeout + time.Second)
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.srv.EpisodeStatusMoved(authz.OperatorUserID)
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.advance(episodeStatusRecheck / 2)
	reads := f.readCount()
	f.srv.EpisodeStatusMoved(authz.OperatorUserID)
	if f.readCount() != reads {
		t.Fatal("a trigger inside the negative TTL read the transcript again")
	}
	if e := f.episodeByID(t, id); !e.Open() {
		t.Fatalf("ended inside the TTL: %+v", e)
	}
	f.advance(episodeStatusRecheck)
	f.srv.EpisodeStatusMoved(authz.OperatorUserID)
	if e := f.episodeByID(t, id); e.EndedAt.IsZero() || e.EndReason != endOwner {
		t.Fatalf("the first trigger after the TTL: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endOwner)
	}
	if f.resolvedCount(tag) != 1 {
		t.Fatalf("pushes: %v", f.pushes())
	}
}

// TestALaggingTranscriptEndsOnTheHooksOwnRecheck: no further status move at
// all — the read that found nothing scheduled ONE re-check after the TTL, and
// that re-check ends the stop once the line is readable. A second miss on the
// same streak schedules no more.
func TestALaggingTranscriptEndsOnTheHooksOwnRecheck(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.advance(pushBroadcastTimeout + time.Second)
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.srv.EpisodeStatusMoved(authz.OperatorUserID)
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.advance(episodeStatusRecheck)
	if waits := f.rechecks.fire(); len(waits) != 1 || waits[0] != episodeStatusRecheck {
		t.Fatalf("re-checks: %v, want one of %v", waits, episodeStatusRecheck)
	}
	if e := f.episodeByID(t, id); e.EndedAt.IsZero() || e.EndReason != endOwner || f.resolvedCount(tag) != 1 {
		t.Fatalf("after the re-check: ended %v reason %q, pushes %v", e.EndedAt, e.EndReason, f.pushes())
	}
}

// TestARecheckIsScheduledOncePerStreak: a streak whose re-check also finds
// nothing schedules no further re-check (the pass reads it next).
func TestARecheckIsScheduledOncePerStreak(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	f.advance(pushBroadcastTimeout + time.Second)
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.srv.EpisodeStatusMoved(authz.OperatorUserID)
	f.advance(episodeStatusRecheck)
	if waits := f.rechecks.fire(); len(waits) != 1 {
		t.Fatalf("re-checks after the first miss: %v", waits)
	}
	f.advance(episodeStatusRecheck)
	if waits := f.rechecks.fire(); len(waits) != 0 {
		t.Fatalf("re-checks after the re-check's own miss: %v, want none", waits)
	}
	if e := f.episodeByID(t, id); !e.Open() {
		t.Fatalf("ended with no owner line: %+v", e)
	}
}

// TestAnUnpushedStopIsNotTheStatusHooks (🟢, the pushed filter): an open stop
// that was never pushed, the owner's line in its transcript and its window
// moved, is not ended by the hook — nothing to withdraw; the pass ends it —
// and no transcript is read for it.
func TestAnUnpushedStopIsNotTheStatusHooks(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE session_episodes SET pushed_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	f.advance(pushBroadcastTimeout + time.Second)
	f.ownerTypes(t, id)
	reads := f.readCount()
	f.srv.EpisodeStatusMoved(authz.OperatorUserID)
	if e := f.episodeByID(t, id); !e.Open() {
		t.Fatalf("the status hook ended an unpushed stop: ended %v reason %q", e.EndedAt, e.EndReason)
	}
	if f.readCount() != reads {
		t.Fatal("the status hook read a transcript for an unpushed stop")
	}
}

// TestAWindowThatHasNotMovedIsNotRead (🟢, the streak guard): the owner's line
// is in the transcript but the window sits on the episode's own streak (no
// status change since the stop). The hook reads nothing and ends nothing; it
// acts only on a window that moved. Run on a RESTARTED process, whose
// owner-acted record (ownerChecked, in memory) is empty: in the process that
// opened the stop that record alone already skips the read, and would hide a
// missing guard.
func TestAWindowThatHasNotMovedIsNotRead(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	f.restart()
	f.holdClearRetries()
	f.advance(pushBroadcastTimeout + time.Second)
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	reads := f.readCount()
	f.srv.EpisodeStatusMoved(authz.OperatorUserID)
	if e := f.episodeByID(t, id); !e.Open() {
		t.Fatalf("the status hook ended a stop whose window never moved: ended %v reason %q", e.EndedAt, e.EndReason)
	}
	if f.readCount() != reads {
		t.Fatal("the status hook read a transcript for a window that never moved")
	}
}
