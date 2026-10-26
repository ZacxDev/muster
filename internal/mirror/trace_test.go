package mirror_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// hostPhases is every phase span a Claude Code watch's open has on the host,
// in order (mirrorhost's tail).
var hostPhases = []string{"resolve", "open", "backfill", "redact", "frame", "send"}

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

	if stats.Sent.IsZero() || stats.First.Before(stats.Sent) {
		t.Fatalf("the hub measured no round trip: %+v", stats)
	}

	// Every span closes in time for the read below except the host's, which
	// end on their own goroutine — and host.watch LAST: its final phase,
	// host.send, is the first batch's write, which returns on the host only
	// after muster may already have read the batch. So the wait is for
	// host.watch itself to have ENDED (an exported span is an ended one),
	// never for an earlier phase.
	var spans tracetest.SpanStubs
	eventually(t, "the host's watch span to end", func() bool {
		spans = exp.GetSpans()
		_, ok := spanNamed(spans, "host.watch")
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
	for _, p := range hostPhases {
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
//     the host never reads (a raw host here decodes it as one that predates
//     the field), and the view works — the batch arrives, the round trip is
//     measured, the host's spans are simply absent.
//   - An OLDER muster and a NEW host: a watch with no traceparent is served
//     as before, and the host starts no span of its own (no trace to join):
//     its reply is exactly what an older muster always read.
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
		if stats.First.IsZero() {
			t.Errorf("stats = %+v: want a round trip", stats)
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
		time.Sleep(50 * time.Millisecond)
		for _, s := range exp.GetSpans() {
			if strings.HasPrefix(s.Name, "host.") {
				t.Errorf("the host started %q with no trace to join", s.Name)
			}
		}
	})
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

// TestADuplicateStreamIDEndsBothOpens: a second watch with an id already open
// is refused — and its open span ends with the refusal, while the FIRST
// open's span is still the one its first batch ends. (A duplicate that
// replaced the registered open orphaned the first span: nothing ended it.)
func TestADuplicateStreamIDEndsBothOpens(t *testing.T) {
	exp := traceRecorder(t)
	tr := newTranscript(t, promptLine("q"), textLine("a"))
	inner := newClaudeBackend("main")
	inner.setPane("main", mirrorhost.ClaudePane{Pane: "%1", Transcript: tr.path, Gen: "g1"})
	// The first watch's pane resolution is HELD until the duplicate has been
	// refused: the first open is still unreplied when the duplicate arrives —
	// the overlap the bug needs, made deterministic.
	b := &gatedClaude{claudeBackend: inner, release: make(chan struct{})}
	const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	replies := make(chan mirror.Msg, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		if _, _, err := ws.Read(ctx); err != nil {
			return
		}
		write := func(m mirror.Msg) { bb, _ := json.Marshal(m); _ = ws.Write(ctx, websocket.MessageText, bb) }
		write(mirror.Msg{Type: mirror.TypeWelcome, Version: 2, Host: "laptop"})
		b.hold.Store(true)
		watch := mirror.Msg{Type: mirror.TypeWatch, ID: 9, Session: "main", Traceparent: tp}
		write(watch)
		write(watch)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.ID == 9 {
				replies <- m
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := mirrorhost.New(mirrorhost.Config{URL: srv.URL, Token: mirror.TokenPrefix + "x", Host: "laptop",
		Backend: b, AllowHTTP: true, ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	refused, batched := false, false
	deadline := time.After(5 * time.Second)
	for !refused || !batched {
		select {
		case m := <-replies:
			if m.Type == mirror.TypeClosed && m.Error != "" && !refused {
				refused = true
				close(b.release)
			}
			batched = batched || m.Type == mirror.TypeEvents
		case <-deadline:
			t.Fatalf("refused %v, batched %v: the host did not answer both watches", refused, batched)
		}
	}
	// Both spans end NOW — the first at its first batch, the duplicate at its
	// refusal — not when the connection ends (the server above hangs up at
	// 4 s, which would end an orphaned span too, late and wrongly).
	var watches []tracetest.SpanStub
	until := time.Now().Add(1500 * time.Millisecond)
	for {
		watches = nil
		for _, s := range exp.GetSpans() {
			if s.Name == "host.watch" {
				watches = append(watches, s)
			}
		}
		if len(watches) == 2 || time.Now().After(until) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(watches) != 2 {
		t.Fatalf("%d host.watch span(s) ended with the first batch sent and the duplicate refused, want 2", len(watches))
	}
	reasons := map[string]bool{}
	for _, s := range watches {
		for _, a := range s.Attributes {
			if a.Key == "muster.reason" {
				reasons[a.Value.AsString()] = true
			}
		}
	}
	if !reasons[telemetry.ReasonOK] || len(reasons) != 2 {
		t.Errorf("the two opens ended with %v: want the first ok and the duplicate refused", reasons)
	}
}

// gatedClaude is a claudeBackend whose pane resolution, once hold is set,
// waits for release.
type gatedClaude struct {
	*claudeBackend
	hold    atomic.Bool
	release chan struct{}
}

func (g *gatedClaude) ClaudePanes(ctx context.Context, sessions []string) map[string]mirrorhost.ClaudePane {
	if g.hold.Load() {
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
	return g.claudeBackend.ClaudePanes(ctx, sessions)
}
