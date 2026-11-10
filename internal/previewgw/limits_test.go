package previewgw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
	"github.com/coder/websocket"
)

// ------------------------------------------------------------ helpers

// inFlight is preview id's count of requests and WebSockets in flight, -1
// when it is not open.
func inFlight(e *gwEnv, id string) int {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	p, ok := e.g.previews[id]
	if !ok {
		return -1
	}
	return p.active
}

// syncOffers waits until every offer line sent on fh before it has been
// applied: the gateway reads the stream in order, so a listeners line with
// a fresh FirstSeen landing means every earlier line has landed (a windows
// line included, with what it does to the previews).
func (e *gwEnv) syncOffers(t *testing.T, fh *fakeHost, owner int64, host string, ls []wire.Listener, mark string) {
	t.Helper()
	marked := append([]wire.Listener(nil), ls...)
	marked[0].FirstSeen = mark
	fh.offers <- wire.Offer{Type: wire.TypeListeners, Listeners: marked}
	waitFor(t, "the offer stream to catch up", func() bool {
		tn := e.tunnelFor(owner, host)
		tn.mu.Lock()
		defer tn.mu.Unlock()
		return len(tn.state.Listeners) > 0 && tn.state.Listeners[0].FirstSeen == mark
	})
}

// ------------------------------------------------------------ the tests

// ------------------------------------------------------------ the per-preview cap

// The per-preview cap's fixtures: a cap of 5 — not the default 64 — on a
// host whose maxConns is 9, so every admitted request holds a real upstream
// stream and the host's own slots are never what a request waits for; and
// the harness's 300 ms dial wait.
const (
	capFixture      = 5
	capHostMaxConns = 9
	capWait         = 300 * time.Millisecond
	// capSlow is how long /slow takes to answer its headers.
	capSlow = 410 * time.Millisecond
)

// capApp is a loopback app: a WebSocket upgrade is accepted and held until
// the browser leaves; /hold/<k> answers its headers and blocks until
// release(k) (or the request ends); /fail drops the connection unanswered;
// /slow answers its headers after capSlow; anything else answers 200 "ok",
// counted.
type capApp struct {
	port int
	mu   sync.Mutex
	rel  map[string]chan struct{}
	oks  atomic.Int64
}

func newCapApp(t *testing.T) *capApp {
	t.Helper()
	a := &capApp{rel: map[string]chan struct{}{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headerHasToken(r.Header, "Connection", "upgrade") {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer c.CloseNow()
			for {
				if _, _, err := c.Read(r.Context()); err != nil {
					return
				}
			}
		}
		if k, ok := strings.CutPrefix(r.URL.Path, "/hold/"); ok {
			ch := a.ch(k)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-ch:
			case <-r.Context().Done():
			}
			return
		}
		if r.URL.Path == "/slow" {
			time.Sleep(capSlow)
			_, _ = io.WriteString(w, "slow")
			return
		}
		if r.URL.Path == "/fail" {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				c.Close()
			}
			return
		}
		a.oks.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	a.port = srv.Listener.Addr().(*net.TCPAddr).Port
	return a
}

func (a *capApp) ch(k string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.rel[k]
	if !ok {
		ch = make(chan struct{})
		a.rel[k] = ch
	}
	return ch
}

func (a *capApp) release(k string) { close(a.ch(k)) }

// capEnv is a gateway with the cap fixtures (dialWait: the dial wait; 0 is
// capWait) and one open preview on a capApp.
type capEnv struct {
	*gwEnv
	app    *capApp
	id     string
	hdr    http.Header
	client *http.Client
}

func newCapEnv(t *testing.T, dialWait time.Duration) *capEnv {
	t.Helper()
	if dialWait == 0 {
		dialWait = capWait
	}
	const host = "alpha"
	e := newGW(t, gwOpts{dialWait: dialWait, idle: 20 * time.Hour, ttl: 30 * time.Hour, perPrev: capFixture})
	app := newCapApp(t)
	e.serve(4301, app.port)
	fh := e.mustDial(t, dialOpts{listener: "public", token: e.mint(t, 1, host), hostName: host, maxConns: capHostMaxConns})
	fh.offers <- wire.Offer{Type: wire.TypeConfig, Config: &wire.Config{Ports: []string{"4000-4999"}, Offer: true, MaxOpen: 8,
		MaxConns: capHostMaxConns}}
	waitFor(t, "the config", func() bool { return e.tunnelFor(1, host).stateMaxConns() == capHostMaxConns })
	e.syncOffers(t, fh, 1, host, []wire.Listener{{Port: 4301, Process: "vite", Window: "@1"}}, "m1")
	id, cookie, _ := e.admitIn(t, 1, host, "@1", 4301)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200}}
	t.Cleanup(client.CloseIdleConnections)
	return &capEnv{gwEnv: e, app: app, id: id, hdr: own(id, cookie), client: client}
}

