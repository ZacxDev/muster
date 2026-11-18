package mirror_test

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// WHAT AN OPEN'S FIRST BATCH COST, ON BOTH ENDS' SPANS (mirror/transcript.go,
// mirrorhost/transcript.go): its size as JSON on the host and as read by the
// hub (the same message, so the same number), its event count, the bytes the
// host's socket carried for it, and the link's round trip from the keepalive —
// what tells a slow open's latency from its bandwidth.

// attrsOf is span name's attributes, by key.
func attrsOf(spans tracetest.SpanStubs, name string) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	if s, ok := spanNamed(spans, name); ok {
		for _, a := range s.Attributes {
			out[a.Key] = a.Value
		}
	}
	return out
}

// bigTranscript is a transcript whose backfill is tens of KiB of prose:
// large enough that its size is unmistakable.
func bigTranscript(t *testing.T) *transcript {
	t.Helper()
	pad := strings.Repeat("the hub routes every batch to the watch that asked for it. ", 40)
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, promptLine("question "+pad), textLine("answer "+pad))
	}
	return newTranscript(t, lines...)
}

// watchTraced opens one traced watch of host's "main" and returns the spans
// once the host's have ended.
func watchTraced(t *testing.T, exp *tracetest.InMemoryExporter, r *rig, host string) tracetest.SpanStubs {
	t.Helper()
	exp.Reset()
	ctx, root := telemetry.Start(asUser(1), "request")
	ctx, cancel := context.WithCancel(ctx)
	first := make(chan struct{})
	done := make(chan error, 1)
	once := false
	go func() {
		done <- r.svc.Watch(ctx, host, "main", "", 0, func(mirror.Msg) error {
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
	var spans tracetest.SpanStubs
	eventually(t, "the host's watch span", func() bool {
		spans = exp.GetSpans()
		_, ok := spanNamed(spans, "host.watch")
		return ok
	})
	return spans
}

func TestAnOpensSpansCarryItsFirstBatchCostAndTheLinksRoundTrip(t *testing.T) {
	exp := traceRecorder(t)
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%3", PID: 42, Transcript: bigTranscript(t).path, Gen: "gen-z"})
	cl, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "ws", Backend: b, AllowHTTP: true,
		Refresh: 30 * time.Millisecond, ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = cl.RunOnce(ctx) }()
	r.online(t, 1, "ws", "main")
	eventually(t, "main is a Claude pane", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && h[0].Rendered })

	// The first keepalive ping is sent at registration; an open after its
	// pong carries the link's round trip.
	var spans tracetest.SpanStubs
	for i := 0; i < 50; i++ {
		spans = watchTraced(t, exp, r, "ws")
		if _, ok := attrsOf(spans, "mirror.watch")["mirror.host_rtt_ms"]; ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	server, host := attrsOf(spans, "mirror.watch"), attrsOf(spans, "host.watch")
	if rtt, ok := server["mirror.host_rtt_ms"]; !ok || rtt.AsFloat64() <= 0 {
		t.Errorf("the server's watch span has no link round trip: %v", server)
	}
	encoded, wire := host["mirror.first_batch_bytes"].AsInt64(), host["mirror.first_batch_wire_bytes"].AsInt64()
	if encoded < 10<<10 || server["mirror.first_batch_bytes"].AsInt64() != encoded {
		t.Fatalf("the first batch is %d bytes on the host and %d on the server; want the same, over 10 KiB",
			encoded, server["mirror.first_batch_bytes"].AsInt64())
	}
	if n := host["mirror.first_batch_events"].AsInt64(); n == 0 || server["mirror.first_batch_events"].AsInt64() != n {
		t.Errorf("first batch events: host %d, server %d", n, server["mirror.first_batch_events"].AsInt64())
	}
	// The wire counter: the socket carried all of the JSON and its framing,
	// and not a whole second message (an upper bound, tight on a quiet link).
	if wire < encoded || wire > encoded+1024 {
		t.Errorf("the socket carried %d bytes for %d of JSON", wire, encoded)
	}
	if _, ok := spanNamed(spans, "host.send"); !ok {
		t.Error("no host.send phase")
	}
}
