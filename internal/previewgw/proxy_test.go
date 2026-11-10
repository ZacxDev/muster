package previewgw

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// ------------------------------------------------------------ helpers

// hostFor dials host for owner on the public listener and reports ls as its
// listeners, waiting until the gateway holds them.
func (e *gwEnv) hostFor(t *testing.T, owner int64, host string, ls ...wire.Listener) *fakeHost {
	t.Helper()
	fh := e.mustDial(t, dialOpts{listener: "public", token: e.mint(t, owner, host), hostName: host})
	fh.offers <- wire.Offer{Type: wire.TypeListeners, Listeners: ls}
	waitFor(t, "the listeners", func() bool {
		tn := e.tunnelFor(owner, host)
		if tn == nil {
			return false
		}
		tn.mu.Lock()
		defer tn.mu.Unlock()
		return len(tn.state.Listeners) == len(ls)
	})
	return fh
}

// admitIn opens port's offer on owner's session page for (host, window) and
// enters it as a new tab: the preview id, its owner cookie and the page token.
func (e *gwEnv) admitIn(t *testing.T, owner int64, host, window string, port int) (id, cookie, tok string) {
	t.Helper()
	a := e.enter(t, e.fm.handoff("mpl_owner"+itoa(owner), owner, host, "main", window), owner*10)
	if a.status != http.StatusOK {
		t.Fatalf("enter owner %d %s %s: HTTP %d", owner, host, window, a.status)
	}
	tok = pageToken(t, a)
	u, id, oa := e.openURL(t, tok, `{"port":`+strconv.Itoa(port)+`}`)
	if u == "" {
		t.Fatalf("open :%d: HTTP %d %s", port, oa.status, oa.body)
	}
	return id, ownerCookieOf(t, e.useEntry(t, id, entryCodeOf(t, u, id), "document")), tok
}

// admitted: owner 1's host alpha offers vport in @1, served by the loopback
// app on real; the owner opens it and enters. vport is what the gateway and
// the app's Host header name; real is where the app listens.
func (e *gwEnv) admitted(t *testing.T, vport, real int) (fh *fakeHost, id, cookie, tok string) {
	t.Helper()
	e.serve(vport, real)
	fh = e.hostFor(t, 1, "alpha", wire.Listener{Port: vport, Process: "vite", Window: "@1"})
	id, cookie, tok = e.admitIn(t, 1, "alpha", "@1", vport)
	return fh, id, cookie, tok
}

// relist replaces owner's host's listeners with ls, waiting until the
// gateway holds them.
func (e *gwEnv) relist(t *testing.T, fh *fakeHost, owner int64, host string, ls ...wire.Listener) {
	t.Helper()
	fh.offers <- wire.Offer{Type: wire.TypeListeners, Listeners: ls}
	waitFor(t, "the new listeners", func() bool {
		tn := e.tunnelFor(owner, host)
		tn.mu.Lock()
		defer tn.mu.Unlock()
		return len(tn.state.Listeners) == len(ls)
	})
}

// own is the headers of a same-origin request of preview id with cookie.
func own(id, cookie string) http.Header {
	return http.Header{"Cookie": {OwnerCookie + "=" + cookie}, "Sec-Fetch-Site": {"same-origin"},
		"Origin": {"https://" + pHost(id)}}
}

// send sends one request to the public listener and returns the response
// unread (the caller closes its body).
func (e *gwEnv) send(t *testing.T, ctx context.Context, method, host, target string, hdr http.Header, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, e.public.URL+target, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := (&http.Client{CheckRedirect: noRedirect.CheckRedirect}).Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, host, target, err)
	}
	return resp
}

// wsDial dials preview id's path as a browser would, with hdr.
func (e *gwEnv) wsDial(t *testing.T, id, path string, hdr http.Header, o *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if o == nil {
		o = &websocket.DialOptions{}
	}
	o.Host = pHost(id)
	o.HTTPHeader = hdr
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.public.URL, "http")+path, o)
}

