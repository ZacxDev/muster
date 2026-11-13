package api

import (
	"bytes"
	"context"
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
	SuperviseBackoff, SuperviseBackoffMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { SuperviseBackoff, SuperviseBackoffMax = 5*time.Second, 5*time.Minute })
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

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
