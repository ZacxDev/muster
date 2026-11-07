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
	r.Do(context.Background(), KindAttention, func(context.Context) error { <-release; return nil })
	// The worker may or may not have taken the first off the queue yet; 51
	// more always overflow a bound of one at least 49 times.
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

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