// recorder is a loopback app that records each request's Host and headers
// and answers with what handle writes (200 "ok" when nil).
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
	n    atomic.Int64
}

func (rc *recorder) start(t *testing.T, handle http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.n.Add(1)
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, r.Clone(context.Background()))
		rc.mu.Unlock()
		if handle != nil {
			handle(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func (rc *recorder) last(t *testing.T) *http.Request {
	t.Helper()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.reqs) == 0 {
		t.Fatal("the app saw no request")
	}
	return rc.reqs[len(rc.reqs)-1]
}

var codeRE = regexp.MustCompile(`^[a-z_]{1,32}$`)

// ------------------------------------------------------------ the tests

// 🔴 Credentials never reach the app: the gateway's cookies, muster_*
// cookies, Forwarded / X-Forwarded-* / X-Real-IP / CF-* / True-Client-IP /
// X-Muster-* are all gone; the viewer's address is not forwarded; the app's
// own cookie and Authorization pass; Host is localhost:<the preview's port>
// (4174, not the app's real port), with the public host in
// X-Forwarded-Host and https in X-Forwarded-Proto.
func TestCredentialsNeverReachTheApp(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4174, rc.start(t, nil))
	h := own(id, cookie)
	h.Set("Cookie", "app=1; "+OwnerCookie+"="+cookie+"; muster_session=s3cret; "+ShareCookie+"=x; theme=dark")
	for k, v := range map[string]string{
		"X-Forwarded-For": "203.0.113.9", "Forwarded": "for=203.0.113.9", "X-Forwarded-Host": "evil.example",
		"X-Real-IP": "203.0.113.9", "CF-Connecting-IP": "203.0.113.9", "CF-Ray": "abc", "True-Client-IP": "203.0.113.9",
		"X-Muster-Owner": "1", "Authorization": "Bearer app-token",
	} {
		h.Set(k, v)
	}
	if a := e.do(t, "GET", pHost(id), "/api?q=1", h, ""); a.status != http.StatusOK || a.body != "ok" {
		t.Fatalf("proxied GET: HTTP %d %q", a.status, a.body)
	}
	r := rc.last(t)
	if got := r.Header.Get("Cookie"); got != "app=1; theme=dark" {
		t.Errorf("the app saw Cookie %q; want only its own cookies, \"app=1; theme=dark\"", got)
	}
	for _, k := range []string{"X-Real-IP", "Forwarded", "CF-Connecting-IP", "CF-Ray", "True-Client-IP", "X-Muster-Owner"} {
		if v := r.Header.Get(k); v != "" {
			t.Errorf("the app saw %s: %q", k, v)
		}
	}
	if v := r.Header.Values("X-Forwarded-For"); len(v) != 0 {
		t.Errorf("the app saw X-Forwarded-For %q: the viewer's address was forwarded", v)
	}
	if r.Host != "localhost:4174" {
		t.Errorf("the app saw Host %q; want localhost:4174", r.Host)
	}
	if got := r.Header.Get("X-Forwarded-Host"); got != pHost(id) {
		t.Errorf("X-Forwarded-Host %q; want %q", got, pHost(id))
	}
	if got := r.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto %q; want https", got)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer app-token" {
		t.Errorf("Authorization %q; the app's own must pass", got)
	}
	if got := r.Header.Get("Origin"); got != "http://localhost:4174" {
		t.Errorf("Origin %q; the preview's own origin must become http://localhost:4174", got)
	}
	if r.URL.RequestURI() != "/api?q=1" {
		t.Errorf("the app saw %q; want /api?q=1", r.URL.RequestURI())
	}
}

