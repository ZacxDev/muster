package mirror_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// TRACING ACROSS THE TUNNEL (trace.go, mirrorhost's trace.go): one watch's
// open is ONE trace — the caller's span, the hub's watch and its round trip,
// and the host's spans for the same request — joined by the trace context the
// watch frame carries. And the field is optional both ways: an older host and
// an older muster each work exactly as before.

func traceRecorder(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restore := telemetry.Use(tp)
	t.Cleanup(func() { restore(); _ = tp.Shutdown(context.Background()) })
	return exp
}

func spanNamed(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

// TestOneOpenIsOneTraceFromServerToHost drives a real watch through a real
// hub and a real muster-mirror, and reads the spans both sides exported:
// every one of them is in the caller's trace, each the child of the one
// before it, and the host reported its phases on the first batch.
func TestOneOpenIsOneTraceFromServerToHost(t *testing.T) {
	exp := traceRecorder(t)
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	tr := newTranscript(t, promptLine("a question"), textLine("an answer"))
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%3", PID: 42, Transcript: tr.path, Gen: "gen-t"})
	r.connect(t, "ws", tok, b, false)
	r.online(t, 1, "ws", "main")
	eventually(t, "main is a Claude pane", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && h[0].Rendered })

	ctx, root := telemetry.Start(asUser(1), "request")
	var stats mirror.OpenStats
	ctx = mirror.WithOpenStats(ctx, &stats)
	first := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	once := false
	go func() {
		done <- r.svc.Watch(ctx, "ws", "main", "", 0, func(mirror.Msg) error {
			if !once {
				once = true
				close(first)
			}
			return nil
		})
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("no first batch")
	}
	cancel()
	<-done
	telemetry.End(root, "")

	// The host's phases arrived on the first batch, every one of them.
	if stats.Host == nil {
		t.Fatal("the first batch carried no host timing")
	}
	for _, p := range mirror.HostPhases {
		if _, ok := stats.Host.Phases[p]; !ok {
			t.Errorf("the host did not report phase %q (got %v)", p, stats.Host.Phases)
		}
	}
	if !stats.OffsetOK {
		t.Fatal("no clock offset estimate from the host's instants")
	}
	// Same process, same clock: the estimate must be within its own error
	// bound (half the round trip) of zero, plus a millisecond of rounding.
	if bound := stats.Delay/2 + time.Millisecond; stats.Offset > bound || stats.Offset < -bound {
		t.Errorf("clock offset %s on ONE clock, beyond the estimate's own bound ±%s", stats.Offset, bound)
	}
	t.Logf("one clock: offset %s, delay %s, round trip %s", stats.Offset, stats.Delay, stats.First.Sub(stats.Sent))

	// Every span closes in time for the read below except the host's, which
	// end on their own goroutine.
	var spans tracetest.SpanStubs
	eventually(t, "the host's spans", func() bool {
		spans = exp.GetSpans()
		_, ok := spanNamed(spans, "host.frame")
		return ok
	})
	if len(spans) < 8 {
		t.Fatalf("POSITIVE CONTROL: only %d span(s) exported", len(spans))
	}
	chain := []string{"request", "mirror.watch", "mirror.watch.first_batch", "host.watch"}
	for i, name := range chain {
		s, ok := spanNamed(spans, name)
		if !ok {
			t.Fatalf("no %q span among %d", name, len(spans))
		}
		if s.SpanContext.TraceID() != root.SpanContext().TraceID() {
			t.Errorf("%q is in trace %s, not the request's %s", name, s.SpanContext.TraceID(), root.SpanContext().TraceID())
		}
		if i == 0 {
			continue
		}
		parent, _ := spanNamed(spans, chain[i-1])
		if s.Parent.SpanID() != parent.SpanContext.SpanID() {
			t.Errorf("%q's parent is %s, want %q (%s)", name, s.Parent.SpanID(), chain[i-1], parent.SpanContext.SpanID())
		}
	}
	hostWatch, _ := spanNamed(spans, "host.watch")
	for _, p := range mirror.HostPhases {
		s, ok := spanNamed(spans, "host."+p)
		if !ok {
			t.Errorf("no host.%s span", p)
			continue
		}
		if s.Parent.SpanID() != hostWatch.SpanContext.SpanID() {
			t.Errorf("host.%s is not host.watch's child", p)
		}
	}
}