// result is one request's outcome: status 0 when it failed (err set).
type result struct {
	status int
	code   string
	body   string
	err    error
	took   time.Duration
}

// get sends GET path for the preview under ctx and delivers its outcome on
// the channel it returns. A /hold/ response is delivered at its headers
// (the body is drained afterwards, in the background).
func (c *capEnv) get(ctx context.Context, path string) <-chan result {
	out := make(chan result, 1)
	go func() {
		start := time.Now()
		req, _ := http.NewRequestWithContext(ctx, "GET", c.public.URL+path, nil)
		req.Host = pHost(c.id)
		for k, vs := range c.hdr {
			req.Header[k] = vs
		}
		resp, err := c.client.Do(req)
		if err != nil {
			out <- result{err: err, took: time.Since(start)}
			return
		}
		r := result{status: resp.StatusCode, code: resp.Header.Get(ErrorCodeHeader), took: time.Since(start)}
		if strings.HasPrefix(path, "/hold/") {
			out <- r
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		r.body = string(b)
		out <- r
	}()
	return out
}

// fill holds the cap: capFixture /hold/<i> requests, each answered (so in
// flight upstream) before it returns.
func (c *capEnv) fill(t *testing.T) {
	t.Helper()
	for i := range capFixture {
		if r := <-c.get(context.Background(), "/hold/"+strconv.Itoa(i)); r.status != http.StatusOK {
			t.Fatalf("holding request %d: HTTP %d %v", i, r.status, r.err)
		}
	}
	if n := inFlight(c.gwEnv, c.id); n != capFixture {
		t.Fatalf("after filling the cap %d in flight; want %d", n, capFixture)
	}
}

// pending reports that ch has no outcome yet, at d from now.
func pending(ch <-chan result, d time.Duration) (result, bool) {
	select {
	case r := <-ch:
		return r, false
	case <-time.After(d):
		return result{}, true
	}
}

func recv(t *testing.T, what string, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no outcome after 10 s", what)
		return result{}
	}
}

// 🔴 One request past a preview's cap WAITS for a slot: when one of the
// requests in flight ends within the dial wait, the waiter is admitted and
// answered by the app — it neither is refused nor goes past the cap.
func TestOneRequestPastThePerPreviewCapWaitsForASlot(t *testing.T) {
	c := newCapEnv(t, 0)
	c.fill(t)
	w := c.get(context.Background(), "/x")
	if r, ok := pending(w, capWait/3); !ok {
		t.Fatalf("the request past the cap was answered at once (HTTP %d %q %v); want it to wait for a slot", r.status, r.code, r.err)
	}
	if n := inFlight(c.gwEnv, c.id); n != capFixture {
		t.Fatalf("while one waits %d in flight; want %d (the waiter holds no slot)", n, capFixture)
	}
	c.app.release("0")
	r := recv(t, "the waiter", w)
	if r.status != http.StatusOK || r.body != "ok" {
		t.Fatalf("the waiter, after a slot freed: HTTP %d %s=%q %q; want the app's 200", r.status, ErrorCodeHeader, r.code, r.body)
	}
	if n := c.metric(t, `muster_preview_refusals_total{reason="too_many_connections"}`); n > 0 {
		t.Fatalf("a waiter that got a slot counted %v refusals", n)
	}
}