// The response rules: Set-Cookie loses Domain and a gateway-named cookie is
// dropped; a Location at the app's loopback port points at the preview,
// another is left alone; Referrer-Policy is no-referrer REPLACING the app's;
// HSTS is dropped; frame-ancestors with muster's origin is added beside the
// app's own CSP; nosniff.
func TestResponseRules(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4175, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Set-Cookie", "sid=abc; Domain=preview.example; Path=/; HttpOnly")
		h.Add("Set-Cookie", "pref=1; domain=.preview.example")
		h.Add("Set-Cookie", OwnerCookie+"=forged; Path=/; Secure")
		h.Add("Set-Cookie", "__host-MP=forged2; Path=/; Secure")
		h.Set("Referrer-Policy", "unsafe-url")
		h.Set("Strict-Transport-Security", "max-age=63072000")
		h.Set("Content-Security-Policy", "default-src 'self'")
		switch r.URL.Path {
		case "/go":
			h.Set("Location", "http://localhost:4175/next?x=1")
		case "/go6":
			h.Set("Location", "http://[::1]:4175/six")
		case "/away":
			h.Set("Location", "http://localhost:9999/elsewhere")
		}
		w.WriteHeader(http.StatusFound)
	}))
	a := e.do(t, "GET", pHost(id), "/go", own(id, cookie), "")
	if a.status != http.StatusFound {
		t.Fatalf("HTTP %d; want the app's 302", a.status)
	}
	if got := a.header.Values("Set-Cookie"); strings.Join(got, "|") != "sid=abc; Path=/; HttpOnly|pref=1" {
		t.Errorf("Set-Cookie %q; want Domain removed and the gateway-named cookies dropped", got)
	}
	if got := a.header.Get("Location"); got != "https://"+pHost(id)+"/next?x=1" {
		t.Errorf("Location %q; want the preview's origin", got)
	}
	if got := e.do(t, "GET", pHost(id), "/go6", own(id, cookie), "").header.Get("Location"); got != "https://"+pHost(id)+"/six" {
		t.Errorf("[::1] Location %q; want the preview's origin", got)
	}
	if got := e.do(t, "GET", pHost(id), "/away", own(id, cookie), "").header.Get("Location"); got != "http://localhost:9999/elsewhere" {
		t.Errorf("a Location at another port became %q; want it left alone", got)
	}
	if got := a.header.Values("Referrer-Policy"); len(got) != 1 || got[0] != "no-referrer" {
		t.Errorf("Referrer-Policy %q; want exactly no-referrer", got)
	}
	if got := a.header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS %q passed through", got)
	}
	if got := a.header.Values("Content-Security-Policy"); len(got) != 2 || got[0] != "default-src 'self'" ||
		got[1] != "frame-ancestors "+testPublicOrigin {
		t.Errorf("CSP %q; want the app's and frame-ancestors %s", got, testPublicOrigin)
	}
	if got := a.header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options %q", got)
	}
}

// 🔴 What the viewer sees from the gateway is a code, never an error's text;
// an app's own error is the app's response. Offline host: 503 host_offline.
// Nothing listening: 502 not_answering. The app's 500 passes as is. Every
// gateway page carries no-referrer.
func TestGatewayErrorsAreCodesAndAppErrorsPassThrough(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	fh, id, cookie, _ := e.admitted(t, 4176, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "app exploded: stack trace here", http.StatusInternalServerError)
	}))
	a := e.do(t, "GET", pHost(id), "/", own(id, cookie), "")
	if a.status != 500 || a.body != "app exploded: stack trace here\n" || a.header.Get(ErrorCodeHeader) != "" {
		t.Fatalf("the app's 500: HTTP %d %q (gateway code %q); want the app's response as it sent it",
			a.status, a.body, a.header.Get(ErrorCodeHeader))
	}
	leak := regexp.MustCompile(`previewgw|tunnel:|CONNECT|refused|dial |127\.0\.0\.1`)
	check := func(what string, a answer, status int, code string) {
		t.Helper()
		got := a.header.Get(ErrorCodeHeader)
		if a.status != status || got != code || !codeRE.MatchString(got) {
			t.Errorf("%s: HTTP %d code %q; want %d %s", what, a.status, got, status, code)
		}
		if m := leak.FindString(a.body); m != "" {
			t.Errorf("%s: the page leaks error text %q:\n%s", what, m, a.body)
		}
		if a.header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: no Referrer-Policy: no-referrer", what)
		}
	}
	e.serve(4176, closedPort(t))
	e.tunnelFor(1, "alpha").rt.CloseIdleConnections() // the pooled connection to the old app
	check("nothing listening", e.do(t, "GET", pHost(id), "/", own(id, cookie), ""), http.StatusBadGateway, codeNotAnswering)
	fh.cancel()
	waitFor(t, "the tunnel to go", func() bool { return e.tunnelFor(1, "alpha") == nil })
	check("host offline", e.do(t, "GET", pHost(id), "/", own(id, cookie), ""), http.StatusServiceUnavailable, codeHostOffline)
}

