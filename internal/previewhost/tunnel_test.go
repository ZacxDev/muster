package previewhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/tunnel"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

// ---------------------------------------------------------------- harness

// fakeGateway is the gateway end of the tunnel, built on G1's tunnel client:
// it accepts the host's WebSocket on wire.HostPath (or refuses it while
// refuse is set), opens the offer stream with its boot id, and hands each
// tunnel to the test.
type fakeGateway struct {
	t       *testing.T
	srv     *httptest.Server
	url     string
	boot    atomic.Value // string
	refuse  atomic.Bool
	accepts atomic.Int32
	opts    tunnel.ClientOptions
	tunnels chan *gwTunnel
	hdrs    chan http.Header
}

// gwTunnel is one accepted tunnel: its client and its offer stream's lines.
type gwTunnel struct {
	t           *testing.T
	c           *tunnel.Client
	ctx         context.Context
	lines       chan []byte
	cancel      context.CancelFunc
	offerCancel context.CancelFunc
}

// startOffer opens an offer stream (GET wire.OfferPath with boot) on the
// tunnel, its lines into a fresh gt.lines.
func (gt *gwTunnel) startOffer(boot string) {
	octx, ocancel := context.WithCancel(gt.ctx)
	gt.lines, gt.offerCancel = make(chan []byte, 256), ocancel
	lines := gt.lines
	req, _ := http.NewRequestWithContext(octx, http.MethodGet, wire.OfferPath, nil)
	req.Header.Set(wire.GatewayBootHeader, boot)
	go func() {
		resp, err := gt.c.RoundTrip(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
}

// openOffer is another offer stream on the same tunnel, as its own view.
func (gt *gwTunnel) openOffer(boot string) *gwTunnel {
	n := &gwTunnel{t: gt.t, c: gt.c, ctx: gt.ctx, cancel: gt.cancel}
	n.startOffer(boot)
	return n
}

func newFakeGateway(t *testing.T, boot string) *fakeGateway {
	t.Helper()
	g := &fakeGateway{t: t, tunnels: make(chan *gwTunnel, 8), hdrs: make(chan http.Header, 8), opts: tunnel.ClientOptions{DialWait: 300 * time.Millisecond}}
	g.boot.Store(boot)
	ctx, cancel := context.WithCancel(context.Background())
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.HostPath {
			http.NotFound(w, r)
			return
		}
		if g.refuse.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		g.accepts.Add(1)
		select {
		case g.hdrs <- r.Header.Clone():
		default:
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		tctx, tcancel := context.WithCancel(ctx)
		c, err := tunnel.NewClient(tctx, ws, g.opts)
		if err != nil {
			tcancel()
			t.Errorf("NewClient: %v", err)
			return
		}
		gt := &gwTunnel{t: t, c: c, ctx: tctx, cancel: func() { c.Close(); tcancel() }}
		gt.startOffer(g.boot.Load().(string))
		g.tunnels <- gt
	}))
	g.url = "ws" + strings.TrimPrefix(g.srv.URL, "http") + wire.HostPath
	t.Cleanup(func() { cancel(); g.srv.Close() })
	return g
}

// tunnel waits for the next tunnel the host opens.
func (g *fakeGateway) next(within time.Duration) *gwTunnel {
	g.t.Helper()
	select {
	case gt := <-g.tunnels:
		g.t.Cleanup(gt.cancel)
		return gt
	case <-time.After(within):
		g.t.Fatalf("no tunnel reached the gateway at %s within %v", g.url, within)
	}
	return nil
}

// offer reads the next offer line, raw and decoded.
func (gt *gwTunnel) offer(within time.Duration) ([]byte, wire.Offer) {
	gt.t.Helper()
	select {
	case line := <-gt.lines:
		return gt.decode(line)
	case <-time.After(within):
		gt.t.Fatalf("no offer line within %v", within)
	}
	return nil, wire.Offer{}
}

// decode is one offer line, decoded.
func (gt *gwTunnel) decode(line []byte) ([]byte, wire.Offer) {
	gt.t.Helper()
	m, err := wire.NewDecoder(bytes.NewReader(line)).Next()
	if err != nil {
		gt.t.Fatalf("bad offer line %q: %v", line, err)
	}
	return line, m
}

// offerOf reads lines until one of type typ.
func (gt *gwTunnel) offerOf(typ string, within time.Duration) ([]byte, wire.Offer) {
	gt.t.Helper()
	deadline := time.Now().Add(within)
	for {
		line, m := gt.offer(time.Until(deadline))
		if m.Type == typ {
			return line, m
		}
	}
}

// quiet asserts no offer line arrives for d.
func (gt *gwTunnel) quiet(d time.Duration) {
	gt.t.Helper()
	select {
	case line := <-gt.lines:
		gt.t.Fatalf("unexpected offer line %q", line)
	case <-time.After(d):
	}
}

// answer POSTs a to path (wire.OpenedPath or wire.ClosedPath).
func (gt *gwTunnel) answer(path string, a wire.Answer) {
	gt.t.Helper()
	if st := gt.answerStatus(path, a); st != http.StatusNoContent {
		gt.t.Fatalf("POST %s: %d", path, st)
	}
}