// 🔴 When none of the requests in flight ends, the one past the cap is
// refused 429 too_many_connections after the dial wait — not at once, not
// later — and the app never sees it; the refusal holds no slot and is
// counted in muster_preview_refusals_total.
func TestOneRequestPastThePerPreviewCapIsRefusedAfterTheWait(t *testing.T) {
	c := newCapEnv(t, 0)
	c.fill(t)
	r := recv(t, "the request past the cap", c.get(context.Background(), "/x"))
	if r.status != http.StatusTooManyRequests || r.code != "too_many_connections" {
		t.Fatalf("the request past the cap: HTTP %d %s=%q %v; want 429 too_many_connections", r.status, ErrorCodeHeader, r.code, r.err)
	}
	if r.took < capWait-20*time.Millisecond || r.took > capWait+2*time.Second {
		t.Fatalf("refused after %v; want after the %v dial wait", r.took, capWait)
	}
	if n := c.app.oks.Load(); n != 0 {
		t.Fatalf("the app saw %d refused requests", n)
	}
	if n := inFlight(c.gwEnv, c.id); n != capFixture {
		t.Fatalf("after a refusal %d in flight; want %d (a refusal holds no slot)", n, capFixture)
	}
	if n := c.metric(t, `muster_preview_refusals_total{reason="too_many_connections"}`); n != 1 {
		t.Fatalf("muster_preview_refusals_total{too_many_connections} = %v; want 1", n)
	}
}

