package asklog

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
)

// TestAFullQueueLogsAtMostOncePerMinute (round 1, green b): every dropped
// capture is counted, but the "queue is full" line is written at most once per
// DropLogEvery, saying how many were dropped since.
func TestAFullQueueLogsAtMostOncePerMinute(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	r := NewRecorder(log.New(&lockedWriter{w: &buf, mu: &mu}, "", 0))
	r.SetQueueBound(1)
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	r.SetClock(func() time.Time { clock.Lock(); defer clock.Unlock(); return now })
	write := metrics.AskCaptureErrors.WithLabelValues(KindAttention, StageWrite)
	w0 := testutil.ToFloat64(write)
	release := make(chan struct{})
	started := make(chan struct{})
	r.Do(context.Background(), KindAttention, func(context.Context) error { close(started); <-release; return nil })
	// Wait until the worker has taken the first capture off the queue and is
	// parked inside it. Otherwise it could take it later — after the burst but
	// before the Do after the clock advance — freeing the queue's one slot, so
	// that last capture would be queued instead of dropped and no second line
	// would be written. From here the burst's first capture fills the slot and
	// every later Do is dropped.
	<-started
	for i := 0; i < 51; i++ {
		r.Do(context.Background(), KindAttention, func(context.Context) error { return nil })
	}
	lines := func() int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(buf.String(), "capture queue is full")
	}
	if n := lines(); n != 1 {
		t.Fatalf("%d queue-full lines within a minute, want 1", n)
	}
	clock.Lock()
	now = now.Add(DropLogEvery)
	clock.Unlock()
	r.Do(context.Background(), KindAttention, func(context.Context) error { return nil })
	if n := lines(); n != 2 {
		t.Fatalf("%d queue-full lines after a minute, want 2", n)
	}
	if d := testutil.ToFloat64(write) - w0; d < 50 {
		t.Fatalf("dropped captures counted %v, want every one (at least 50)", d)
	}
	close(release)
	flush(t, r)
}

// TestTheDropsStillUnloggedAreSaidWhenTheQueueDrains (round 2): drops held
// back by the once-a-minute limit are logged, with their count, when the queue
// drains — so none goes unlogged.
func TestTheDropsStillUnloggedAreSaidWhenTheQueueDrains(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	r := NewRecorder(log.New(&lockedWriter{w: &buf, mu: &mu}, "", 0))
	r.SetQueueBound(1)
	r.SetClock(func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) })
	release := make(chan struct{})
	r.Do(context.Background(), KindAttention, func(context.Context) error { <-release; return nil })
	for i := 0; i < 20; i++ {
		r.Do(context.Background(), KindAttention, func(context.Context) error { return nil })
	}
	close(release)
	flush(t, r)
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if strings.Count(out, "capture queue is full") != 1 || !strings.Contains(out, "capture queue drained") {
		t.Fatalf("the log after a burst and a drain:\n%s", out)
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