// TestAnOlderHostIgnoresTheTraceContext: the two mixed-version directions.
//
//   - A NEW muster and an OLDER host: the watch frame carries a traceparent
//     the host never reads (a raw host here answers as one that predates the
//     field: no timing), and the view works — the batch arrives, the round
//     trip is measured, the host's half is simply absent.
//   - An OLDER muster and a NEW host: a watch with no traceparent is served
//     as before, the host starts no span of its own (no trace to join), and
//     its reply's timing is a field an older muster ignores.
func TestAnOlderHostIgnoresTheTraceContext(t *testing.T) {
	exp := traceRecorder(t)

	t.Run("new muster, older host", func(t *testing.T) {
		r := newRig(t)
		tok, _ := r.store.mint(t, 1, "old")
		h, err := r.rawConnect(t, tok, mirror.Msg{Type: mirror.TypeHello, Version: 2, Host: "old", Sessions: []string{"main"}, Claude: []string{"main"}})
		if err != nil {
			t.Fatal(err)
		}
		// The welcome is written before the host is registered (ServeConnect):
		// a watch before that answers ErrHostOffline and sends no frame.
		r.online(t, 1, "old", "main")
		ctx, root := telemetry.Start(asUser(1), "request")
		var stats mirror.OpenStats
		ctx, cancel := context.WithCancel(mirror.WithOpenStats(ctx, &stats))
		defer cancel()
		got := make(chan mirror.Msg, 1)
		go func() {
			_ = r.svc.Watch(ctx, "old", "main", "", 0, func(m mirror.Msg) error { got <- m; return nil })
		}()
		var watch mirror.Msg
		eventually(t, "the watch frame", func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, m := range h.text {
				if m.Type == mirror.TypeWatch {
					watch = m
					return true
				}
			}
			return false
		})
		if !telemetry.ValidTraceparent(watch.Traceparent) {
			t.Fatalf("POSITIVE CONTROL: the watch carried no trace context (%q)", watch.Traceparent)
		}
		// What an older host does with that frame: decode it into the shape
		// it knows (here, the field set it had) and answer as it always did.
		var old struct {
			Type string `json:"type"`
			ID   uint32 `json:"id"`
		}
		raw, _ := json.Marshal(watch)
		if err := json.Unmarshal(raw, &old); err != nil || old.Type != mirror.TypeWatch || old.ID != watch.ID {
			t.Fatalf("an older decoder could not read the frame: %v %+v", err, old)
		}
		reply, _ := json.Marshal(map[string]any{"type": mirror.TypeEvents, "id": old.ID, "gen": "g", "offset": 10, "reset": true})
		if err := h.ws.Write(context.Background(), websocket.MessageText, reply); err != nil {
			t.Fatal(err)
		}
		select {
		case m := <-got:
			if !m.Reset || m.Offset != 10 {
				t.Fatalf("the batch arrived as %+v", m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an older host's batch never reached the view")
		}
		if stats.First.IsZero() || stats.Host != nil || stats.OffsetOK {
			t.Errorf("stats = %+v: want a round trip, and no host half", stats)
		}
		cancel()
		telemetry.End(root, "")
	})

	t.Run("older muster, new host", func(t *testing.T) {
		exp.Reset()
		tr := newTranscript(t, promptLine("q"), textLine("a"))
		b := newClaudeBackend("main")
		b.setPane("main", mirrorhost.ClaudePane{Pane: "%1", Transcript: tr.path, Gen: "g1"})
		// An older muster: its watch carries no traceparent.
		reply := driveV2(t, false, b, mirror.Msg{Type: mirror.TypeWatch, Session: "main"})
		if reply.Type != mirror.TypeEvents || !reply.Reset {
			t.Fatalf("the host answered %+v, want its backfill", reply)
		}
		if reply.Timing == nil || len(reply.Timing.Phases) == 0 {
			t.Error("the host's first batch carried no timing (an older muster ignores it; this one reads it)")
		}
		time.Sleep(50 * time.Millisecond)
		for _, s := range exp.GetSpans() {
			if strings.HasPrefix(s.Name, "host.") {
				t.Errorf("the host started %q with no trace to join", s.Name)
			}
		}
	})
}

// TestClockOffsetIsNTPs: the estimate from a known skew and known legs.
func TestClockOffsetIsNTPs(t *testing.T) {
	t0 := time.Unix(1000, 0)
	// The host's clock is 2s ahead; each leg takes 30ms; the host works 100ms.
	recv := t0.Add(30*time.Millisecond + 2*time.Second)
	sent := recv.Add(100 * time.Millisecond)
	t3 := t0.Add(160 * time.Millisecond)
	off, delay, ok := mirror.ClockOffset(t0, t3, &mirror.HostTiming{Recv: recv.UnixMicro(), Sent: sent.UnixMicro()})
	if !ok || off != 2*time.Second || delay != 60*time.Millisecond {
		t.Fatalf("offset %s delay %s ok %v, want 2s 60ms", off, delay, ok)
	}
	if _, _, ok := mirror.ClockOffset(t0, t3, &mirror.HostTiming{Recv: 1}); ok {
		t.Fatal("an estimate from one host instant")
	}
}

// TestReasonSpeaksOnlyItsVocabulary: every error the plugin names maps to a
// word in Reasons (a metric label), and an unknown error is "error".
func TestReasonSpeaksOnlyItsVocabulary(t *testing.T) {
	known := map[string]bool{}
	for _, r := range mirror.Reasons {
		known[r] = true
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, mirror.ErrHostDisconnected, mirror.ErrHostOffline,
		mirror.ErrSessionGone, mirror.ErrWindowGone, mirror.ErrNotClaude, mirror.ErrNoRenderedView, mirror.ErrTooManyWatches,
		mirror.ErrViewOverrun, mirror.ErrInteractiveRefused, mirror.ErrHostEnded, mirror.ErrPasteRefused, io.EOF} {
		if r := mirror.Reason(err); !known[r] {
			t.Errorf("Reason(%v) = %q, which is not in Reasons", err, r)
		}
	}
	if mirror.Reason(mirror.ErrHostDisconnected) != "host_disconnected" || mirror.Reason(io.EOF) != telemetry.ReasonError {
		t.Error("a reason moved")
	}
}
