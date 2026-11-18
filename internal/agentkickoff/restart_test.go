package agentkickoff

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// TestAPanickedRunDoesNotSayDoneAndTheRestartedRunIsWaitedFor (#654 round 2):
// the deliverer runs under api.Supervise, which restarts a Run that panics.
// Shutdown waits on Done for in-flight first turns to record their outcome.
// Before the fix the panicked Run's unwind closed Done, so after the restart
// shutdown stopped waiting at once while the restarted Run had a turn in
// flight. Here: the first Run panics (its pass), Done stays open; the
// restarted Run starts a turn that blocks; Done stays open while it is in
// flight; cancelling ends the turn, and only then does Done close (the
// CONTROL that Done can close at all).
func TestAPanickedRunDoesNotSayDoneAndTheRestartedRunIsWaitedFor(t *testing.T) {
	row := ownedRow()
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, row)
	inFlight := make(chan struct{})
	h.gw.hold = inFlight
	h.insts.panics.Store(1)
	// What api.Supervise does (it cannot be imported here: api imports this
	// package): run, recover a panic, run again.
	runOnce := func(ctx context.Context) (panicked bool) {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		h.d.Run(ctx, time.Millisecond)
		return false
	}
	if !runOnce(context.Background()) {
		t.Fatal("fixture: the first Run did not panic")
	}
	select {
	case <-h.d.Done():
		t.Fatal("a Run that PANICKED closed Done: shutdown would stop waiting for the restarted Run's turns")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runOnce(ctx)
	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatalf("the restarted Run started no turn.%s", transcript(h.log.snapshot()))
	}
	select {
	case <-h.d.Done():
		t.Fatal("Done closed with the restarted Run's turn still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-h.d.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("control: Done did not close after the restarted Run stopped and its turn recorded")
	}
	if h.gw.callCount() != 1 {
		t.Errorf("the gateway was called %d times; want the one turn", h.gw.callCount())
	}
}
