package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

// ---------------------------------------------------------------------------
// OPENING A MIRRORED VIEW, TRACED END TO END through the real mux, a real
// mirror hub and a REAL muster-mirror client over a real tunnel: only tmux is
// played (sentinelBackend). These are the tests docs/telemetry.md's claims
// rest on — one trace per open, nothing private in it, the relay's doors.
// ---------------------------------------------------------------------------

// sentinel is written into every fixture that carries private content — the
// transcript's prompt, reply, tool call and tool output, the session's
// display label — and into the session NAME too, so the test is stricter than
// the rule (a name is an id, which a span MAY carry; muster's spans carry none).
const sentinel = "SENTINEL7c1e"

// tokenStore is the plugin's token store with one token, for one owner.
type tokenStore struct {
	token string
	rec   mirror.Token
}

func (s *tokenStore) Authenticate(_ context.Context, token string) (mirror.Token, error) {
	if token != s.token {
		return mirror.Token{}, mirror.ErrBadToken
	}
	return s.rec, nil
}
func (s *tokenStore) TokenLive(context.Context, int64) (bool, error) { return true, nil }
func (s *tokenStore) CreateToken(context.Context, string) (string, mirror.Token, error) {
	return "", mirror.Token{}, errors.New("not here")
}
func (s *tokenStore) ListTokens(context.Context) ([]mirror.Token, error) {
	return []mirror.Token{s.rec}, nil
}
func (s *tokenStore) RevokeToken(context.Context, int64) (bool, error) { return false, nil }

// sentinelBackend plays tmux for one Claude Code session.
type sentinelBackend struct {
	session string
	pane    mirrorhost.ClaudePane
}

func (b *sentinelBackend) List(context.Context) ([]string, error) { return []string{b.session}, nil }
func (b *sentinelBackend) Attach(context.Context, string, bool, uint16, uint16) (mirrorhost.Terminal, error) {
	return nil, errors.New("no terminal in this test")
}
func (b *sentinelBackend) Paste(context.Context, mirrorhost.ClaudePane, string) error {
	return errors.New("no paste in this test")
}
func (b *sentinelBackend) ClaudePanes(_ context.Context, sessions []string) map[string]mirrorhost.ClaudePane {
	out := map[string]mirrorhost.ClaudePane{}
	for _, s := range sessions {
		if s == b.session {
			out[s] = b.pane
		}
	}
	return out
}

// tracedMirrorRig is a server with tracing on (an in-memory exporter), the
// real plugin, and a real host connected to it, serving one Claude Code
// session whose transcript, label and name all carry the sentinel.
type tracedMirrorRig struct {
	s       *Server
	ts      *httptest.Server
	exp     *tracetest.InMemoryExporter
	host    string
	session string
}