// closedPort is a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

// A streamed response reaches the browser as the app writes it: the first
// chunk arrives while the app is still holding the rest (red with a
// buffering proxy). The app declares its Content-Length: ReverseProxy flushes
// a body of unknown length at once by itself, so only a known-length body
// shows whether the proxy flushes every write.
func TestAStreamedResponseIsNotBuffered(t *testing.T) {
	e := newGW(t, gwOpts{})
	release := make(chan struct{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4177, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len("first-chunk-rest")))
		_, _ = io.WriteString(w, "first-chunk")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "-rest")
	}))
	defer close(release)
	buf := make([]byte, len("first-chunk"))
	got := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest("GET", e.public.URL+"/stream", nil)
		req.Host = pHost(id)
		req.Header = own(id, cookie)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			got <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.ReadFull(resp.Body, buf)
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil || string(buf) != "first-chunk" {
			t.Fatalf("first chunk %q: %v", buf, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first chunk did not arrive while the app held the rest: the proxy buffers")
	}
}

// SSE: the first event arrives before the app ends the stream; a quiet
// stream gets a comment line after the keepalive interval, ONLY at an event
// boundary — an app holding an event half-written for several intervals gets
// nothing injected inside it.
func TestSSEFlushesAndKeepsAliveOnlyAtEventBoundaries(t *testing.T) {
	e := newGW(t, gwOpts{keepalive: 40 * time.Millisecond})
	step := make(chan string)
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4178, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case s, ok := <-step:
				if !ok {
					return
				}
				_, _ = io.WriteString(w, s)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	resp := e.send(t, context.Background(), "GET", pHost(id), "/events", own(id, cookie), nil)
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readUntil := func(want string) string {
		t.Helper()
		var got strings.Builder
		done := make(chan error, 1)
		go func() {
			for !strings.HasSuffix(got.String(), want) {
				b, err := br.ReadByte()
				if err != nil {
					done <- err
					return
				}
				got.WriteByte(b)
			}
			done <- nil
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reading for %q: %v (got %q)", want, err, got.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%q did not arrive", want)
		}
		return got.String()
	}
	step <- "data: one\n\n"
	if got := readUntil("data: one\n\n"); got != "data: one\n\n" && got != ":\n\ndata: one\n\n" {
		t.Fatalf("first event: %q", got)
	}
	// Quiet at a boundary: a comment within a few intervals.
	if got := readUntil(":\n\n"); strings.Trim(got, ":\n") != "" {
		t.Fatalf("before the comment: %q; want only comments", got)
	}
	// Half an event, held for ~8 intervals: nothing may land inside it.
	step <- "data: tw"
	time.Sleep(320 * time.Millisecond)
	step <- "o\n\n"
	got := readUntil("o\n\n")
	if i := strings.Index(got, "data: tw"); i < 0 || strings.Contains(got[i:], ":\n\n") || !strings.HasSuffix(got, "data: two\n\n") {
		t.Fatalf("a comment was injected inside an event: %q", got)
	}
	close(step)
}

