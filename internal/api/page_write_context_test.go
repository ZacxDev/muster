package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

// withPageWriteTimes shrinks the page-write bounds for one test.
func withPageWriteTimes(t *testing.T, timeout, grace time.Duration) {
	t.Helper()
	oldTimeout, oldGrace := terminalWriteTimeout, pageWriteGrace
	terminalWriteTimeout, pageWriteGrace = timeout, grace
	t.Cleanup(func() { terminalWriteTimeout, pageWriteGrace = oldTimeout, oldGrace })
}

// endsWithin waits for ctx to end and reports how long that took, failing the
// test if it takes longer than limit.
func endsWithin(t *testing.T, ctx context.Context, limit time.Duration, what string) time.Duration {
	t.Helper()
	start := time.Now()
	select {
	case <-ctx.Done():
		return time.Since(start)
	case <-time.After(limit):
		t.Fatalf("%s: the write context had not ended after %v", what, limit)
		return 0
	}
}

// TestAPageWriteOutlivesItsStreamByTheGraceOnly: once the stream's context is
// CANCELLED (a shutdown), a page write's context stays live — a write in
// flight then, or started after, can land its frame instead of
// coder/websocket cutting the socket — and ends within the grace.
func TestAPageWriteOutlivesItsStreamByTheGraceOnly(t *testing.T) {
	const grace = 300 * time.Millisecond
	withPageWriteTimes(t, time.Hour, grace)

	stream, end := context.WithCancel(context.Background())
	wctx, cancel := pageWriteContext(stream)
	defer cancel()
	end()
	// AfterFunc runs on its own goroutine; give it time to (wrongly) end wctx.
	time.Sleep(grace / 3)
	if err := wctx.Err(); err != nil {
		t.Fatalf("the write context ended with its stream (%v); a write in flight would cut the socket", err)
	}
	took := endsWithin(t, wctx, grace+500*time.Millisecond, "after the stream was cancelled")
	if took+grace/3 < grace*8/10 {
		t.Errorf("the write context ended %v after the stream, well before the grace %v", took+grace/3, grace)
	}

	// A write started AFTER the stream was cancelled gets the grace too.
	late, lcancel := pageWriteContext(stream)
	defer lcancel()
	if late.Err() != nil {
		t.Fatalf("a write context made after the stream ended is already over: %v", late.Err())
	}
	endsWithin(t, late, grace+500*time.Millisecond, "a write started after the stream ended")
}

// TestAPageWriteIsBoundedByTheWriteTimeout: with the stream alive, a page
// write's context ends at terminalWriteTimeout (a stalled page), as a deadline.
func TestAPageWriteIsBoundedByTheWriteTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	withPageWriteTimes(t, timeout, time.Hour)
	wctx, cancel := pageWriteContext(context.Background())
	defer cancel()
	endsWithin(t, wctx, timeout+500*time.Millisecond, "a live stream")
	if !errors.Is(wctx.Err(), context.DeadlineExceeded) {
		t.Errorf("the write timeout surfaced as %v, want context.DeadlineExceeded", wctx.Err())
	}
}

// TestAPageWriteHonoursTheCallersDeadline: a caller's own deadline earlier than
// the write timeout (shutdownFrame's one second) is kept — no grace past it —
// and still reads as DeadlineExceeded, not as a cancellation.
func TestAPageWriteHonoursTheCallersDeadline(t *testing.T) {
	const deadline = 150 * time.Millisecond
	withPageWriteTimes(t, time.Hour, time.Hour)
	caller, ccancel := context.WithTimeout(context.Background(), deadline)
	defer ccancel()
	wctx, cancel := pageWriteContext(caller)
	defer cancel()
	took := endsWithin(t, wctx, deadline+500*time.Millisecond, "a caller with a deadline")
	if took > deadline+300*time.Millisecond {
		t.Errorf("the write context outlived the caller's %v deadline by %v", deadline, took-deadline)
	}
	if !errors.Is(wctx.Err(), context.DeadlineExceeded) {
		t.Errorf("the caller's deadline surfaced as %v, want context.DeadlineExceeded", wctx.Err())
	}
}