// answerStatus POSTs a to path and returns the host's status.
func (gt *gwTunnel) answerStatus(path string, a wire.Answer) int {
	gt.t.Helper()
	b, _ := json.Marshal(a)
	req, _ := http.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	resp, err := gt.c.RoundTrip(req)
	if err != nil {
		gt.t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// connectRaw sends a CONNECT with the given authority (not the gateway's
// usual 127.0.0.1:<port>) and returns the status and, on 200, the stream.
func (gt *gwTunnel) connectRaw(authority string) (int, wire.Reason, io.ReadWriteCloser) {
	gt.t.Helper()
	pr, pw := io.Pipe()
	req := (&http.Request{Method: http.MethodConnect, URL: &url.URL{Host: authority}, Host: authority,
		Header: http.Header{}, Body: pr, ContentLength: -1, Proto: "HTTP/2.0", ProtoMajor: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	gt.t.Cleanup(cancel)
	resp, err := gt.c.RoundTrip(req.WithContext(ctx))
	if err != nil {
		gt.t.Fatalf("CONNECT %s: %v", authority, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return resp.StatusCode, wire.RefusalOf(resp.StatusCode, resp.Header), nil
	}
	return 200, "", struct {
		io.Reader
		io.Writer
		io.Closer
	}{resp.Body, pw, closerFunc(func() error { pw.Close(); return resp.Body.Close() })}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// echoServer listens on network/addr and echoes every connection.
func echoServer(t *testing.T, addr string) (int, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, &n
}

// echoes writes msg on rw and reads it back.
func echoes(t *testing.T, rw io.ReadWriter, msg string) {
	t.Helper()
	if _, err := rw.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(rw, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo: %q, %v; want %q", buf, err, msg)
	}
}

// syncLog is a logger the test reads while the host writes.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// hostRig is a Host running against one or more gateways.
type hostRig struct {
	h    *Host
	file string
	log  *syncLog
	stop func()
}

// writePreviews writes previews.json and returns it parsed.
func writePreviews(t *testing.T, file, body string) Config {
	t.Helper()
	// Write-then-rename: the poll never reads a half-written file.
	tmp := file + ".tmp"
	must(t, os.WriteFile(tmp, []byte(body), 0o600))
	must(t, os.Rename(tmp, file))
	c, err := Load(file)
	if err != nil {
		t.Fatalf("previews.json %s: %v", body, err)
	}
	return c
}

// startHost runs a Host whose previews.json is body, with hc's other fields
// and tc's endpoints. Proc defaults to the real /proc.
func startHost(t *testing.T, body string, hc HostConfig, tc TunnelConfig) *hostRig {
	t.Helper()
	file := filepath.Join(t.TempDir(), FileName)
	hc.Config = writePreviews(t, file, body)
	hc.File = file
	if hc.UID == 0 {
		hc.UID = os.Getuid()
	}
	if hc.Procs == nil {
		hc.Procs = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	}
	if hc.Poll == 0 {
		hc.Poll = 50 * time.Millisecond
	}
	lg := &syncLog{}
	hc.Logger = log.New(lg, "", 0)
	h := NewHost(hc)
	if tc.Token == "" {
		tc.Token = "mpt_" + strings.Repeat("A", 43)
	}
	if tc.HostName == "" {
		tc.HostName = "test-host"
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx, tc) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("Run did not return after its context ended")
		}
	}
	t.Cleanup(stop)
	return &hostRig{h: h, file: file, log: lg, stop: stop}
}

func portsJSON(ports ...int) string {
	q := make([]string, len(ports))
	for i, p := range ports {
		q[i] = strconv.Quote(strconv.Itoa(p))
	}
	return `{"ports": [` + strings.Join(q, ", ") + `]}`
}

// ------------------------------------------------------------------ CONNECT

// TestConnectIgnoresTheRequestedHostPart: a CONNECT whose authority names
// another host (a TEST-NET address, a name) still reaches the LOOPBACK
// listener on that port, because the host takes only the port. A host that
// dialled the authority would answer 502 (or hang on the TEST-NET address).
func TestConnectIgnoresTheRequestedHostPart(t *testing.T) {
	port, accepted := echoServer(t, "127.0.0.1:0")
	gw := newFakeGateway(t, "b1")
	startHost(t, portsJSON(port), HostConfig{}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	for _, host := range []string{"192.0.2.1", "attacker.invalid", "[2001:db8::1]"} {
		auth := host + ":" + strconv.Itoa(port)
		if host[0] == '[' {
			auth = host + ":" + strconv.Itoa(port)
		}
		st, reason, rw := gt.connectRaw(auth)
		if st != 200 {
			t.Fatalf("CONNECT %s: %d %s; want 200 to the loopback listener", auth, st, reason)
		}
		echoes(t, rw, "hello "+host)
		rw.Close()
	}
	if n := accepted.Load(); n != 3 {
		t.Fatalf("the loopback listener accepted %d connections, want 3", n)
	}
}

// TestConnectFallsBackToIPv6Loopback: a server bound to [::1] only is
// reached: 127.0.0.1 refuses, then ::1 answers.
func TestConnectFallsBackToIPv6Loopback(t *testing.T) {
	port, accepted := echoServer(t, "[::1]:0")
	gw := newFakeGateway(t, "b1")
	startHost(t, portsJSON(port), HostConfig{}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	c, err := gt.c.Dial(context.Background(), port)
	if err != nil {
		t.Fatalf("Dial :%d with the server on [::1] only: %v", port, err)
	}
	defer c.Close()
	echoes(t, c, "over ::1")
	if accepted.Load() != 1 {
		t.Fatalf("the [::1] listener accepted %d", accepted.Load())
	}
}

// TestMaxConnsPlusOneIs429: with "maxConns": 2, a gateway that believes in
// more slots gets 429 max_conns for the third concurrent stream: the host's
// tunnel is served with previews.json's maxConns, not the default 64.
func TestMaxConnsPlusOneIs429(t *testing.T) {
	port, _ := echoServer(t, "127.0.0.1:0")
	gw := newFakeGateway(t, "b1")
	startHost(t, fmt.Sprintf(`{"ports": ["%d"], "maxConns": 2}`, port), HostConfig{}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	for i := 0; i < 2; i++ {
		c, err := gt.c.Dial(context.Background(), port)
		if err != nil {
			t.Fatalf("stream %d of 2: %v", i+1, err)
		}
		defer c.Close()
		echoes(t, c, "x")
	}
	c, err := gt.c.Dial(context.Background(), port)
	var re *tunnel.RefusedError
	if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || re.Reason != wire.ReasonMaxConns {
		if c != nil {
			c.Close()
		}
		t.Fatalf("stream 3 of maxConns 2: %v; want 429 max_conns", err)
	}
}

// ------------------------------------------------------------------- dial

// blackhole accepts TCP and never answers: a mesh endpoint whose relay is up
// but whose gateway is not.
func blackhole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				mu.Lock()
				for _, c := range held {
					c.Close()
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	return "ws://" + ln.Addr().String() + wire.HostPath
}

// TestADeadMeshEndpointFallsBackWithinTheBudget: a mesh endpoint that takes
// the TCP connection and never answers the upgrade is abandoned within the
// 3 s budget, and the public endpoint carries the tunnel by 3.5 s. The dial
// sends the token, the version and the host name.
func TestADeadMeshEndpointFallsBackWithinTheBudget(t *testing.T) {
	mesh := blackhole(t)
	pub := newFakeGateway(t, "b1")
	start := time.Now()
	rig := startHost(t, portsJSON(5173), HostConfig{}, TunnelConfig{URLs: []string{mesh, pub.url}, HostName: "laptop"})
	pub.next(8 * time.Second) // generous: the timing assertion below is the check
	if took := time.Since(start); took > 3500*time.Millisecond {
		t.Fatalf("the public endpoint carried the tunnel after %v; want ≤ 3.5 s", took)
	}
	hdr := <-pub.hdrs
	if hdr.Get("Authorization") != "Bearer mpt_"+strings.Repeat("A", 43) || hdr.Get(wire.VersionHeader) != "1" || hdr.Get(wire.HostNameHeader) != "laptop" {
		t.Fatalf("dial headers %v", hdr)
	}
	if strings.Contains(rig.log.String(), "mpt_") {
		t.Fatalf("the log prints the token:\n%s", rig.log.String())
	}
}

// TestTheDialNeverFollowsARedirect: an endpoint that answers the upgrade with
// a redirect is a failed dial — the Location never receives a request (it
// would get the host token) — and the host goes on to its next endpoint.
func TestTheDialNeverFollowsARedirect(t *testing.T) {
	var reached atomic.Int32
	loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer loc.Close()
	var redirects atomic.Int32
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects.Add(1)
		http.Redirect(w, r, loc.URL+wire.HostPath, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	pub := newFakeGateway(t, "b1")
	startHost(t, portsJSON(5173), HostConfig{}, TunnelConfig{URLs: []string{"ws" + strings.TrimPrefix(redir.URL, "http") + wire.HostPath, pub.url}})
	pub.next(5 * time.Second)
	if redirects.Load() == 0 {
		t.Fatal("instrument: the redirecting endpoint was never dialled")
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the redirect's Location received %d request(s)", n)
	}
}

// fakeTimers is TunnelConfig.after for the tests: a wait of exactly `of`
// returns a channel the test fires; every other wait is real.
type fakeTimers struct {
	of    time.Duration
	asked chan time.Duration
	fire  chan time.Time
}

func newFakeTimers(of time.Duration) *fakeTimers {
	return &fakeTimers{of: of, asked: make(chan time.Duration, 16), fire: make(chan time.Time)}
}

func (f *fakeTimers) after(d time.Duration) <-chan time.Time {
	if d != f.of {
		return time.After(d)
	}
	f.asked <- d
	ch := make(chan time.Time, 1)
	go func() { ch <- <-f.fire }()
	return ch
}

// TestAReturningMeshIsTakenAtTheNextTick: a host on the public endpoint
// (the mesh refusing) waits DefaultMeshRetry — 5 minutes, on a test clock —
// and when that tick fires with the mesh back, moves its tunnel there; not
// before the tick.
func TestAReturningMeshIsTakenAtTheNextTick(t *testing.T) {
	mesh := newFakeGateway(t, "b1")
	mesh.refuse.Store(true)
	pub := newFakeGateway(t, "b1")
	timers := newFakeTimers(5 * time.Minute)
	tc := TunnelConfig{URLs: []string{mesh.url, pub.url}, after: timers.after}
	startHost(t, portsJSON(5173), HostConfig{}, tc)
	pt := pub.next(5 * time.Second)
	select {
	case d := <-timers.asked:
		if d != 5*time.Minute {
			t.Fatalf("mesh retry waits %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the host on the public path never armed its 5-minute mesh retry")
	}
	mesh.refuse.Store(false)
	select {
	case <-mesh.tunnels:
		t.Fatal("the host moved to the mesh before the tick")
	case <-time.After(300 * time.Millisecond):
	}
	if n := mesh.accepts.Load(); n != 0 {
		t.Fatalf("the mesh was dialled %d time(s) before the tick", n)
	}
	timers.fire <- time.Now()
	mesh.next(5 * time.Second)
	// The public tunnel is not cut by the host: it drains until the gateway
	// closes it.
	select {
	case <-pt.c.Done():
		t.Fatal("the host cut the public tunnel instead of letting it drain")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestANewTunnelsDeathRedialsWhileTheOldOneDrains: after the move to the
// mesh, the replaced public tunnel drains for as long as the gateway keeps it
// (the gateway owns its close; this one never closes it). When the NEW tunnel
// dies meanwhile, the host redials at once — it does not wait for the old
// tunnel's drain — and the old one is still draining.
func TestANewTunnelsDeathRedialsWhileTheOldOneDrains(t *testing.T) {
	mesh := newFakeGateway(t, "b1")
	mesh.refuse.Store(true)
	pub := newFakeGateway(t, "b1")
	timers := newFakeTimers(5 * time.Minute)
	startHost(t, portsJSON(5173), HostConfig{}, TunnelConfig{URLs: []string{mesh.url, pub.url}, after: timers.after})
	pt := pub.next(5 * time.Second)
	<-timers.asked
	mesh.refuse.Store(false)
	timers.fire <- time.Now()
	mt := mesh.next(5 * time.Second)
	mt.cancel() // the new tunnel dies; the old one is still up
	select {
	case <-mesh.tunnels:
	case <-time.After(4 * time.Second):
		t.Fatal("the host did not redial within 4 s of its tunnel dying: it is waiting on the replaced tunnel's drain")
	}
	select {
	case <-pt.c.Done():
		t.Fatal("the replaced tunnel was closed by the host; the gateway owns its close")
	default:
	}
}

// ------------------------------------------------------------ offer stream

// windowSource is a HostConfig.Windows the test changes: each read returns
// the current set (in the order set) and counts.
type windowSource struct {
	mu    sync.Mutex
	ws    []Window
	reads atomic.Int32
}

func (w *windowSource) set(ws ...Window) {
	w.mu.Lock()
	w.ws = ws
	w.mu.Unlock()
}

func (w *windowSource) read(context.Context) ([]Window, error) {
	w.reads.Add(1)
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Window(nil), w.ws...), nil
}

// TestTheWindowListingIsSentOnlyOnChange: the host lists the windows itself
// every poll; the listing goes out once when known, never again while the
// set stays the same (whatever order tmux returns it in) — over many polls,
// counted — and again when the set changes.
func TestTheWindowListingIsSentOnlyOnChange(t *testing.T) {
	src := &windowSource{}
	src.set(Window{ID: "@2"}, Window{ID: "@1"})
	gw := newFakeGateway(t, "b1")
	startHost(t, portsJSON(5173), HostConfig{Windows: src.read}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	_, cfg := gt.offer(2 * time.Second)
	if cfg.Type != wire.TypeConfig || cfg.Config.MaxConns != 64 || len(cfg.Config.Ports) != 1 || cfg.Config.Ports[0] != "5173" {
		t.Fatalf("first line %+v; want the config", cfg)
	}
	_, w := gt.offerOf(wire.TypeWindows, 2*time.Second)
	if strings.Join(w.Windows, ",") != "@1,@2" {
		t.Fatalf("windows %v", w.Windows)
	}
	src.set(Window{ID: "@1"}, Window{ID: "@2"}) // same set, other order
	before := src.reads.Load()
	gt.quiet(500 * time.Millisecond) // ~10 polls; ≥ 2 counted even on a loaded CI runner
	if n := src.reads.Load() - before; n < 2 {
		t.Fatalf("instrument: only %d window reads during the quiet window; the poll is not running", n)
	}
	src.set(Window{ID: "@1"}, Window{ID: "@3"})
	_, w = gt.offer(2 * time.Second)
	if w.Type != wire.TypeWindows || strings.Join(w.Windows, ",") != "@1,@3" {
		t.Fatalf("after the change: %+v; want windows @1,@3", w)
	}
}

// TestGatewayBootChangeReannouncesOpens: the opens a gateway acknowledged —
// an agent's (by request) and the owner's click (unsolicited) — are
// re-announced, each naming its old preview id, to a gateway with a NEW boot
// id, and the answers replace the ids. A reconnect to the SAME boot id
// re-announces nothing.
func TestGatewayBootChangeReannouncesOpens(t *testing.T) {
	gw := newFakeGateway(t, "boot-1")
	rig := startHost(t, portsJSON(5173, 6006), HostConfig{}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offer(2 * time.Second) // config
	// The owner clicked an offer: unsolicited.
	gt.answer(wire.OpenedPath, wire.Answer{Preview: "p-click", Window: "@1", Port: 6006})
	// An agent asked.
	type res struct {
		a   wire.Answer
		err error
	}
	got := make(chan res, 1)
	go func() {
		a, err := rig.h.RequestOpen(context.Background(), wire.Request{Window: "@2", Port: 5173, Label: "app", Path: "/x"})
		got <- res{a, err}
	}()
	_, op := gt.offerOf(wire.TypeOpen, 2*time.Second)
	if op.Request.Window != "@2" || op.Request.Port != 5173 || op.Request.Label != "app" || op.Request.ID == "" {
		t.Fatalf("open request %+v", op.Request)
	}
	gt.answer(wire.OpenedPath, wire.Answer{RequestID: op.Request.ID, Preview: "p-agent"})
	if r := <-got; r.err != nil || r.a.Preview != "p-agent" {
		t.Fatalf("RequestOpen: %+v %v", r.a, r.err)
	}
	if o := rig.h.Opens(); len(o) != 2 || o[0].Preview != "p-agent" || o[1].Preview != "p-click" {
		t.Fatalf("opens %+v", o)
	}

	// Same gateway process, tunnel dropped: nothing re-announced.
	gt.cancel()
	gt = gw.next(10 * time.Second)
	gt.offer(2 * time.Second) // config
	gt.quiet(300 * time.Millisecond)

	// A new gateway process.
	gw.boot.Store("boot-2")
	gt.cancel()
	gt = gw.next(10 * time.Second)
	gt.offer(2 * time.Second) // config
	re := map[string]wire.Request{}
	for len(re) < 2 {
		_, m := gt.offerOf(wire.TypeOpen, 2*time.Second)
		re[m.Request.Preview] = *m.Request
	}
	if r := re["p-agent"]; r.Window != "@2" || r.Port != 5173 || r.Label != "app" || r.Path != "/x" {
		t.Fatalf("re-announced agent open %+v", r)
	}
	if r := re["p-click"]; r.Window != "@1" || r.Port != 6006 {
		t.Fatalf("re-announced click open %+v", r)
	}
	gt.answer(wire.OpenedPath, wire.Answer{RequestID: re["p-agent"].ID, Preview: "q-agent"})
	gt.answer(wire.OpenedPath, wire.Answer{RequestID: re["p-click"].ID, Preview: "q-click"})
	if o := rig.h.Opens(); len(o) != 2 || o[0].Preview != "q-agent" || o[1].Preview != "q-click" || o[0].Label != "app" {
		t.Fatalf("opens after re-announce %+v", o)
	}
}

// TestRequestsWithoutAGatewayFailFast: with no offer stream an open is
// gateway_not_connected at once; with one that never answers it is
// gateway_timeout after AnswerWait.
func TestRequestsWithoutAGatewayFailFast(t *testing.T) {
	h := NewHost(HostConfig{AnswerWait: 200 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if _, err := h.RequestOpen(context.Background(), wire.Request{Window: "@1", Port: 5173}); !errors.Is(err, ErrGatewayNotConnected) {
		t.Fatalf("no gateway: %v", err)
	}
	gw := newFakeGateway(t, "b1")
	rig := startHost(t, portsJSON(5173), HostConfig{AnswerWait: 200 * time.Millisecond}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offer(2 * time.Second)
	start := time.Now()
	if _, err := rig.h.RequestOpen(context.Background(), wire.Request{Window: "@1", Port: 5173}); !errors.Is(err, ErrGatewayTimeout) || time.Since(start) > 2*time.Second {
		t.Fatalf("silent gateway: %v after %v", err, time.Since(start))
	}
}

// TestAPortRemovedFromTheAllowlistClosesItsPreviews: rewriting previews.json
// without a port sends the new config, a close for that port's preview (and
// not the other's), ends its live connection, and refuses new ones; the
// other port's preview stays.
func TestAPortRemovedFromTheAllowlistClosesItsPreviews(t *testing.T) {
	a, _ := echoServer(t, "127.0.0.1:0")
	b, _ := echoServer(t, "127.0.0.1:0")
	gw := newFakeGateway(t, "b1")
	rig := startHost(t, portsJSON(a, b), HostConfig{}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offer(2 * time.Second)
	gt.answer(wire.OpenedPath, wire.Answer{Preview: "pa", Window: "@1", Port: a})
	gt.answer(wire.OpenedPath, wire.Answer{Preview: "pb", Window: "@1", Port: b})
	live, err := gt.c.Dial(context.Background(), a)
	must(t, err)
	defer live.Close()
	echoes(t, live, "before")

	writePreviews(t, rig.file, portsJSON(b))
	_, cfg := gt.offerOf(wire.TypeConfig, 3*time.Second)
	if strings.Join(cfg.Config.Ports, ",") != strconv.Itoa(b) {
		t.Fatalf("new config %+v", cfg.Config)
	}
	var cl wire.Offer
	select {
	case line := <-gt.lines:
		_, cl = gt.decode(line)
	case <-time.After(2 * time.Second):
		t.Fatal("after the config: no close of pa")
	}
	if cl.Type != wire.TypeClose || cl.Request.Preview != "pa" || cl.Request.Port != a {
		t.Fatalf("after the config: %+v; want a close of pa", cl)
	}
	gt.quiet(300 * time.Millisecond) // and no close of pb
	if o := rig.h.Opens(); len(o) != 1 || o[0].Preview != "pb" {
		t.Fatalf("opens %+v; want pb only", o)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := live.Read(make([]byte, 1))
		ended <- err
	}()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the live connection read data, want its end")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the live connection on the removed port stayed open")
	}
	if st, reason, _ := gt.connectRaw(wire.ConnectAuthority(a)); st != 403 || reason != wire.ReasonPortNotAllowed {
		t.Fatalf("new connection to the removed port: %d %q", st, reason)
	}
}

// --------------------------------------------------------- audit round 1

// TestTheGatewayCannotMakeTheHostHoldMoreThanMaxOpen: an unsolicited
// "opened" is recorded only when the host would serve it — at most maxOpen
// opens, an allowed port, a well-formed preview id (one DNS label's worth)
// and tmux window id — so 2,000 hostile POSTs leave maxOpen small records,
// and a gateway restart re-announces only those.
func TestTheGatewayCannotMakeTheHostHoldMoreThanMaxOpen(t *testing.T) {
	gw := newFakeGateway(t, "boot-1")
	rig := startHost(t, `{"ports": ["5173"], "maxOpen": 2}`, HostConfig{}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offer(2 * time.Second)
	for _, c := range []struct {
		name string
		a    wire.Answer
		want int
	}{
		// Each malformed case runs while slots are free, so only its own
		// check can refuse it (not maxOpen).
		{"off the allowlist", wire.Answer{Preview: "p1", Window: "@1", Port: 1}, 409},
		{"id longer than a DNS label allows", wire.Answer{Preview: strings.Repeat("a", 62), Window: "@1", Port: 5173}, 409},
		{"id with a dot", wire.Answer{Preview: "p.1", Window: "@1", Port: 5173}, 409},
		{"window not a tmux id", wire.Answer{Preview: "p1", Window: "@1x", Port: 5173}, 409},
		{"window too long", wire.Answer{Preview: "p1", Window: "@" + strings.Repeat("9", 11), Port: 5173}, 409},
		{"body over the answer bound", wire.Answer{Preview: strings.Repeat("a", 5000), Window: "@1", Port: 5173}, 400},
		{"the longest id allowed (control)", wire.Answer{Preview: strings.Repeat("a", 61), Window: "@1234567890", Port: 5173}, 204},
		{"first", wire.Answer{Preview: "p1", Window: "@1", Port: 5173}, 204},
		{"past maxOpen", wire.Answer{Preview: "p3", Window: "@1", Port: 5173}, 409},
		{"the same id again (not a new open)", wire.Answer{Preview: "p1", Window: "@2", Port: 5173}, 204},
	} {
		if st := gt.answerStatus(wire.OpenedPath, c.a); st != c.want {
			t.Errorf("%s: %d, want %d", c.name, st, c.want)
		}
	}
	for i := 0; i < 2000; i++ {
		gt.answerStatus(wire.OpenedPath, wire.Answer{Preview: fmt.Sprintf("h%d-%s", i, strings.Repeat("x", 40)), Window: "@1", Port: 5173})
	}
	opens := rig.h.Opens()
	if len(opens) != 2 || opens[0].Preview != strings.Repeat("a", 61) || opens[1].Preview != "p1" {
		t.Fatalf("after 2,000 hostile opens the host holds %d: %+v", len(opens), opens)
	}
	// A restart re-announces the two, nothing more.
	gw.boot.Store("boot-2")
	gt.cancel()
	gt = gw.next(10 * time.Second)
	gt.offer(2 * time.Second)
	for i := 0; i < 2; i++ {
		gt.offerOf(wire.TypeOpen, 2*time.Second)
	}
	// No third re-announce. (Closes of the refused opens may still arrive:
	// the host closes every open it would not keep.)
	quiet := time.After(300 * time.Millisecond)
	for {
		select {
		case line := <-gt.lines:
			if _, m := gt.decode(line); m.Type == wire.TypeOpen {
				t.Fatalf("a re-announce past the 2 kept opens: %s", line)
			}
		case <-quiet:
			return
		}
	}
}

// testClock is a HostConfig.Now the test moves.
type testClock struct{ off atomic.Int64 }

func (c *testClock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *testClock) advance(d time.Duration) { c.off.Add(int64(d)) }

// TestReannouncedOpensFollowTheGatewaysAnswer: what the host chose for opens
// across a gateway restart or a long disconnect. The restarted gateway's
// answer is authoritative — an opened answer keeps the open (its original
// open time too), a closed answer drops it, and no answer within AnswerWait
// drops it — and an open older than MaxOpenAge (24 h; also what a disconnect
// longer than a day leaves) is dropped, not re-announced.
func TestReannouncedOpensFollowTheGatewaysAnswer(t *testing.T) {
	clock := &testClock{}
	gw := newFakeGateway(t, "boot-1")
	rig := startHost(t, portsJSON(5173), HostConfig{AnswerWait: 300 * time.Millisecond, Now: clock.now}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offer(2 * time.Second)
	for _, id := range []string{"keep", "closed", "silent"} {
		gt.answer(wire.OpenedPath, wire.Answer{Preview: id, Window: "@1", Port: 5173})
	}
	var opened time.Time
	for _, o := range rig.h.Opens() {
		if o.Preview == "keep" {
			opened = o.Opened
		}
	}
	restart := func(boot string) *gwTunnel {
		gw.boot.Store(boot)
		gt.cancel()
		gt = gw.next(10 * time.Second)
		gt.offer(2 * time.Second)
		return gt
	}
	gt = restart("boot-2")
	re := map[string]string{}
	for len(re) < 3 {
		_, m := gt.offerOf(wire.TypeOpen, 2*time.Second)
		re[m.Request.Preview] = m.Request.ID
	}
	gt.answer(wire.OpenedPath, wire.Answer{RequestID: re["keep"], Preview: "keep2"})
	// A closed answer drops the old open whatever id it names.
	gt.answer(wire.ClosedPath, wire.Answer{RequestID: re["closed"], Preview: "closed-new"})
	// "silent" gets no answer: gone after AnswerWait.
	deadline := time.Now().Add(3 * time.Second)
	for len(rig.h.Opens()) != 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	o := rig.h.Opens()
	if len(o) != 1 || o[0].Preview != "keep2" || !o[0].Opened.Equal(opened) {
		t.Fatalf("after the restart: %+v; want keep2 alone, opened at %v", o, opened)
	}
	rig.h.mu.Lock()
	leaked := len(rig.h.pending)
	rig.h.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("%d request(s) still pending after their answers and expiry", leaked)
	}
	// A day later (a long disconnect, or a gateway that keeps restarting):
	// not re-announced, dropped.
	clock.advance(MaxOpenAge)
	gt = restart("boot-3")
	gt.quiet(400 * time.Millisecond)
	if o := rig.h.Opens(); len(o) != 0 {
		t.Fatalf("an open older than MaxOpenAge survived: %+v", o)
	}
}

// TestAnOpenedTheHostDoesNotKeepIsClosedAgain: an "opened" the host will
// not keep is answered 409 AND closed on the offer stream, so the gateway
// keeps no preview the host has forgotten — a late answer to a request the
// agent gave up on (gateway_timeout), an answer refused by maxOpen, and an
// answer to no request the host knows. The abandoned request is held for
// lateGrace only; maxPending agent requests in flight refuse the next at once.
func TestAnOpenedTheHostDoesNotKeepIsClosedAgain(t *testing.T) {
	gw := newFakeGateway(t, "b1")
	// AnswerWait 1 s: long enough that the "refused" request below is still
	// pending (not expired, which would make it an answer to an unknown
	// request) on a loaded machine. Poll 1 h: no periodic wake-up, so each
	// close must be delivered by its own (queueCloseLocked's broadcast).
	rig := startHost(t, `{"ports": ["5173"], "maxOpen": 1}`, HostConfig{AnswerWait: time.Second, Poll: time.Hour}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offer(2 * time.Second)
	expectClose := func(what, preview, window string) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case line := <-gt.lines:
				if _, m := gt.decode(line); m.Type == wire.TypeClose {
					if m.Request.Preview != preview || m.Request.Window != window {
						t.Fatalf("%s: close %+v; want one of %s in %s", what, m.Request, preview, window)
					}
					return
				}
			case <-deadline:
				t.Fatalf("%s: no close of %s was sent: the gateway keeps a preview the host forgot", what, preview)
			}
		}
	}

	// Late: the agent was told gateway_timeout before the answer came.
	if _, err := rig.h.RequestOpen(context.Background(), wire.Request{Window: "@4", Port: 5173}); !errors.Is(err, ErrGatewayTimeout) {
		t.Fatalf("silent gateway: %v", err)
	}
	_, m := gt.offerOf(wire.TypeOpen, time.Second)
	if st := gt.answerStatus(wire.OpenedPath, wire.Answer{RequestID: m.Request.ID, Preview: "late"}); st != http.StatusConflict {
		t.Fatalf("late opened: %d, want 409", st)
	}
	expectClose("late", "late", "@4")
	if o := rig.h.Opens(); len(o) != 0 {
		t.Fatalf("a late answer was recorded: %+v", o)
	}

	// Refused: maxOpen 1 is taken — an owner's click past it, and an
	// agent's open answered anyway.
	gt.answer(wire.OpenedPath, wire.Answer{Preview: "taken", Window: "@1", Port: 5173})
	if st := gt.answerStatus(wire.OpenedPath, wire.Answer{Preview: "extra", Window: "@7", Port: 5173}); st != http.StatusConflict {
		t.Fatalf("unsolicited past maxOpen: %d, want 409", st)
	}
	expectClose("unsolicited refused", "extra", "@7")
	res := make(chan wire.Answer, 1)
	go func() {
		a, _ := rig.h.RequestOpen(context.Background(), wire.Request{Window: "@5", Port: 5173})
		res <- a
	}()
	_, m = gt.offerOf(wire.TypeOpen, time.Second)
	if st := gt.answerStatus(wire.OpenedPath, wire.Answer{RequestID: m.Request.ID, Preview: "over"}); st != http.StatusConflict {
		t.Fatalf("refused opened: %d, want 409", st)
	}
	if a := <-res; a.Error != "max_open" {
		t.Fatalf("the agent's answer %+v; want max_open", a)
	}
	expectClose("refused", "over", "@5")

	// Unknown: no such request.
	if st := gt.answerStatus(wire.OpenedPath, wire.Answer{RequestID: "r999", Preview: "ghost", Window: "@6", Port: 5173}); st != http.StatusConflict {
		t.Fatalf("unknown opened: %d, want 409", st)
	}
	expectClose("unknown", "ghost", "@6")
	if o := rig.h.Opens(); len(o) != 1 || o[0].Preview != "taken" {
		t.Fatalf("opens %+v; want taken alone", o)
	}

	// maxPending agent requests in flight: the next fails at once, sends nothing.
	rig.h.mu.Lock()
	for i := 0; i < maxPending; i++ {
		rig.h.pending[fmt.Sprintf("fill%d", i)] = pendingReq{deadline: time.Now().Add(time.Hour)}
	}
	rig.h.mu.Unlock()
	start := time.Now()
	if _, err := rig.h.RequestOpen(context.Background(), wire.Request{Window: "@1", Port: 5173}); !errors.Is(err, ErrGatewayTimeout) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("at maxPending: %v after %v; want gateway_timeout at once", err, time.Since(start))
	}
	gt.quiet(200 * time.Millisecond)
}

// TestRepeatedBootIDsDoNotGrowPending: GETs of the offer stream with a new
// boot id each, over and over, re-announce each open at most once at a time:
// pending stays at most the opens (not opens × GETs), and an agent request
// still gets through.
func TestRepeatedBootIDsDoNotGrowPending(t *testing.T) {
	gw := newFakeGateway(t, "boot-0")
	rig := startHost(t, portsJSON(5173), HostConfig{}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offer(2 * time.Second)
	gt.answer(wire.OpenedPath, wire.Answer{Preview: "p1", Window: "@1", Port: 5173})
	gt.answer(wire.OpenedPath, wire.Answer{Preview: "p2", Window: "@1", Port: 5173})
	for i := 0; i < 300; i++ {
		req, _ := http.NewRequest(http.MethodGet, wire.OfferPath, nil)
		req.Header.Set(wire.GatewayBootHeader, fmt.Sprintf("storm-%d", i))
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := gt.c.RoundTrip(req.WithContext(ctx))
		if err == nil {
			resp.Body.Close()
		}
		cancel()
	}
	rig.h.mu.Lock()
	n := len(rig.h.pending)
	rig.h.mu.Unlock()
	if n > 2 {
		t.Fatalf("%d requests pending after 300 boot-id GETs; want at most the 2 opens", n)
	}
	// A fresh stream still carries an agent request and its answer.
	st := gt.openOffer("storm-299")
	st.offer(2 * time.Second) // its config: it is the current stream now
	deadline := time.Now().Add(3 * time.Second)
	for {
		rig.h.mu.Lock()
		cur := rig.h.stream != nil
		rig.h.mu.Unlock()
		if cur || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	type res struct {
		a   wire.Answer
		err error
	}
	got := make(chan res, 1)
	go func() {
		a, err := rig.h.RequestOpen(context.Background(), wire.Request{Window: "@2", Port: 5173})
		got <- res{a, err}
	}()
	_, m := st.offerOf(wire.TypeOpen, 3*time.Second)
	for m.Request.Preview != "" { // skip any re-announce
		_, m = st.offerOf(wire.TypeOpen, 3*time.Second)
	}
	st.answer(wire.OpenedPath, wire.Answer{RequestID: m.Request.ID, Preview: "p3"})
	if r := <-got; r.err != nil || r.a.Preview != "p3" {
		t.Fatalf("an agent request after the storm: %+v, %v", r.a, r.err)
	}
}

// TestAPortLeavingTheListBetweenCheckAndDialIsRefused: previews.json drops
// the port after the CONNECT's checks passed but before the upstream is
// spliced (the dial is where the test removes it): the re-check under the
// reload's lock refuses with port_not_allowed instead of splicing a port no
// longer allowed.
func TestAPortLeavingTheListBetweenCheckAndDialIsRefused(t *testing.T) {
	a, _ := echoServer(t, "127.0.0.1:0")
	gw := newFakeGateway(t, "b1")
	var rig *hostRig
	ready := make(chan struct{})
	dial := func(ctx context.Context, port int) (net.Conn, error) {
		<-ready
		writePreviews(t, rig.file, portsJSON(5173))
		rig.h.reload()
		return dialLoopback(ctx, port)
	}
	rig = startHost(t, portsJSON(a, 5173), HostConfig{dial: dial}, TunnelConfig{URLs: []string{gw.url}})
	close(ready)
	gt := gw.next(5 * time.Second)
	if st, reason, rw := gt.connectRaw(wire.ConnectAuthority(a)); st != 403 || reason != wire.ReasonPortNotAllowed {
		if rw != nil {
			rw.Close()
		}
		t.Fatalf("port removed between check and dial: %d %q; want 403 port_not_allowed", st, reason)
	}
}

// TestHostClosesGoOutOnTheCurrentTunnelOnly: with a replaced tunnel still
// draining, a close the host decides (its port left the list) is never
// written on the draining tunnel's offer stream. DETERMINISTIC: the current
// tunnel's stream is closed first, so when the close is queued the draining
// stream is the ONLY one awake to take it — it wakes (its config line
// arrives) and must leave the close alone; the current tunnel's next stream
// then carries it.
func TestHostClosesGoOutOnTheCurrentTunnelOnly(t *testing.T) {
	mesh := newFakeGateway(t, "b1")
	mesh.refuse.Store(true)
	pub := newFakeGateway(t, "b1")
	timers := newFakeTimers(5 * time.Minute)
	rig := startHost(t, portsJSON(5173, 6006), HostConfig{}, TunnelConfig{URLs: []string{mesh.url, pub.url}, after: timers.after})
	pt := pub.next(5 * time.Second)
	pt.offer(2 * time.Second)
	<-timers.asked
	mesh.refuse.Store(false)
	timers.fire <- time.Now()
	mt := mesh.next(5 * time.Second)
	mt.offer(2 * time.Second)
	mt.answer(wire.OpenedPath, wire.Answer{Preview: "pa", Window: "@1", Port: 6006})
	mt.offerCancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rig.h.mu.Lock()
		gone := rig.h.stream == nil
		rig.h.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("instrument: the current tunnel's offer stream never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	writePreviews(t, rig.file, portsJSON(5173))
	_, cfg := pt.offerOf(wire.TypeConfig, 3*time.Second) // the draining stream woke
	if strings.Join(cfg.Config.Ports, ",") != "5173" {
		t.Fatalf("config %+v", cfg.Config)
	}
	quiet := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case line := <-pt.lines:
			if _, m := pt.decode(line); m.Type == wire.TypeClose {
				t.Fatalf("the draining tunnel's stream carried the close: %s", line)
			}
		case <-quiet:
			done = true
		}
	}
	nt := mt.openOffer("b1")
	if _, cl := nt.offerOf(wire.TypeClose, 2*time.Second); cl.Request.Preview != "pa" {
		t.Fatalf("on the current tunnel's next stream: %+v; want the close of pa", cl)
	}
}

// TestTheDisabledReasonCarriesNoPath: an invalid previews.json turns previews
// off and the reason reaches the gateway — naming previews.json, never the
// file's absolute path (a home directory, a user name).
func TestTheDisabledReasonCarriesNoPath(t *testing.T) {
	gw := newFakeGateway(t, "b1")
	rig := startHost(t, portsJSON(5173), HostConfig{}, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offer(2 * time.Second)
	must(t, os.WriteFile(rig.file, []byte(`{"ports": ["80"]}`), 0o600))
	_, m := gt.offerOf(wire.TypeConfig, 3*time.Second)
	why := m.Config.Disabled
	if why == "" || !strings.Contains(why, FileName) || strings.Contains(why, filepath.Dir(rig.file)) {
		t.Fatalf("disabled reason %q; want one naming %s without %s", why, FileName, filepath.Dir(rig.file))
	}
}