// 🔴 Paused readers neither buffer nor stall: three proxied 16 MiB downloads
// whose browsers stop reading after the headers hold the app back (it has
// written far less than it was asked to — the proxy is not reading the app
// into memory), while an unrelated GET and a WebSocket echo on the same
// tunnel still complete; resumed, each download arrives byte-exact.
func TestPausedReadersNeitherBufferNorStall(t *testing.T) {
	const size = 16 << 20
	e := newGW(t, gwOpts{})
	var written [3]atomic.Int64
	var rc recorder
	echoWS := wsEchoApp(t, nil)
	e.serve(4180, echoWS.port)
	fh, id, cookie, _ := e.admitted(t, 4179, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/small" {
			_, _ = io.WriteString(w, "small")
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		w.Header().Set("Content-Length", strconv.Itoa(size))
		chunk := bytes.Repeat([]byte{byte('a' + n)}, 64<<10)
		for sent := 0; sent < size; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			written[n].Add(int64(len(chunk)))
		}
	}))
	var paused []*http.Response
	t.Cleanup(func() { // before the servers' cleanups, which wait for these connections
		for _, r := range paused {
			r.Body.Close()
		}
	})
	for n := range 3 {
		resp := e.send(t, context.Background(), "GET", pHost(id), "/big?n="+strconv.Itoa(n), own(id, cookie), nil)
		paused = append(paused, resp)
	}
	time.Sleep(300 * time.Millisecond)
	for n := range 3 {
		if w := written[n].Load(); w >= size {
			t.Fatalf("download %d: the app wrote all %d bytes to a paused reader: the proxy buffered it", n, w)
		}
	}
	if a := e.do(t, "GET", pHost(id), "/small", own(id, cookie), ""); a.status != 200 || a.body != "small" {
		t.Fatalf("an unrelated GET beside 3 paused readers: HTTP %d %q", a.status, a.body)
	}
	// The echo app is a second preview (another port) on the same tunnel.
	e.relist(t, fh, 1, "alpha", wire.Listener{Port: 4179, Process: "vite", Window: "@1"},
		wire.Listener{Port: 4180, Process: "node", Window: "@1"})
	wsID, wsCookie, _ := e.admitIn(t, 1, "alpha", "@1", 4180)
	c, _, err := e.wsDial(t, wsID, "/", own(wsID, wsCookie), nil)
	if err != nil {
		t.Fatalf("a WebSocket beside 3 paused readers: %v", err)
	}
	wsEcho(t, c, "ping beside paused readers")
	c.Close(websocket.StatusNormalClosure, "")
	for n, resp := range paused {
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || len(b) != size || bytes.Count(b, []byte{byte('a' + n)}) != size {
			t.Fatalf("download %d resumed: %d bytes, %v; want %d bytes of %q", n, len(b), err, size, 'a'+n)
		}
	}
}

// wsApp is a loopback WebSocket echo app that records each upgrade request.
type wsApp struct {
	port int
	mu   sync.Mutex
	reqs []*http.Request
}

func (a *wsApp) last(t *testing.T) *http.Request {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.reqs) == 0 {
		t.Fatal("the app saw no upgrade")
	}
	return a.reqs[len(a.reqs)-1]
}

func wsEchoApp(t *testing.T, subprotocols []string) *wsApp {
	t.Helper()
	a := &wsApp{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.reqs = append(a.reqs, r.Clone(context.Background()))
		a.mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: subprotocols, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(-1)
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, b); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	a.port = srv.Listener.Addr().(*net.TCPAddr).Port
	return a
}

func wsEcho(t *testing.T, c *websocket.Conn, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatalf("write %q: %v", msg, err)
	}
	typ, b, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(b) != msg {
		t.Fatalf("echo of %q: %v %q %v", msg, typ, b, err)
	}
}