// 🔴 A waiter whose browser leaves takes no slot with it: afterwards the
// preview has room for exactly the cap — one freed slot admits exactly one
// more, and the next waits and is refused. Failed requests (the app drops
// them, 502) hold none either.
func TestAWaiterThatLeavesHoldsNoSlot(t *testing.T) {
	c := newCapEnv(t, 0)
	c.fill(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := c.get(ctx, "/x")
	if _, ok := pending(w, capWait/3); !ok {
		t.Fatal("the request past the cap was answered at once; want it to wait")
	}
	cancel()
	if r := recv(t, "the waiter that left", w); r.err == nil {
		t.Fatalf("the waiter that left got HTTP %d; want its own cancellation", r.status)
	}
	time.Sleep(capWait) // past the dial wait the leaver would have had
	if n := inFlight(c.gwEnv, c.id); n != capFixture {
		t.Fatalf("after the waiter left %d in flight; want %d", n, capFixture)
	}
	if n := c.metric(t, `muster_preview_refusals_total{reason="too_many_connections"}`); n > 0 {
		t.Fatalf("a waiter that left counted %v refusals; it was refused nothing", n)
	}
	c.app.release("0")
	waitFor(t, "the finished request's slot to free", func() bool { return inFlight(c.gwEnv, c.id) == capFixture-1 })
	if r := <-c.get(context.Background(), "/hold/again"); r.status != http.StatusOK {
		t.Fatalf("the freed slot: HTTP %d %s=%q %v; want it admitted", r.status, ErrorCodeHeader, r.code, r.err)
	}
	if r := recv(t, "one past the cap again", c.get(context.Background(), "/x")); r.status != http.StatusTooManyRequests ||
		r.code != "too_many_connections" {
		t.Fatalf("one past the cap after the leaver: HTTP %d %s=%q; want 429 too_many_connections (capacity exactly %d)",
			r.status, ErrorCodeHeader, r.code, capFixture)
	}
	for i := 1; i < capFixture; i++ {
		c.app.release(strconv.Itoa(i))
	}
	c.app.release("again")
	waitFor(t, "every slot to free", func() bool { return inFlight(c.gwEnv, c.id) == 0 })
	for i := range 3 * capFixture {
		if r := <-c.get(context.Background(), "/fail"); r.status != http.StatusBadGateway {
			t.Fatalf("failing request %d: HTTP %d; want 502", i+1, r.status)
		}
	}
	held := make([]<-chan result, 0, capFixture)
	for i := range capFixture {
		held = append(held, c.get(context.Background(), "/hold/f"+strconv.Itoa(i)))
	}
	for i, h := range held {
		if r := recv(t, "a slot after the failures", h); r.status != http.StatusOK {
			t.Fatalf("slot %d after %d failed requests: HTTP %d %s=%q; want all %d free", i, 3*capFixture, r.status,
				ErrorCodeHeader, r.code, capFixture)
		}
	}
}

// 🔴 Closing a preview releases its waiters at once, on the closed path
// (404 preview_closed) — not after the dial wait. The dial wait here is
// long, so a waiter left to it would still be waiting.
func TestClosingAPreviewReleasesItsWaitersAtOnce(t *testing.T) {
	const long = 20 * time.Second
	c := newCapEnv(t, long)
	c.fill(t)
	ws := []<-chan result{c.get(context.Background(), "/x"), c.get(context.Background(), "/y")}
	for _, w := range ws {
		if _, ok := pending(w, 100*time.Millisecond); !ok {
			t.Fatal("a request past the cap was answered at once; want it to wait")
		}
	}
	if !c.g.closeFromPage(pageClaim{Owner: 1, Host: "alpha", Window: "@1"}, c.id) {
		t.Fatal("Close found no preview")
	}
	for i, w := range ws {
		r := recv(t, "a waiter of the closed preview", w)
		if r.status != http.StatusNotFound || r.code != codePreviewClosed {
			t.Fatalf("waiter %d after Close: HTTP %d %s=%q %v; want 404 %s", i, r.status, ErrorCodeHeader, r.code, r.err, codePreviewClosed)
		}
		if r.took > 3*time.Second {
			t.Fatalf("waiter %d was released %v after it began; want promptly at the close, not at the %v dial wait", i, r.took, long)
		}
	}
	if n := c.app.oks.Load(); n != 0 {
		t.Fatalf("the app saw %d requests of the closed preview's waiters", n)
	}
}

// 🔴 The close itself wakes a waiter — not the uses it cuts ending and
// freeing their slots. The slots here are held by no request at all, so
// nothing ends when the preview closes; the waiter must still be answered
// preview_closed at once, not at the (long) dial wait.
func TestAClosedPreviewWakesAWaiterItself(t *testing.T) {
	const long = 20 * time.Second
	c := newCapEnv(t, long)
	c.g.mu.Lock()
	slots := c.g.previews[c.id].slots
	c.g.mu.Unlock()
	for range capFixture {
		slots <- struct{}{}
	}
	type begun struct {
		code string
		left bool
		took time.Duration
	}
	out := make(chan begun, 1)
	go func() {
		start := time.Now()
		_, code, left := c.g.useBegin(context.Background(), c.id)
		out <- begun{code, left, time.Since(start)}
	}()
	select {
	case b := <-out:
		t.Fatalf("useBegin past the cap returned at once (%q); want it to wait", b.code)
	case <-time.After(100 * time.Millisecond):
	}
	if !c.g.closeFromPage(pageClaim{Owner: 1, Host: "alpha", Window: "@1"}, c.id) {
		t.Fatal("Close found no preview")
	}
	select {
	case b := <-out:
		if b.code != codePreviewClosed || b.left {
			t.Fatalf("the waiter after Close: code %q left %v; want %s", b.code, b.left, codePreviewClosed)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the waiter was not woken by the close itself within 3 s (dial wait %v)", long)
	}
}

// 🔴 WebSockets count against the same cap as requests: with the cap held
// by WebSockets, a request waits and is refused after the dial wait, and so
// is one more WebSocket; when one WebSocket ends, its slot admits a request.
func TestWebSocketsCountAgainstThePerPreviewCap(t *testing.T) {
	c := newCapEnv(t, 0)
	var conns []*websocket.Conn
	for i := range capFixture {
		ws, _, err := c.wsDial(t, c.id, "/hmr", c.hdr, nil)
		if err != nil {
			t.Fatalf("WebSocket %d: %v", i, err)
		}
		conns = append(conns, ws)
	}
	t.Cleanup(func() {
		for _, ws := range conns {
			ws.CloseNow()
		}
	})
	waitFor(t, "the WebSockets in flight", func() bool { return inFlight(c.gwEnv, c.id) == capFixture })
	r := recv(t, "a request past WebSockets at the cap", c.get(context.Background(), "/x"))
	if r.status != http.StatusTooManyRequests || r.code != "too_many_connections" || r.took < capWait-20*time.Millisecond {
		t.Fatalf("a request with the cap held by WebSockets: HTTP %d %s=%q after %v; want 429 too_many_connections after the %v wait",
			r.status, ErrorCodeHeader, r.code, r.took, capWait)
	}
	start := time.Now()
	if _, resp, err := c.wsDial(t, c.id, "/hmr", c.hdr, nil); err == nil || statusOf(resp) != http.StatusTooManyRequests ||
		resp.Header.Get(ErrorCodeHeader) != "too_many_connections" || time.Since(start) < capWait-20*time.Millisecond {
		t.Fatalf("one more WebSocket: %v HTTP %d after %v; want 429 too_many_connections after the wait", err, statusOf(resp), time.Since(start))
	}
	conns[0].Close(websocket.StatusNormalClosure, "")
	waitFor(t, "the ended WebSocket's slot to free", func() bool { return inFlight(c.gwEnv, c.id) == capFixture-1 })
	if r := recv(t, "a request after a WebSocket ended", c.get(context.Background(), "/x")); r.status != http.StatusOK || r.body != "ok" {
		t.Fatalf("a request after a WebSocket ended: HTTP %d %s=%q; want the app's 200", r.status, ErrorCodeHeader, r.code)
	}
}

// The cap defaults to MaxConnsPerPreview (64), and that is what a preview
// gets when nothing overrides it.
func TestThePerPreviewCapDefaultsTo64(t *testing.T) {
	if MaxConnsPerPreview != 64 {
		t.Fatalf("MaxConnsPerPreview = %d; want the design's 64", MaxConnsPerPreview)
	}
	e := newGW(t, gwOpts{})
	e.hostFor(t, 1, "alpha", wire.Listener{Port: 5173, Process: "vite", Window: "@1"})
	id, _, _ := e.admitIn(t, 1, "alpha", "@1", 5173)
	e.g.mu.Lock()
	n := cap(e.g.previews[id].slots)
	e.g.mu.Unlock()
	if n != 64 {
		t.Fatalf("a preview's cap is %d; want 64", n)
	}
}

// pausedBody yields its first part, then waits on gate before the rest, so
// a test can see the app receive the first part before the rest exists.
type pausedBody struct {
	first, rest []byte
	gate        <-chan struct{}
	stage       int
}

func (b *pausedBody) Read(p []byte) (int, error) {
	switch b.stage {
	case 0:
		n := copy(p, b.first)
		b.first = b.first[n:]
		if len(b.first) == 0 {
			b.stage = 1
		}
		return n, nil
	case 1:
		<-b.gate
		b.stage = 2
		fallthrough
	default:
		if len(b.rest) == 0 {
			return 0, io.EOF
		}
		n := copy(p, b.rest)
		b.rest = b.rest[n:]
		return n, nil
	}
}

// onlyReader hides a reader's type, so net/http sends it chunked (no
// Content-Length).
type onlyReader struct{ io.Reader }

// 🔴 PREVIEW_MAX_BODY (100003 bytes here, matching no other limit) caps a
// proxied request's body: a Content-Length past it is refused 413
// body_too_large before anything is dialled (the app sees nothing); a
// chunked body is counted as it streams and refused the same way when it
// passes the limit; a body of exactly the limit passes either way. The
// count streams: the app receives the first part of an upload before the
// client has sent the rest (nothing is buffered).
func TestARequestBodyPastPreviewMaxBodyIsRefused(t *testing.T) {
	const limit = 100_003
	e := newGW(t, gwOpts{maxBody: limit})
	var dials atomic.Int64
	gotFirst := make(chan struct{}, 1)
	app := (&recorder{}).start(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			buf := make([]byte, 1000)
			if _, err := io.ReadFull(r.Body, buf); err != nil {
				http.Error(w, "short", http.StatusBadRequest)
				return
			}
			gotFirst <- struct{}{}
		}
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return
		}
		if r.URL.Path == "/stream" {
			n += 1000
		}
		_, _ = fmt.Fprintf(w, "got %d", n)
	})
	fh, id, cookie, _ := e.admitted(t, 4302, app)
	hdr := own(id, cookie)
	post := func(path string, body io.Reader) answer {
		t.Helper()
		resp := e.send(t, context.Background(), "POST", pHost(id), path, hdr, body)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return answer{resp.StatusCode, resp.Header, string(b)}
	}
	tooLarge := func(what string, a answer) {
		t.Helper()
		if a.status != http.StatusRequestEntityTooLarge || a.header.Get(ErrorCodeHeader) != "body_too_large" {
			t.Fatalf("%s: HTTP %d %s=%q %q; want 413 body_too_large", what, a.status, ErrorCodeHeader, a.header.Get(ErrorCodeHeader), a.body)
		}
	}

	dials.Store(fh.connects.Load())
	tooLarge("a Content-Length one byte past the limit", post("/cl", bytes.NewReader(make([]byte, limit+1))))
	if n := fh.connects.Load() - dials.Load(); n != 0 {
		t.Fatalf("a Content-Length past the limit dialled the host %d times; want none", n)
	}
	if a := post("/cl", bytes.NewReader(make([]byte, limit))); a.status != http.StatusOK || a.body != "got "+strconv.Itoa(limit) {
		t.Fatalf("a Content-Length of exactly the limit: HTTP %d %q; want the app's 200 got %d", a.status, a.body, limit)
	}
	tooLarge("a chunked body one byte past the limit", post("/chunked", onlyReader{bytes.NewReader(make([]byte, limit+1))}))
	if a := post("/chunked", onlyReader{bytes.NewReader(make([]byte, limit))}); a.status != http.StatusOK || a.body != "got "+strconv.Itoa(limit) {
		t.Fatalf("a chunked body of exactly the limit: HTTP %d %q; want the app's 200 got %d", a.status, a.body, limit)
	}

	gate := make(chan struct{})
	res := make(chan answer, 1)
	go func() {
		req, _ := http.NewRequest("POST", e.public.URL+"/stream", onlyReader{&pausedBody{first: make([]byte, 1000), rest: make([]byte, 500), gate: gate}})
		req.Host = pHost(id)
		for k, vs := range hdr {
			req.Header[k] = vs
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			res <- answer{body: err.Error()}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		res <- answer{status: resp.StatusCode, body: string(b)}
	}()
	select {
	case <-gotFirst:
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("the app never received the first 1000 bytes before the rest was sent: the gateway buffered the body")
	}
	close(gate)
	if a := <-res; a.status != http.StatusOK || a.body != "got 1500" {
		t.Fatalf("the streamed upload: HTTP %d %q; want the app's 200 got 1500", a.status, a.body)
	}
	if n := e.metric(t, `muster_preview_refusals_total{reason="body_too_large"}`); n != 2 {
		t.Fatalf("muster_preview_refusals_total{body_too_large} = %v; want 2", n)
	}
}

