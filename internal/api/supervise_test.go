package api

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSuperviseKeepsALoopAliveAcrossAPanic: a loop that panics on its first
// two runs is logged, restarted (after the backoff) and its third run
// completes; the panic does not escape (it would kill this test binary). The
// CONTROL is a loop that returns normally: it runs once and is NOT restarted.
func TestSuperviseKeepsALoopAliveAcrossAPanic(t *testing.T) {
	setSuperviseTimes(t, time.Millisecond, 4*time.Millisecond, time.Hour)
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := log.New(lockedWriter{&mu, &buf}, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int32
	done := make(chan struct{})
	Supervise(ctx, logger, "probe-loop", func(context.Context) {
		if runs.Add(1) <= 2 {
			panic("probe: a pass met a team")
		}
		close(done)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the supervised loop was not restarted after its panics (ran %d times)", runs.Load())
	}
	if got := runs.Load(); got != 3 {
		t.Errorf("runs = %d; want 3 (two panics, one completion)", got)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if strings.Count(out, "PANIC in background loop probe-loop: probe: a pass met a team") != 2 {
		t.Errorf("the log does not record both panics:\n%s", out)
	}

	var normal atomic.Int32
	Supervise(ctx, logger, "control-loop", func(context.Context) { normal.Add(1) })
	time.Sleep(50 * time.Millisecond)
	if got := normal.Load(); got != 1 {
		t.Errorf("control: a loop that returned ran %d times; want once, not restarted", got)
	}
}

// setSuperviseTimes sets Supervise's backoff, cap and healthy period for one
// test and restores the production values after it.
func setSuperviseTimes(t *testing.T, backoff, max, healthy time.Duration) {
	t.Helper()
	SuperviseBackoff, SuperviseBackoffMax, SuperviseHealthy = backoff, max, healthy
	t.Cleanup(func() {
		SuperviseBackoff, SuperviseBackoffMax, SuperviseHealthy = 5*time.Second, 5*time.Minute, 5*time.Minute
	})
}

// TestSuperviseBackoffDoublesAndResetsAfterAHealthyRun: three quick panics
// wait 1, 2 and 4 ms (doubling); a run that then stays up past the healthy
// period before panicking starts the wait again at 1 ms instead of going on
// to 8 ms. Read from the supervisor's own "restarting it in" lines.
func TestSuperviseBackoffDoublesAndResetsAfterAHealthyRun(t *testing.T) {
	setSuperviseTimes(t, time.Millisecond, 8*time.Millisecond, 40*time.Millisecond)
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := log.New(lockedWriter{&mu, &buf}, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	done := make(chan struct{})
	Supervise(ctx, logger, "backoff-loop", func(context.Context) {
		switch runs.Add(1) {
		case 1, 2, 3:
			panic("quick")
		case 4:
			time.Sleep(60 * time.Millisecond) // healthy: past SuperviseHealthy
			panic("after a healthy run")
		default:
			close(done)
		}
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the loop did not reach its fifth run (ran %d times)", runs.Load())
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	var waits []string
	for _, line := range strings.Split(out, "\n") {
		if _, after, ok := strings.Cut(line, "supervise: backoff-loop panicked; restarting it in "); ok {
			waits = append(waits, after)
		}
	}
	if strings.Join(waits, ",") != "1ms,2ms,4ms,1ms" {
		t.Errorf("restart waits = %v; want [1ms 2ms 4ms 1ms] (doubling, then reset after the healthy run)", waits)
	}
}

// TestSuperviseStopsWhenItsContextEndsDuringTheBackoff: a loop panics and its
// supervisor waits (an hour, here) to restart it; cancelling the context
// during that wait ends the supervisor at once, with no second run.
func TestSuperviseStopsWhenItsContextEndsDuringTheBackoff(t *testing.T) {
	setSuperviseTimes(t, time.Hour, time.Hour, time.Hour)
	exited := make(chan string, 1)
	superviseExited = func(name string) { exited <- name }
	t.Cleanup(func() { superviseExited = nil })
	logger := log.New(io.Discard, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	panicked := make(chan struct{})
	Supervise(ctx, logger, "cancel-loop", func(context.Context) {
		if runs.Add(1) == 1 {
			close(panicked)
			panic("first run")
		}
	})
	<-panicked
	select {
	case name := <-exited:
		t.Fatalf("control: the supervisor of %s exited before its context ended", name)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the supervisor did not stop when its context ended during the backoff")
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("runs = %d; want 1 (no restart after the context ended)", got)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
