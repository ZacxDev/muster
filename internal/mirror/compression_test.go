package mirror_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// THE TUNNEL IS COMPRESSED ONLY WHEN BOTH ENDS SAY SO (mirrorhost/tunnel.go,
// Service.ServeConnect): permessage-deflate is negotiated in the handshake, so
// each mixed-version pairing gets the plain tunnel it always had, and only a
// new host on a new muster deflates. Each case drives a real watch and reads
// what both ends' spans say the first batch cost: its size as JSON, its size
// on the host's socket, and whether the link was compressed.

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
// enough for deflate to show.
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

func TestTheTunnelIsCompressedOnlyWhenBothEndsSaySo(t *testing.T) {
	exp := traceRecorder(t)
	for _, c := range []struct {
		name               string
		hostOff, serverOff bool
		want               bool
	}{
		{"new host, new muster: compressed", false, false, true},
		{"older host (offers nothing), new muster: plain", true, false, false},
		{"new host, muster refusing it: plain", false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.svc.SetTunnelCompression(!c.serverOff)
			tok, _ := r.store.mint(t, 1, "ws")
			b := newClaudeBackend("main")
			b.setPane("main", mirrorhost.ClaudePane{Pane: "%3", PID: 42, Transcript: bigTranscript(t).path, Gen: "gen-z"})
			cl, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "ws", Backend: b, AllowHTTP: true,
				NoCompression: c.hostOff, Refresh: 30 * time.Millisecond, ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go func() { _ = cl.RunOnce(ctx) }()
			r.online(t, 1, "ws", "main")
			eventually(t, "main is a Claude pane", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && h[0].Rendered })

			// The first keepalive ping is sent at registration; an open after
			// its pong carries the link's round trip.
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
			if got := server["mirror.tunnel_compressed"].AsBool(); got != c.want {
				t.Errorf("server: tunnel_compressed = %v, want %v", got, c.want)
			}
			if got := host["mirror.tunnel_compressed"].AsBool(); got != c.want {
				t.Errorf("host: tunnel_compressed = %v, want %v", got, c.want)
			}
			encoded, wire := host["mirror.first_batch_bytes"].AsInt64(), host["mirror.first_batch_wire_bytes"].AsInt64()
			// Both ends measured the same message: the server reads it inflated.
			if encoded < 10<<10 || server["mirror.first_batch_bytes"].AsInt64() != encoded {
				t.Fatalf("POSITIVE CONTROL: the first batch is %d bytes on the host and %d on the server; want the same, over 10 KiB",
					encoded, server["mirror.first_batch_bytes"].AsInt64())
			}
			if n := host["mirror.first_batch_events"].AsInt64(); n == 0 || server["mirror.first_batch_events"].AsInt64() != n {
				t.Errorf("first batch events: host %d, server %d", n, server["mirror.first_batch_events"].AsInt64())
			}
			// The wire counter, both ways: deflated, the socket carried a
			// fraction of the JSON; plain, at least all of it (plus framing).
			if c.want && wire*2 > encoded {
				t.Errorf("compressed, yet the socket carried %d bytes for %d of JSON", wire, encoded)
			}
			if !c.want && wire < encoded {
				t.Errorf("uncompressed, yet the socket carried %d bytes for %d of JSON: the counter is wrong", wire, encoded)
			}
			t.Logf("first batch: %d bytes JSON, %d on the wire, %d events", encoded, wire, host["mirror.first_batch_events"].AsInt64())
		})
	}
}

// TestANewHostOnAMusterThatPredatesCompression: the muster side here is the
// code an older muster ran — websocket.Accept with no options — playing the
// tunnel by hand. The new host offers deflate, is answered without it, and
// its batch arrives as the plain JSON an older muster reads.
func TestANewHostOnAMusterThatPredatesCompression(t *testing.T) {
	exp := traceRecorder(t)
	tr := bigTranscript(t)
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%1", Transcript: tr.path, Gen: "g1"})
	offered := make(chan string, 1)
	got := make(chan mirror.Msg, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offered <- r.Header.Get("Sec-WebSocket-Extensions")
		ws, err := websocket.Accept(w, r, nil) // an older muster's Accept
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(mirror.MaxFrame)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := ws.Read(ctx); err != nil {
			return
		}
		write := func(m mirror.Msg) { bb, _ := json.Marshal(m); _ = ws.Write(ctx, websocket.MessageText, bb) }
		write(mirror.Msg{Type: mirror.TypeWelcome, Version: 2, Host: "laptop"})
		write(mirror.Msg{Type: mirror.TypeWatch, ID: 3, Session: "main", Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.Type == mirror.TypeEvents {
				got <- m
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := mirrorhost.New(mirrorhost.Config{URL: srv.URL, Token: mirror.TokenPrefix + "x", Host: "laptop", Backend: b, AllowHTTP: true,
		ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	if ext := <-offered; !strings.Contains(ext, "permessage-deflate") {
		t.Fatalf("POSITIVE CONTROL: the new host offered no compression (%q)", ext)
	}
	select {
	case m := <-got:
		if !m.Reset || len(m.Events) == 0 {
			t.Fatalf("the batch arrived as %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an older muster never got the new host's batch")
	}
	var host map[attribute.Key]attribute.Value
	eventually(t, "the host's watch span", func() bool {
		host = attrsOf(exp.GetSpans(), "host.watch")
		_, ok := host["mirror.tunnel_compressed"]
		return ok
	})
	if host["mirror.tunnel_compressed"].AsBool() {
		t.Error("the host believes a muster that answered without the extension is compressing")
	}
}
