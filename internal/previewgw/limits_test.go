package previewgw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// holdApp is a loopback app: /hold blocks until release is closed (or the
// request ends), /fail drops the connection without answering, anything
// else answers 200 "ok". It counts the requests each path saw.
type holdApp struct {
	port    int
	release chan struct{}
	mu      sync.Mutex
	seen    map[string]int
}

func newHoldApp(t *testing.T) *holdApp {
	t.Helper()
	a := &holdApp{release: make(chan struct{}), seen: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen[r.URL.Path]++
		a.mu.Unlock()
		switch r.URL.Path {
		case "/hold":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-a.release:
			case <-r.Context().Done():
			}
		case "/fail":
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				c.Close()
			}
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	t.Cleanup(func() {
		select {
		case <-a.release:
		default:
			close(a.release)
		}
		srv.Close()
	})
	a.port = srv.Listener.Addr().(*net.TCPAddr).Port
	return a
}

func (a *holdApp) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[path]
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

// 🔴 A preview has at most 64 requests and WebSockets in flight. The host's
// maxConns here is 7, so 7 of the 64 hold upstream streams and 57 queue for
// a slot (the dial wait is long): the 65th request — and a WebSocket — are
// refused 429 too_many_connections at once, never reaching the app. A slot
// is released when a request is cancelled, and when requests complete; a
// refusal holds none, and 70 requests the app drops (502) leak none.
func TestAPreviewHasAtMost64RequestsInFlight(t *testing.T) {
	const host = "alpha"
	e := newGW(t, gwOpts{dialWait: 20 * time.Second, idle: 20 * time.Hour, ttl: 30 * time.Hour})
	app := newHoldApp(t)
	e.serve(4301, app.port)
	fh := e.mustDial(t, dialOpts{listener: "public", token: e.mint(t, 1, host), hostName: host, maxConns: 7})
	fh.offers <- wire.Offer{Type: wire.TypeConfig, Config: &wire.Config{Ports: []string{"4000-4999"}, Offer: true, MaxOpen: 8, MaxConns: 7}}
	waitFor(t, "the config", func() bool { return e.tunnelFor(1, host).stateMaxConns() == 7 })
	ls := []wire.Listener{{Port: 4301, Process: "vite", Window: "@1"}}
	e.syncOffers(t, fh, 1, host, ls, "m1")
	id, cookie, _ := e.admitIn(t, 1, host, "@1", 4301)
	hdr := own(id, cookie)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200}}
	t.Cleanup(client.CloseIdleConnections)

	type held struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	hold := func() held {
		ctx, cancel := context.WithCancel(context.Background())
		h := held{cancel, make(chan struct{})}
		go func() {
			defer close(h.done)
			req, _ := http.NewRequestWithContext(ctx, "GET", e.public.URL+"/hold", nil)
			req.Host = pHost(id)
			for k, vs := range hdr {
				req.Header[k] = vs
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
		return h
	}
	var hs []held
	for range 64 {
		hs = append(hs, hold())
	}
	waitFor(t, "64 requests in flight", func() bool { return inFlight(e, id) == 64 })

	// A refusal is at once: a request still waiting after 3 s was admitted
	// and queued for a host slot (status 0 below).
	refused := func(what string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", e.public.URL+"/x", nil)
		req.Host = pHost(id)
		for k, vs := range hdr {
			req.Header[k] = vs
		}
		st, code := 0, ""
		if resp, err := client.Do(req); err == nil {
			st, code = resp.StatusCode, resp.Header.Get(ErrorCodeHeader)
			resp.Body.Close()
		}
		if st != http.StatusTooManyRequests || code != "too_many_connections" {
			t.Fatalf("%s: HTTP %d %s=%q; want 429 too_many_connections", what, st, ErrorCodeHeader, code)
		}
	}
	refused("the 65th request")
	if _, resp, err := e.wsDial(t, id, "/hmr", hdr, nil); err == nil || statusOf(resp) != http.StatusTooManyRequests ||
		resp.Header.Get(ErrorCodeHeader) != "too_many_connections" {
		t.Fatalf("a WebSocket past 64 in flight: %v HTTP %d; want 429 too_many_connections", err, statusOf(resp))
	}
	if n := app.count("/x") + app.count("/hmr"); n != 0 {
		t.Fatalf("the app saw %d refused requests", n)
	}
	if n := inFlight(e, id); n != 64 {
		t.Fatalf("after two refusals %d in flight; want 64 (a refusal holds no slot)", n)
	}

	hs[0].cancel()
	<-hs[0].done
	waitFor(t, "the cancelled request's slot to be released", func() bool { return inFlight(e, id) == 63 })
	hs = append(hs[1:], hold())
	waitFor(t, "the released slot to be taken again", func() bool { return inFlight(e, id) == 64 })
	refused("the 65th request, again")

	close(app.release)
	for _, h := range hs {
		<-h.done
	}
	waitFor(t, "every slot to be released on completion", func() bool { return inFlight(e, id) == 0 })
	for i := range 70 {
		if a := e.do(t, "GET", pHost(id), "/fail", hdr, ""); a.status != http.StatusBadGateway {
			t.Fatalf("failing request %d: HTTP %d; want 502", i+1, a.status)
		}
	}
	waitFor(t, "no slot held after 70 failed requests", func() bool { return inFlight(e, id) == 0 })
	if a := e.do(t, "GET", pHost(id), "/x", hdr, ""); a.status != http.StatusOK || a.body != "ok" {
		t.Fatalf("after the burst: HTTP %d %q; want the app's 200", a.status, a.body)
	}
	if n := e.metric(t, `muster_preview_refusals_total{reason="too_many_connections"}`); n != 3 {
		t.Fatalf("muster_preview_refusals_total{too_many_connections} = %v; want 3", n)
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
