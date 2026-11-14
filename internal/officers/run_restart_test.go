package officers_test

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestAPanickedReconcilerRunNeitherSaysDoneNorClosesTwice (#654 round 2):
// the wake loop runs under api.Supervise, which restarts a Run that panics.
// The panicked Run must NOT close Done (shutdown would stop waiting while the
// restarted Run still works), and the restarted Run's own return must close
// it exactly once — before the fix the second close panicked on a closed
// channel. The CONTROL is that Done closes when the restarted Run returns.
func TestAPanickedReconcilerRunNeitherSaysDoneNorClosesTwice(t *testing.T) {
	saved := officers.RunTick
	officers.RunTick = time.Millisecond
	t.Cleanup(func() { officers.RunTick = saved })
	var calls atomic.Int32
	now := func() time.Time {
		if calls.Add(1) == 1 {
			panic("probe: the first pass panics")
		}
		return time.Now()
	}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry(nil), HQ: officers.NewBackends(nil),
		Fleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}, Logger: log.New(io.Discard, "", 0), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	// What api.Supervise does (it cannot be imported here: api imports
	// officers): run, recover a panic, run again.
	runOnce := func(ctx context.Context) (panicked bool) {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		rec.Run(ctx)
		return false
	}
	if !runOnce(context.Background()) {
		t.Fatal("fixture: the first Run did not panic")
	}
	select {
	case <-rec.Done():
		t.Fatal("a Run that PANICKED closed Done: shutdown would stop waiting while the restarted Run works")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan bool, 1)
	go func() { returned <- runOnce(ctx) }()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-rec.Done():
		t.Fatal("Done closed while the restarted Run was still running")
	default:
	}
	cancel()
	if panicked := <-returned; panicked {
		t.Fatal("the restarted Run panicked on its return (closing Done twice)")
	}
	select {
	case <-rec.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("control: Done did not close when the restarted Run returned")
	}
}
