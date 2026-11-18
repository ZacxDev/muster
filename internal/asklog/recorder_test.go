package asklog

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
)

func flush(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// TestTheRecorderRunsCapturesInOrderOffTheCallersGoroutine: Do returns at once
// even while a capture is blocked, and the captures run in the order queued
// (an ask's open before its resolution).
func TestTheRecorderRunsCapturesInOrderOffTheCallersGoroutine(t *testing.T) {
	r := NewRecorder(log.New(io.Discard, "", 0))
	release := make(chan struct{})
	var mu sync.Mutex
	var order []int
	queued := make(chan struct{})
	go func() {
		r.Do(context.Background(), KindPlainText, func(context.Context) error { <-release; return nil })
		for i := 1; i <= 50; i++ {
			r.Do(context.Background(), KindPlainText, func(context.Context) error {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				return nil
			})
		}
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		close(release) // let a caller stuck behind the capture go, then fail
		t.Fatal("Do blocked behind a stuck capture: it ran on the caller's goroutine")
	}
	close(release)
	flush(t, r)
	if len(order) != 50 {
		t.Fatalf("%d captures ran, want 50", len(order))
	}
	for i, v := range order {
		if v != i+1 {
			t.Fatalf("capture %d ran at position %d: %v", v, i, order)
		}
	}
}

// TestACaptureFailureIsCountedByStageAndTheNextOneRuns: an error is counted
// under its stage (an unmarked one as write), a panic is recovered and counted,
// and the captures after them still run.
func TestACaptureFailureIsCountedByStageAndTheNextOneRuns(t *testing.T) {
	r := NewRecorder(log.New(io.Discard, "", 0))
	read := metrics.AskCaptureErrors.WithLabelValues(KindAskUserQuestion, StageRead)
	write := metrics.AskCaptureErrors.WithLabelValues(KindAskUserQuestion, StageWrite)
	classify := metrics.AskCaptureErrors.WithLabelValues(KindAskUserQuestion, StageClassify)
	r0, w0, c0 := testutil.ToFloat64(read), testutil.ToFloat64(write), testutil.ToFloat64(classify)
	ran := false
	r.Do(context.Background(), KindAskUserQuestion, func(context.Context) error { return AtStage(StageRead, errors.New("host gone")) })
	r.Do(context.Background(), KindAskUserQuestion, func(context.Context) error { return errors.New("db down") })
	r.Do(context.Background(), KindAskUserQuestion, func(context.Context) error { return AtStage(StageClassify, errors.New("timeout")) })
	r.Do(context.Background(), KindAskUserQuestion, func(context.Context) error { panic("boom") })
	r.Do(context.Background(), KindAskUserQuestion, func(context.Context) error { ran = true; return nil })
	flush(t, r)
	if d := testutil.ToFloat64(read) - r0; d != 1 {
		t.Errorf("read failures counted %v, want 1", d)
	}
	if d := testutil.ToFloat64(write) - w0; d != 2 { // the unmarked error and the panic
		t.Errorf("write failures counted %v, want 2", d)
	}
	if d := testutil.ToFloat64(classify) - c0; d != 1 {
		t.Errorf("classify failures counted %v, want 1", d)
	}
	if !ran {
		t.Fatal("the capture after the failures did not run")
	}
}

// TestAFullQueueDropsTheCaptureAndCountsIt: past its bound a capture is
// dropped at once — counted as a write failure — and never blocks the caller.
func TestAFullQueueDropsTheCaptureAndCountsIt(t *testing.T) {
	r := NewRecorder(log.New(io.Discard, "", 0))
	r.SetQueueBound(2)
	write := metrics.AskCaptureErrors.WithLabelValues(KindOpenCodeQuestion, StageWrite)
	w0 := testutil.ToFloat64(write)
	release := make(chan struct{})
	r.Do(context.Background(), KindOpenCodeQuestion, func(context.Context) error { <-release; return nil })
	// The worker may or may not have taken the first one off the queue yet;
	// four more always overflow a bound of two.
	for i := 0; i < 4; i++ {
		r.Do(context.Background(), KindOpenCodeQuestion, func(context.Context) error { return nil })
	}
	if d := testutil.ToFloat64(write) - w0; d < 2 {
		t.Fatalf("dropped captures counted %v, want at least 2", d)
	}
	close(release)
	flush(t, r)
}

// TestACaptureOutlivesItsCallersContext: the caller's request ending (its
// context cancelled) does not cancel the capture it queued.
func TestACaptureOutlivesItsCallersContext(t *testing.T) {
	r := NewRecorder(log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	var got error
	r.Do(ctx, KindAttention, func(c context.Context) error { time.Sleep(20 * time.Millisecond); got = c.Err(); return nil })
	cancel()
	flush(t, r)
	if got != nil {
		t.Fatalf("the capture saw its caller's cancellation: %v", got)
	}
}