func newTracedMirrorRig(t *testing.T) *tracedMirrorRig {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restore := telemetry.Use(tp)
	t.Cleanup(func() { restore(); _ = tp.Shutdown(context.Background()) })

	tok, err := mirror.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	const host = "tracehost"
	svc := mirror.NewService(&tokenStore{token: tok, rec: mirror.Token{ID: 1, OwnerID: 1, Host: host, CreatedAt: time.Now()}}, log.New(io.Discard, "", 0))
	// Built AFTER tracing is on: the handler's server spans are decided here.
	s, ts := mirrorServer(t, svc, nil)

	session := "work-" + sentinel
	lines := []map[string]any{
		{"type": "user", "promptId": "p", "message": map[string]any{"content": "please read " + sentinel + " now"}},
		{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash",
			"input": map[string]any{"command": "cat /tmp/" + sentinel}}}}},
		{"type": "user", "promptId": "p", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1",
			"content": "the file says " + sentinel, "is_error": false}}}},
		{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "it says " + sentinel}}}},
	}
	var body bytes.Buffer
	for _, l := range lines {
		b, _ := json.Marshal(l)
		body.Write(append(b, '\n'))
	}
	path := filepath.Join(t.TempDir(), "11111111-aaaa.jsonl")
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &sentinelBackend{session: session, pane: mirrorhost.ClaudePane{Pane: "%3", PID: 42, Transcript: path, Gen: "gen-" + sentinel}}
	c, err := mirrorhost.New(mirrorhost.Config{URL: ts.URL, Token: tok, Host: host, Backend: backend, AllowHTTP: true,
		Codenames: map[string]string{session: "label-" + sentinel},
		Refresh:   30 * time.Millisecond, ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.RunOnce(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	rendered := func() bool {
		for _, h := range svc.Hub().Hosts(1) {
			for _, c := range h.Claude {
				if c == session && h.Rendered {
					return true
				}
			}
		}
		return false
	}
	for !rendered() {
		if time.Now().After(deadline) {
			t.Fatal("the host never came online")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &tracedMirrorRig{s: s, ts: ts, exp: exp, host: host, session: session}
}

// open opens the rendered view as the browser does: the document, then its
// socket carrying the document's traceparent (as the page's own open span
// would: the page parents its span on the document's). It returns the
// document's traceparent and the socket's first batch.
func (r *tracedMirrorRig) open(t *testing.T) (docTP string, firstBatch string) {
	t.Helper()
	q := url.Values{"host": {r.host}, "session": {r.session}}
	resp, body := mirrorGet(t, r.s, r.ts, "/mirror/view?"+q.Encode())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the view page answered %d: %.300s", resp.StatusCode, body)
	}
	for _, st := range resp.Header.Values("Server-Timing") {
		if v, ok := strings.CutPrefix(st, `traceparent;desc="`); ok {
			docTP = strings.TrimSuffix(v, `"`)
		}
	}
	if !telemetry.ValidTraceparent(docTP) {
		t.Fatalf("the document's Server-Timing carries no traceparent: %q", resp.Header.Values("Server-Timing"))
	}
	q.Set("tp", docTP)
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(r.s))
	h.Set("Origin", r.ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.ts.URL, "http")+"/mirror/view/ws?"+q.Encode(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	return docTP, string(data)
}

// spansOf waits for the open's spans — the host's end on their own goroutine,
// the socket's when its handler returns — and returns them.
func (r *tracedMirrorRig) spans(t *testing.T) tracetest.SpanStubs {
	t.Helper()
	var spans tracetest.SpanStubs
	deadline := time.Now().Add(5 * time.Second)
	for {
		spans = r.exp.GetSpans()
		names := map[string]bool{}
		for _, s := range spans {
			names[s.Name] = true
		}
		if names["host.frame"] && names["GET /mirror/view/ws"] && names["mirror.watch"] {
			return spans
		}
		if time.Now().After(deadline) {
			t.Fatalf("the open's spans never all ended: %v", names)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stub(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

// TestOneOpenIsOneTraceFromTheDocumentToTheHost: the document's server span,
// the socket's (joined through ?tp=), the hub's watch and round trip, and the
// host's own spans are ONE trace, each the child of the one before.
func TestOneOpenIsOneTraceFromTheDocumentToTheHost(t *testing.T) {
	r := newTracedMirrorRig(t)
	docTP, batch := r.open(t)
	if !strings.Contains(batch, `"type":"batch"`) {
		t.Fatalf("the socket's first message is not a batch: %.200s", batch)
	}
	spans := r.spans(t)
	chain := []string{"GET /mirror/view", "GET /mirror/view/ws", "mirror.watch", "mirror.watch.first_batch", "host.watch", "host.backfill"}
	doc, _ := stub(spans, chain[0])
	if telemetry.SpanTraceparent(doc.SpanContext) != docTP {
		t.Fatalf("the Server-Timing traceparent %s is not the document span's %s", docTP, telemetry.SpanTraceparent(doc.SpanContext))
	}
	for i, name := range chain {
		s, ok := stub(spans, name)
		if !ok {
			t.Fatalf("no %q span", name)
		}
		if s.SpanContext.TraceID() != doc.SpanContext.TraceID() {
			t.Errorf("%q is in trace %s, not the document's %s", name, s.SpanContext.TraceID(), doc.SpanContext.TraceID())
		}
		if i == 0 {
			continue
		}
		parent, _ := stub(spans, chain[i-1])
		if s.Parent.SpanID() != parent.SpanContext.SpanID() {
			t.Errorf("%q's parent is %s, want %q", name, s.Parent.SpanID(), chain[i-1])
		}
	}
	// The socket ended because the browser left, and its span says so.
	ws, _ := stub(spans, "GET /mirror/view/ws")
	reason := ""
	for _, a := range ws.Attributes {
		if a.Key == "muster.close_reason" {
			reason = a.Value.AsString()
		}
	}
	if reason != telemetry.ReasonClientLeft {
		t.Errorf("the socket's close reason is %q, want %q", reason, telemetry.ReasonClientLeft)
	}
}

// TestNoExportedSpanCarriesPrivateContent drives the open with every private
// fixture holding the sentinel, and reads EVERY exported span: its name, its
// attributes, its events and their attributes, its status, its links and its
// resource. The sentinel is in none of them.
//
// 🔴 THE POSITIVE CONTROLS: the sentinel DID reach the browser (the batch
// carries it — so the fixture flowed through every span's request), and the
// exporter DID receive the open's spans from all three layers (server, hub,
// host) — a sentinel-free export of nothing would otherwise pass.
func TestNoExportedSpanCarriesPrivateContent(t *testing.T) {
	r := newTracedMirrorRig(t)
	_, batch := r.open(t)
	if !strings.Contains(batch, sentinel) {
		t.Fatalf("POSITIVE CONTROL: the batch the browser got does not carry the sentinel; the fixture never flowed: %.300s", batch)
	}
	spans := r.spans(t)
	layers := map[string]bool{}
	for _, s := range spans {
		layers[strings.SplitN(s.Name, ".", 2)[0]] = true
	}
	for _, l := range []string{"GET /mirror/view", "mirror", "host", "open"} {
		if !layers[l] {
			t.Errorf("POSITIVE CONTROL: no %q span was exported (layers: %v)", l, layers)
		}
	}
	if len(spans) < 15 {
		t.Errorf("POSITIVE CONTROL: only %d span(s) exported", len(spans))
	}
	for _, s := range spans {
		var parts []string
		parts = append(parts, s.Name, s.Status.Description)
		for _, a := range s.Attributes {
			parts = append(parts, string(a.Key), a.Value.Emit())
		}
		for _, e := range s.Events {
			parts = append(parts, e.Name)
			for _, a := range e.Attributes {
				parts = append(parts, string(a.Key), a.Value.Emit())
			}
		}
		for _, l := range s.Links {
			for _, a := range l.Attributes {
				parts = append(parts, string(a.Key), a.Value.Emit())
			}
		}
		if s.Resource != nil {
			for _, a := range s.Resource.Attributes() {
				parts = append(parts, string(a.Key), a.Value.Emit())
			}
		}
		for _, p := range parts {
			if strings.Contains(p, sentinel) {
				t.Errorf("span %q exports the sentinel: %q", s.Name, p)
			}
		}
	}
}

// TestCredentialPathsAreNeverTraced: every path whose credential the access
// log redacts (loggedPath) gets no span at all — otelhttp records url.path,
// and the token is IN the path. A relationship between the two lists, read
// from loggedPath's own behaviour.
func TestCredentialPathsAreNeverTraced(t *testing.T) {
	redacted := 0
	for _, p := range []string{"/auth/claim/tok", "/invite/tok", "/auth/reset/tok", "/agents/x", "/mirror/view"} {
		cut := loggedPath(p) != p
		if cut {
			redacted++
		}
		if cut && telemetry.TracedPath(p) {
			t.Errorf("%s: the access log redacts its credential, and a span would record it", p)
		}
		if !cut && !telemetry.TracedPath(p) {
			t.Errorf("%s: an ordinary page is not traced (the control)", p)
		}
	}
	if redacted != 3 {
		t.Fatalf("POSITIVE CONTROL: %d of the probe paths are redacted by loggedPath, want 3", redacted)
	}
}
