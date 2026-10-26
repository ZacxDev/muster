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
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/telemetry"
	"github.com/ZacxDev/muster/internal/ui"
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

// Attach is a terminal whose screen prints the sentinel (pane contents), then
// stays attached until it is closed.
func (b *sentinelBackend) Attach(context.Context, string, bool, uint16, uint16) (mirrorhost.Terminal, error) {
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("pane text: " + sentinel + "\r\n")) }()
	return &sentinelTerm{r: pr, w: pw}, nil
}

type sentinelTerm struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (t *sentinelTerm) Read(p []byte) (int, error)  { return t.r.Read(p) }
func (t *sentinelTerm) Write(p []byte) (int, error) { return len(p), nil }
func (t *sentinelTerm) Resize(uint16, uint16) error { return nil }
func (t *sentinelTerm) Close() error                { _ = t.w.Close(); return t.r.Close() }
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
	return newTracedMirrorRigAt(t, telemetry.DefaultSampleRatio)
}

// newTracedMirrorRigAt is the rig with the sampler's ratio for requests that
// open nothing (a send, a poll) at ratio.
func newTracedMirrorRigAt(t *testing.T, ratio float64) *tracedMirrorRig {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	// What a deployment builds: muster's sampler and the export scrub.
	tp := telemetry.NewProvider(exp, ratio, true)
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
	c, err := mirrorhost.New(mirrorhost.Config{URL: ts.URL, Token: tok, Host: host, Backend: backend, AllowHTTP: true, Interactive: true,
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
// document's traceparent and the socket's first batch, and closes the socket.
func (r *tracedMirrorRig) open(t *testing.T) (docTP string, firstBatch string) {
	t.Helper()
	docTP, firstBatch, c := r.openLive(t, "/mirror/view")
	_ = c.Close(websocket.StatusNormalClosure, "")
	return docTP, firstBatch
}

// openLive is open on page (/mirror/view or /mirror/terminal), leaving the
// socket open.
func (r *tracedMirrorRig) openLive(t *testing.T, page string) (docTP string, firstBatch string, c *websocket.Conn) {
	t.Helper()
	q := url.Values{"host": {r.host}, "session": {r.session}}
	resp, body := mirrorGet(t, r.s, r.ts, page+"?"+q.Encode())
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
	if page == "/mirror/terminal" {
		q.Set("mode", "ro")
		q.Set("cols", "80")
		q.Set("rows", "24")
	}
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(r.s))
	h.Set("Origin", r.ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.ts.URL, "http")+page+"/ws?"+q.Encode(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	// A terminal's first message is its "ready" and then its bytes; a view's
	// is its first batch. Read until the payload is there.
	var all []byte
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, data...)
		if page == "/mirror/view" || bytes.Contains(all, []byte(sentinel)) {
			return docTP, string(all), c
		}
	}
}

// waitFor waits until every name has an exported span, and returns them.
func (r *tracedMirrorRig) waitFor(t *testing.T, names ...string) tracetest.SpanStubs {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		spans := r.exp.GetSpans()
		have := map[string]bool{}
		for _, s := range spans {
			have[s.Name] = true
		}
		missing := ""
		for _, n := range names {
			if !have[n] {
				missing = n
			}
		}
		if missing == "" {
			return spans
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q span was exported (have %v)", missing, have)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
		// host.watch, not an earlier host phase: it ends last, after the
		// first batch's write returns on the host (host.send) — which can be
		// after muster has read the batch and every server span has ended.
		if names["host.watch"] && names["GET /mirror/view/ws"] && names["mirror.watch"] && names["GET /mirror/view/ws closed"] {
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
	// The socket ended because the browser left: its END is a root span of
	// its own, linked to the open, in ANOTHER trace (the open's trace ends at
	// the first batch).
	ws, _ := stub(spans, "GET /mirror/view/ws")
	closed, _ := stub(spans, "GET /mirror/view/ws closed")
	if attr(closed, "muster.close_reason") != telemetry.ReasonClientLeft {
		t.Errorf("the socket's close reason is %q, want %q", attr(closed, "muster.close_reason"), telemetry.ReasonClientLeft)
	}
	if closed.Parent.IsValid() || closed.SpanContext.TraceID() == ws.SpanContext.TraceID() ||
		len(closed.Links) != 1 || closed.Links[0].SpanContext.SpanID() != ws.SpanContext.SpanID() {
		t.Errorf("the close span is not a root linked to the open (parent %v, links %v)", closed.Parent, closed.Links)
	}
}

func attr(s tracetest.SpanStub, key string) string {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.Emit()
		}
	}
	return ""
}

// TestASocketsOpenSpanEndsAtItsFirstBatch: while the view's socket is still
// open, its open is already exported, whole — the socket span, the watch and
// the host's — so a view left open for hours is not one trace growing for
// hours. Nothing the socket does after that joins the open's trace.
func TestASocketsOpenSpanEndsAtItsFirstBatch(t *testing.T) {
	r := newTracedMirrorRig(t)
	_, _, c := r.openLive(t, "/mirror/view")
	defer c.CloseNow()
	spans := r.waitFor(t, "GET /mirror/view/ws", "mirror.watch", "mirror.watch.first_batch", "host.watch")
	if _, ok := stub(spans, "GET /mirror/view/ws closed"); ok {
		t.Fatal("the socket's close was recorded while it was open")
	}
	ws, _ := stub(spans, "GET /mirror/view/ws")
	if ws.Status.Code != codes.Unset && ws.Status.Code != codes.Ok {
		t.Errorf("the open span's status is %v", ws.Status)
	}
	before := len(spans)
	time.Sleep(200 * time.Millisecond) // ten host polls
	for _, s := range r.exp.GetSpans()[before:] {
		if s.SpanContext.TraceID() == ws.SpanContext.TraceID() {
			t.Errorf("%q joined the open's trace after its first batch", s.Name)
		}
	}
}

// TestADeployIsReportedAsShutdownNotTheBrowserLeaving: EndTerminals (what a
// SIGTERM runs) ends a live view and a live terminal; each page is TOLD, in
// an error frame whose reason is "shutdown", and each end is counted and
// recorded as shutdown — an error — never as client_left.
func TestADeployIsReportedAsShutdownNotTheBrowserLeaving(t *testing.T) {
	r := newTracedMirrorRig(t)
	before := map[string]float64{}
	for _, v := range []string{openViewMirror, openViewTerminal} {
		before[v] = testutil.ToFloat64(metrics.ViewDisconnects.WithLabelValues(v, telemetry.ReasonShutdown))
	}
	_, _, view := r.openLive(t, "/mirror/view")
	defer view.CloseNow()
	_, _, term := r.openLive(t, "/mirror/terminal")
	defer term.CloseNow()

	// The pages read while muster shuts down, as a browser does (the close
	// handshake waits for them).
	told := map[string]chan string{}
	for name, c := range map[string]*websocket.Conn{"view": view, "terminal": term} {
		ch := make(chan string, 1)
		told[name] = ch
		go func(c *websocket.Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reason := ""
			for {
				typ, data, err := c.Read(ctx)
				if err != nil {
					// Read on to the close, as a page does: the server's close
					// handshake waits for it.
					if reason == "" {
						reason = "(closed with no shutdown frame: " + err.Error() + ")"
					}
					ch <- reason
					return
				}
				var m map[string]string
				if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m["type"] == "error" {
					reason = m["reason"]
				}
			}
		}(c)
	}
	if !r.s.EndTerminals(5 * time.Second) {
		t.Fatal("EndTerminals timed out")
	}
	for name, ch := range told {
		if reason := <-ch; reason != telemetry.ReasonShutdown {
			t.Errorf("%s: the page was told %q, want %q", name, reason, telemetry.ReasonShutdown)
		}
	}
	for _, v := range []string{openViewMirror, openViewTerminal} {
		if got := testutil.ToFloat64(metrics.ViewDisconnects.WithLabelValues(v, telemetry.ReasonShutdown)) - before[v]; got != 1 {
			t.Errorf("muster_view_disconnects_total{view=%q,reason=shutdown} moved by %v, want 1", v, got)
		}
	}
	spans := r.waitFor(t, "GET /mirror/view/ws closed", "GET /mirror/terminal/ws closed")
	for _, n := range []string{"GET /mirror/view/ws closed", "GET /mirror/terminal/ws closed"} {
		s, _ := stub(spans, n)
		if attr(s, "muster.close_reason") != telemetry.ReasonShutdown || s.Status.Code != codes.Error {
			t.Errorf("%s: reason %q status %v, want shutdown and an error", n, attr(s, "muster.close_reason"), s.Status)
		}
	}
}

// TestARefusalBeforeTheUpgradeIsCounted: a socket refused before it is
// upgraded (a host that is not the principal's) is counted, with its reason.
func TestARefusalBeforeTheUpgradeIsCounted(t *testing.T) {
	r := newTracedMirrorRig(t)
	for _, c := range []struct{ view, path string }{{openViewMirror, "/mirror/view/ws"}, {openViewTerminal, "/mirror/terminal/ws"}} {
		before := testutil.ToFloat64(metrics.ViewDisconnects.WithLabelValues(c.view, "not_found"))
		resp, _ := mirrorGet(t, r.s, r.ts, c.path+"?host=nobody&session=x")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s for another host answered %d", c.path, resp.StatusCode)
		}
		if got := testutil.ToFloat64(metrics.ViewDisconnects.WithLabelValues(c.view, "not_found")) - before; got != 1 {
			t.Errorf("%s: not_found counted %v time(s), want 1", c.path, got)
		}
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
//
// It covers the rendered view's open, the terminal's open (whose screen prints
// the sentinel) and a hub request (a send whose text is the sentinel, which
// the host refuses — its spans run either way). It does NOT cover an agent's
// page: this rig has no agent store with a chat.
func TestNoExportedSpanCarriesPrivateContent(t *testing.T) {
	// Ratio 1: the send is not an open, and must be kept to be read.
	r := newTracedMirrorRigAt(t, 1)
	_, batch := r.open(t)
	if !strings.Contains(batch, sentinel) {
		t.Fatalf("POSITIVE CONTROL: the batch the browser got does not carry the sentinel; the fixture never flowed: %.300s", batch)
	}
	_, screen, term := r.openLive(t, "/mirror/terminal")
	if !strings.Contains(screen, sentinel) {
		t.Fatalf("POSITIVE CONTROL: the terminal's screen does not carry the sentinel: %q", screen)
	}
	_ = term.Close(websocket.StatusNormalClosure, "")
	form := url.Values{"host": {r.host}, "session": {r.session}, "text": {"send " + sentinel}, "nonce": {newSendNonce()}}
	req, _ := http.NewRequest(http.MethodPost, r.ts.URL+ui.MirrorSendPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookieFor(r.s))
	req.Header.Set("Origin", r.ts.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sendBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	t.Logf("the send answered %d %q", resp.StatusCode, sendBody)
	r.waitFor(t, "mirror.paste", "host.paste", "mirror.attach", "host.attach", "GET /mirror/terminal/ws closed")
	spans := r.spans(t)
	layers := map[string]bool{}
	for _, s := range spans {
		layers[strings.SplitN(s.Name, ".", 2)[0]] = true
	}
	for _, l := range []string{"GET /mirror/view", "GET /mirror/terminal", "mirror", "host", "open"} {
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

// TestTheOpenCostAttributesAreSizesAndCountsOnly extends the sentinel test
// above to what an open's spans say its first batch COST (mirror.watch and
// host.watch: its size as JSON and on the host's socket, its event count, the
// link's round trip). The sentinel test proves no attribute spells private
// content; this pins that each of these is a number — a shape no transcript,
// title or path can take — and that they are there at all (POSITIVE CONTROL: an export without them would pass the type check).
func TestTheOpenCostAttributesAreSizesAndCountsOnly(t *testing.T) {
	r := newTracedMirrorRigAt(t, 1)
	_, batch := r.open(t)
	if !strings.Contains(batch, sentinel) {
		t.Fatalf("POSITIVE CONTROL: the batch does not carry the sentinel: %.300s", batch)
	}
	spans := r.waitFor(t, "mirror.watch", "host.watch")
	want := map[string]map[string]attribute.Type{
		"mirror.watch": {"mirror.first_batch_bytes": attribute.INT64, "mirror.first_batch_events": attribute.INT64},
		"host.watch": {"mirror.first_batch_bytes": attribute.INT64, "mirror.first_batch_events": attribute.INT64,
			"mirror.first_batch_wire_bytes": attribute.INT64},
	}
	for name, keys := range want {
		s, ok := stub(spans, name)
		if !ok {
			t.Fatalf("no %q span", name)
		}
		got := map[string]attribute.Value{}
		for _, a := range s.Attributes {
			got[string(a.Key)] = a.Value
		}
		for k, typ := range keys {
			v, ok := got[k]
			if !ok {
				t.Errorf("%s carries no %s", name, k)
				continue
			}
			if v.Type() != typ {
				t.Errorf("%s's %s is a %s, want %s", name, k, v.Type(), typ)
			}
		}
		// Every first_batch / link attribute is typed, whoever adds the next.
		for k, v := range got {
			if strings.HasPrefix(k, "mirror.first_batch_") || k == "mirror.host_rtt_ms" {
				switch v.Type() {
				case attribute.INT64, attribute.FLOAT64, attribute.BOOL:
				default:
					t.Errorf("%s's %s is a %s: an open's cost is numbers and booleans only", name, k, v.Type())
				}
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