// 🔴 A WebSocket (HMR) works end to end through a real tunnel: text and
// binary messages echo, the app's chosen subprotocol is negotiated, and the
// app sees Host and Origin as localhost:<port>, and no gateway cookie.
func TestAWebSocketEchoesThroughTheTunnel(t *testing.T) {
	e := newGW(t, gwOpts{})
	app := wsEchoApp(t, []string{"vite-hmr"})
	_, id, cookie, _ := e.admitted(t, 4181, app.port)
	h := own(id, cookie)
	h.Set("Cookie", OwnerCookie+"="+cookie+"; hmr=1")
	c, resp, err := e.wsDial(t, id, "/hmr?token=x", h, &websocket.DialOptions{Subprotocols: []string{"vite-hmr"}})
	if err != nil {
		t.Fatalf("dial: %v (HTTP %d)", err, statusOf(resp))
	}
	defer c.CloseNow()
	if c.Subprotocol() != "vite-hmr" {
		t.Errorf("subprotocol %q; want vite-hmr", c.Subprotocol())
	}
	wsEcho(t, c, `{"type":"update"}`)
	c.SetReadLimit(-1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bin := bytes.Repeat([]byte{0, 1, 2, 255}, 50000) // 200 KB, past coder/websocket's default read limit
	if err := c.Write(ctx, websocket.MessageBinary, bin); err != nil {
		t.Fatal(err)
	}
	if typ, b, err := c.Read(ctx); err != nil || typ != websocket.MessageBinary || !bytes.Equal(b, bin) {
		t.Fatalf("binary echo: %v %d bytes %v", typ, len(b), err)
	}
	r := app.last(t)
	if r.Host != "localhost:4181" || r.Header.Get("Origin") != "http://localhost:4181" {
		t.Errorf("the app saw Host %q Origin %q; want localhost:4181 for both", r.Host, r.Header.Get("Origin"))
	}
	if got := r.Header.Get("Cookie"); got != "hmr=1" {
		t.Errorf("the app's upgrade carried Cookie %q; want only hmr=1", got)
	}
	if r.URL.RequestURI() != "/hmr?token=x" {
		t.Errorf("the app's upgrade was for %q", r.URL.RequestURI())
	}
	c.Close(websocket.StatusNormalClosure, "")
}

// The browser leg of a quiet relayed WebSocket is pinged within the
// keepalive interval (the edge cuts idle ones), and the app's messages are
// unchanged by it.
func TestAQuietWebSocketIsPingedOnTheBrowserLeg(t *testing.T) {
	e := newGW(t, gwOpts{keepalive: 50 * time.Millisecond})
	app := wsEchoApp(t, nil)
	_, id, cookie, _ := e.admitted(t, 4182, app.port)
	var pings atomic.Int64
	c, _, err := e.wsDial(t, id, "/", own(id, cookie), &websocket.DialOptions{
		OnPingReceived: func(context.Context, []byte) bool { pings.Add(1); return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	// A reader must run for control frames to be read.
	ctx, cancel := context.WithCancel(context.Background())
	msgs := make(chan string, 4)
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			msgs <- string(b)
		}
	}()
	waitFor(t, "a ping on the quiet browser leg", func() bool { return pings.Load() >= 2 })
	if err := c.Write(context.Background(), websocket.MessageText, []byte("after pings")); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-msgs:
		if m != "after pings" {
			t.Fatalf("the app's message became %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no echo after the pings")
	}
	cancel()
}

// wsClosedWith waits for c to be closed by the gateway and returns the close
// status and reason.
func wsClosedWith(t *testing.T, c *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			return ce.Code, ce.Reason
		}
		if ctx.Err() != nil {
			t.Fatal("the WebSocket was still open 5 s later: the gateway did not close it")
		}
		t.Fatalf("the WebSocket ended without a close frame: %v", err)
	}
}