// PREVIEW_MAX_BODY: 32 MiB unset; bytes or KiB/MiB/GiB; refused outside
// 1 KiB–1 GiB or malformed.
func TestPreviewMaxBodyIsParsed(t *testing.T) {
	for v, want := range map[string]int64{"": 32 << 20, "1024": 1024, "1536": 1536, "48KiB": 48 << 10, "7MiB": 7 << 20, "1GiB": 1 << 30} {
		if got, err := byteSize("PREVIEW_MAX_BODY", v, DefaultMaxBody); err != nil || got != want {
			t.Errorf("%q: %d %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"1023", "0", "-5MiB", "2GiB", "32MB", "32 MiB", "1.5MiB", "MiB", "99999999999999999999"} {
		if got, err := byteSize("PREVIEW_MAX_BODY", v, DefaultMaxBody); err == nil {
			t.Errorf("%q accepted as %d", v, got)
		} else if !strings.Contains(err.Error(), "PREVIEW_MAX_BODY") {
			t.Errorf("%q: the error does not name the variable: %v", v, err)
		}
	}
}

// waitGone waits until preview id has closed (without reading the host's
// answers).
func waitGone(t *testing.T, e *gwEnv, id string) {
	t.Helper()
	waitFor(t, "the preview to close", func() bool { return !isOpen(e, id) })
}

// 🔴 A preview whose window has been gone from its host's window listing for
// 10 minutes closes, and its host is told. Times are literals off the 30 s
// sweep grid: the window leaves at 0m07s, so at the 10m00s sweep it has been
// gone 9m53s (open) and at the 10m30s sweep 10m23s (closed). A preview of a
// window still listed stays open; a window listed again before 10 minutes
// starts the clock over; a host that never sends a listing closes nothing;
// and a preview opened in a window the listing already lacks is gone from
// its open.
func TestAPreviewWhoseWindowIsGoneTenMinutesCloses(t *testing.T) {
	const host = "alpha"
	setup := func(t *testing.T) (*gwEnv, *fakeHost, []wire.Listener) {
		e := newGW(t, gwOpts{idle: 20 * time.Hour, ttl: 30 * time.Hour})
		app := appServer(t)
		e.serve(4311, app)
		e.serve(4312, app)
		ls := []wire.Listener{{Port: 4311, Process: "vite", Window: "@1"}, {Port: 4312, Process: "vite", Window: "@2"}}
		fh := e.hostFor(t, 1, host, ls...)
		return e, fh, ls
	}
	n := 0
	windows := func(t *testing.T, e *gwEnv, fh *fakeHost, ls []wire.Listener, ws ...string) {
		t.Helper()
		n++
		fh.offers <- wire.Offer{Type: wire.TypeWindows, Windows: ws}
		e.syncOffers(t, fh, 1, host, ls, "w"+strconv.Itoa(n))
	}
	t.Run("gone ten minutes", func(t *testing.T) {
		e, fh, ls := setup(t)
		windows(t, e, fh, ls, "@1", "@2", "@3")
		gone, _, _ := e.admitIn(t, 1, host, "@1", 4311)
		kept, _, _ := e.admitIn(t, 1, host, "@2", 4312)
		e.clock.Advance(7 * time.Second)
		windows(t, e, fh, ls, "@2", "@3")
		e.clock.Advance(10*time.Minute + 13*time.Second) // 10m20s
		if !isOpen(e, gone) {
			t.Fatal("closed 9m53s after its window left the listing; want open until 10 minutes")
		}
		e.clock.Advance(20 * time.Second) // 10m40s
		waitClosed(t, e, fh, gone)
		if !isOpen(e, kept) {
			t.Fatal("a preview whose window is still listed closed")
		}
		if v := e.metric(t, `muster_preview_closes_total{reason="window_gone"}`); v != 1 {
			t.Fatalf("muster_preview_closes_total{window_gone} = %v; want 1", v)
		}
	})
	t.Run("listed again starts over", func(t *testing.T) {
		e, fh, ls := setup(t)
		windows(t, e, fh, ls, "@1", "@2")
		id, _, _ := e.admitIn(t, 1, host, "@1", 4311)
		e.clock.Advance(7 * time.Second)
		windows(t, e, fh, ls, "@2")
		e.clock.Advance(5 * time.Minute) // 5m07s
		windows(t, e, fh, ls, "@1", "@2")
		e.clock.Advance(time.Minute) // 6m07s
		windows(t, e, fh, ls, "@2")
		e.clock.Advance(9*time.Minute + 53*time.Second) // 16m00s: gone 9m53s since 6m07s
		if !isOpen(e, id) {
			t.Fatal("closed though its window was listed again at 5m07s and left again only 9m53s ago")
		}
		e.clock.Advance(40 * time.Second) // 16m40s: the 16m30s sweep, gone 10m23s
		waitClosed(t, e, fh, id)
	})
	t.Run("no listing closes nothing", func(t *testing.T) {
		e, _, _ := setup(t)
		id, _, _ := e.admitIn(t, 1, host, "@1", 4311)
		e.clock.Advance(25 * time.Minute)
		if !isOpen(e, id) {
			t.Fatal("closed by a host that never sent a window listing")
		}
	})
	t.Run("opened in an unlisted window", func(t *testing.T) {
		e, fh, ls := setup(t)
		windows(t, e, fh, ls, "@2")
		e.clock.Advance(7 * time.Second)
		id, _, _ := e.admitIn(t, 1, host, "@1", 4311)
		e.clock.Advance(10*time.Minute + 13*time.Second)
		if !isOpen(e, id) {
			t.Fatal("closed 9m53s after it opened in an unlisted window")
		}
		e.clock.Advance(20 * time.Second)
		waitGone(t, e, id)
	})
}

// An overlong body's error (*http.MaxBytesError, however wrapped) is a 413
// body_too_large; any other is not.
func TestUpstreamErrorMapsAnOverlongBody(t *testing.T) {
	err := fmt.Errorf("write: %w", &http.MaxBytesError{Limit: 7})
	if st, code := upstreamError(context.Background(), err); st != http.StatusRequestEntityTooLarge || code != "body_too_large" {
		t.Fatalf("an overlong body: %d %s; want 413 body_too_large", st, code)
	}
	if st, code := upstreamError(context.Background(), errors.New("x")); st != http.StatusBadGateway || code != codeNotAnswering {
		t.Fatalf("control: another error: %d %s; want 502 not_answering", st, code)
	}
}

// 🔴 muster_preview_response_seconds starts its clock when the request takes
// its slot, as its Help says: a request that waited 730 ms for one (not the
// 300 ms harness wait, not capSlow) is observed at its app's latency, not at
// the wait plus it. Control: an app that takes capSlow (410 ms) to answer
// its headers IS observed at about that, so the clock is not simply started
// late.
func TestResponseSecondsExcludeTheWaitForASlot(t *testing.T) {
	const waited = 730 * time.Millisecond
	c := newCapEnv(t, 3*time.Second)
	sum := func() float64 { return c.metric(t, `muster_preview_response_seconds_sum{kind="host"}`) }
	c.fill(t)
	before := sum()
	w := c.get(context.Background(), "/x")
	if _, ok := pending(w, waited); !ok {
		t.Fatal("the request past the cap was answered at once; want it to wait for a slot")
	}
	c.app.release("0")
	r := recv(t, "the waiter", w)
	if r.status != http.StatusOK || r.took < waited {
		t.Fatalf("the waiter: HTTP %d after %v; want the app's 200 after at least %v", r.status, r.took, waited)
	}
	if n := c.metric(t, `muster_preview_response_seconds_count{kind="host"}`); n != capFixture+1 {
		t.Fatalf("response_seconds_count = %v; want %d (the holders and the waiter)", n, capFixture+1)
	}
	if d := sum() - before; d >= waited.Seconds()/2 {
		t.Fatalf("the waiter was observed at %.3f s; want its app's latency, not the %v it waited for a slot", d, waited)
	}
	before = sum()
	if r := recv(t, "the slow request", c.get(context.Background(), "/slow")); r.status != http.StatusOK {
		t.Fatalf("/slow: HTTP %d %v", r.status, r.err)
	}
	if d := sum() - before; d < capSlow.Seconds() || d > capSlow.Seconds()+2 {
		t.Fatalf("an app answering its headers after %v was observed at %.3f s; want about that", capSlow, d)
	}
}

// 🔴 Churn: 200 concurrent useBegin calls on a preview with a cap of 5 and
// a 50 ms wait — a third of them leaving at a random moment, every admitted
// one ending after a random hold — with the preview closed mid-flight, 20
// rounds. Afterwards the preview's slot channel is EMPTY: no path (fast,
// waited, left, refused, closed, or a slot taken after the close) keeps a
// token. A slot taken after the close is the re-check branch of useBegin,
// whose own token return this test is the only one to reach: delete it and
// tokens leak here.
func TestUseBeginUnderChurnLeaksNoSlot(t *testing.T) {
	c := newCapEnv(t, 50*time.Millisecond)
	var total [4]int64
	for round := range 20 {
		pctx, pcancel := context.WithCancel(c.g.ctx)
		now := c.g.clock.Now()
		p := &Preview{ID: fmt.Sprintf("churn%d", round), OwnerID: 1, Host: "alpha", Window: "@9", Port: 4999,
			Opened: now, lastUsed: now, ctx: pctx, cancel: pcancel, slots: make(chan struct{}, capFixture)}
		c.g.mu.Lock()
		c.g.previews[p.ID] = p
		c.g.mu.Unlock()
		var wg sync.WaitGroup
		var admitted, left, refused, closed atomic.Int64
		for i := range 200 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if i%3 == 0 {
					time.AfterFunc(time.Duration(rand.Int64N(int64(60*time.Millisecond))), cancel)
				}
				got, code, gone := c.g.useBegin(ctx, p.ID)
				switch {
				case gone:
					left.Add(1)
				case code == codePreviewClosed:
					closed.Add(1)
				case code != "":
					refused.Add(1)
				default:
					admitted.Add(1)
					time.Sleep(time.Duration(rand.Int64N(int64(3 * time.Millisecond))))
					c.g.useEnd(got)
				}
			}()
		}
		// Mid-flight; every fifth round past the 50 ms wait, so some uses are
		// refused before the close.
		closeAt := time.Duration(10+rand.Int64N(30)) * time.Millisecond
		if round%5 == 4 {
			closeAt += 50 * time.Millisecond
		}
		time.Sleep(closeAt)
		c.g.mu.Lock()
		c.g.deletePreviewLocked(p, closePage)
		c.g.mu.Unlock()
		wg.Wait()
		if n := len(p.slots); n != 0 {
			t.Fatalf("round %d: %d slot tokens left after every use ended (admitted %d, left %d, refused %d, closed %d)",
				round, n, admitted.Load(), left.Load(), refused.Load(), closed.Load())
		}
		total[0] += admitted.Load()
		total[1] += left.Load()
		total[2] += refused.Load()
		total[3] += closed.Load()
	}
	// The battery reached every outcome, not only the happy one.
	for i, what := range []string{"admitted", "left", "refused", "closed"} {
		if total[i] == 0 {
			t.Errorf("no use was %s in 20 rounds (admitted, left, refused, closed: %v)", what, total)
		}
	}
}