// 🔴 The owner cookie's expiry reaches an open WebSocket: one opened under a
// cookie stays open (and echoes) through several advances up to 8 h − 1 s on
// the gateway's clock, and is closed (1008, session_expired) once 8 h + 1 s
// have passed.
func TestCookieExpiryClosesAnOpenWebSocket(t *testing.T) {
	e := newGW(t, gwOpts{idle: 11 * time.Hour, ttl: 13 * time.Hour})
	app := wsEchoApp(t, nil)
	_, id, cookie, _ := e.admitted(t, 4183, app.port)
	c, _, err := e.wsDial(t, id, "/", own(id, cookie), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	for _, d := range []time.Duration{3 * time.Hour, 3 * time.Hour, 2*time.Hour - time.Second} { // literal: 8h−1s in all
		e.clock.Advance(d)
		wsEcho(t, c, "still open")
	}
	e.clock.Advance(2 * time.Second)
	if code, reason := wsClosedWith(t, c); code != websocket.StatusPolicyViolation || reason != codeSessionExpired {
		t.Fatalf("at 8h+1s: closed %d %q; want 1008 %s", code, reason, codeSessionExpired)
	}
}

// 🔴 Close on the session page cuts the preview at once: its open WebSocket
// is closed (1008, preview_closed), a response in flight ends, the cookie no
// longer admits (404), and the host is told. Another window's page cannot
// close it.
func TestCloseOnTheSessionPageCutsAtOnce(t *testing.T) {
	e := newGW(t, gwOpts{})
	app := wsEchoApp(t, nil)
	e.serve(4185, app.port)
	hold := make(chan struct{})
	defer close(hold)
	var rc recorder
	fh, id, cookie, tok := e.admitted(t, 4184, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	e.relist(t, fh, 1, "alpha", wire.Listener{Port: 4184, Process: "vite", Window: "@1"},
		wire.Listener{Port: 4185, Process: "node", Window: "@1"})
	wsID, wsCookie, _ := e.admitIn(t, 1, "alpha", "@1", 4185)
	c, _, err := e.wsDial(t, wsID, "/", own(wsID, wsCookie), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	wsEcho(t, c, "before close")
	resp := e.send(t, context.Background(), "GET", pHost(id), "/stream", own(id, cookie), nil)
	defer resp.Body.Close()
	one := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, one); err != nil {
		t.Fatal(err)
	}
	for len(fh.answers) > 0 {
		<-fh.answers // the opens' notices
	}

	other := e.session(t, "@2")
	if a := e.do(t, "POST", webHost(), closePath, bearer(other), `{"preview":"`+id+`"}`); a.status != http.StatusNotFound {
		t.Fatalf("another window's page closed the preview: HTTP %d %s", a.status, a.body)
	}
	for _, p := range []string{id, wsID} {
		if a := e.do(t, "POST", webHost(), closePath, bearer(tok), `{"preview":"`+p+`"}`); a.status != http.StatusOK {
			t.Fatalf("Close: HTTP %d %s", a.status, a.body)
		}
	}
	if code, reason := wsClosedWith(t, c); code != websocket.StatusPolicyViolation || reason != codePreviewClosed {
		t.Fatalf("the WebSocket after Close: %d %q; want 1008 %s", code, reason, codePreviewClosed)
	}
	ended := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); ended <- err }()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a response in flight survived Close")
	}
	if a := e.do(t, "GET", pHost(id), "/", own(id, cookie), ""); a.status != http.StatusNotFound {
		t.Fatalf("the cookie after Close: HTTP %d; want 404", a.status)
	}
	select {
	case got := <-fh.answers:
		if got.path != wire.ClosedPath || got.a.Preview != id || got.a.Port != 4184 || got.a.RequestID != "" {
			t.Fatalf("the host was told %s %+v", got.path, got.a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the host was not told of the Close")
	}
}

// The app refusing an upgrade is the app's own response.
func TestAnUpgradeTheAppRefusesIsTheAppsAnswer(t *testing.T) {
	e := newGW(t, gwOpts{})
	var rc recorder
	_, id, cookie, _ := e.admitted(t, 4186, rc.start(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no sockets here", http.StatusUpgradeRequired)
	}))
	_, resp, err := e.wsDial(t, id, "/", own(id, cookie), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUpgradeRequired || resp.Header.Get(ErrorCodeHeader) != "" {
		t.Fatalf("an app refusing the upgrade: %v, HTTP %d; want its own 426", err, statusOf(resp))
	}
}
