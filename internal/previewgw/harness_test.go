package previewgw

import (
	"context"
	"crypto/tls"
	"encoding/json"
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

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/tunnel"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

const testDomain = "preview.example"

// ------------------------------------------------------------ fake muster

// The session page's tests' muster: its public URL (frame-ancestors is its
// ORIGIN, so it carries a port and a path the origin must keep and drop) and
// the gateway's signing key.
const (
	testPublicURL    = "https://muster.example.net:8443/base"
	testPublicOrigin = "https://muster.example.net:8443"
	testSigningKey   = "0123456789abcdef0123456789abcdef-test"
)

// fakeMuster answers GET /plugin/v1/self per token, and POST
// /plugin/v1/handoffs/redeem as muster does: the token must hold viewer
// (else 403), a code is spent once and only by the token it was minted for
// (else 404).
type fakeMuster struct {
	srv      *httptest.Server
	mu       sync.Mutex
	by       map[string]fakeSelf
	handoffs map[string]fakeHandoff // code → minted for
	calls    atomic.Int64
	redeems  atomic.Int64
}

type fakeHandoff struct {
	token string
	body  map[string]any
}

// handoff mints a code in the fake muster for token, as POST
// /plugin-handoffs would for (owner, host, window).
func (f *fakeMuster) handoff(token string, owner int64, host, session, window string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := fmt.Sprintf("code-%d-%s", len(f.handoffs)+1, window)
	f.handoffs[code] = fakeHandoff{token, map[string]any{"owner_id": owner, "purpose": "view", "stepped_up": false,
		"minted_at": "2000-01-02T03:04:05Z",
		"target":    map[string]any{"mirror": map[string]string{"host": host, "session": session, "window": window}}}}
	return code
}

func (f *fakeMuster) redeem(w http.ResponseWriter, r *http.Request) {
	f.redeems.Add(1)
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.by[tok]
	if !ok || a.status != http.StatusOK {
		http.Error(w, "this route takes a plugin token", http.StatusUnauthorized)
		return
	}
	if !a.self.HasViewer() {
		http.Error(w, "this plugin token lacks the viewer capability", http.StatusForbidden)
		return
	}
	var in struct{ Code string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	h, ok := f.handoffs[in.Code]
	if !ok || h.token != tok {
		http.Error(w, `{"error":"no such hand-off code"}`, http.StatusNotFound)
		return
	}
	delete(f.handoffs, in.Code)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.body)
}

type fakeSelf struct {
	status int
	self   Self
}

func newFakeMuster(t *testing.T) *fakeMuster {
	f := &fakeMuster{by: map[string]fakeSelf{}, handoffs: map[string]fakeHandoff{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plugin/v1/handoffs/redeem" && r.Method == http.MethodPost {
			f.redeem(w, r)
			return
		}
		if r.URL.Path != "/plugin/v1/self" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		f.calls.Add(1)
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		a, ok := f.by[tok]
		f.mu.Unlock()
		if !ok || a.status == http.StatusUnauthorized {
			http.Error(w, "this route takes a plugin token", http.StatusUnauthorized)
			return
		}
		if a.status != http.StatusOK {
			http.Error(w, "nope", a.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.self)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMuster) set(tok string, status int, s Self) {
	f.mu.Lock()
	f.by[tok] = fakeSelf{status, s}
	f.mu.Unlock()
}

func viewerSelf(owner int64, login string) Self {
	return Self{PluginID: owner * 10, PluginName: "previews", OwnerID: owner, OwnerLogin: login,
		Capabilities: []string{"messages", "viewer"}}
}

// ------------------------------------------------------------ the gateway

type gwEnv struct {
	g      *Gateway
	clock  *fakeClock
	fm     *fakeMuster
	store  HostTokens
	public *httptest.Server
	mesh   *httptest.Server
	// ports maps a port a fake host is asked to CONNECT to onto the real
	// loopback port that serves it (int → int); an unmapped port is dialled
	// as is. 5173 is mapped to appServer, so the offers the tests make at
	// :5173 are listening, and an admitted request is answered by it.
	ports sync.Map
	// refuse makes a fake host refuse a CONNECT to a port with a reason
	// (int → wire.Reason), as a real host's checks would.
	refuse sync.Map
}

// serve makes a fake host's CONNECT to port reach real instead.
func (e *gwEnv) serve(port, real int) { e.ports.Store(port, real) }

type gwOpts struct {
	tokens    []string // plugin tokens; default {"mpl_owner1"}
	dialWait  time.Duration
	ttl, idle time.Duration // Config.TTL, Config.Idle; 0 = the defaults
	maxBody   int64         // Config.MaxBody; 0 = the default
	keepalive time.Duration // Options.StreamKeepalive; 0 = an hour (off)
	perPrev   int           // Options.MaxConnsPerPreview; 0 = MaxConnsPerPreview
}

// newGW starts a gateway with a fake clock and a fake muster where
// mpl_owner1 is owner 1 with viewer (callers may change answers before the
// next CheckOwners). Both listeners are httptest servers.
func newGW(t *testing.T, o gwOpts) *gwEnv {
	t.Helper()
	fm := newFakeMuster(t)
	if o.tokens == nil {
		o.tokens = []string{"mpl_owner1"}
	}
	fm.set("mpl_owner1", 200, viewerSelf(1, "one@example.invalid"))
	fm.set("mpl_owner2", 200, viewerSelf(2, "two@example.invalid"))
	if o.dialWait == 0 {
		o.dialWait = 300 * time.Millisecond
	}
	dir := t.TempDir()
	cfg := Config{MusterURL: fm.srv.URL, PublicURL: testPublicURL, PluginTokens: o.tokens, SigningKey: []byte(testSigningKey),
		Domain: testDomain, Listen: ":0", StateDir: dir, TTL: o.ttl, Idle: o.idle, MaxBody: o.maxBody}
	if o.keepalive == 0 {
		o.keepalive = time.Hour
	}
	fc := newFakeClock(time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC))
	g, err := New(cfg, Options{Clock: fc, Muster: NewMusterClient(fm.srv.URL, "test"), DialWait: o.dialWait,
		PingInterval: time.Hour, StreamKeepalive: o.keepalive, MaxConnsPerPreview: o.perPrev})
	if err != nil {
		t.Fatal(err)
	}
	g.Start(context.Background())
	t.Cleanup(g.Close)
	e := &gwEnv{g: g, clock: fc, fm: fm, store: HostTokens{Dir: dir},
		public: httptest.NewServer(g.PublicHandler()), mesh: httptest.NewServer(g.MeshHandler())}
	t.Cleanup(e.public.Close)
	t.Cleanup(e.mesh.Close)
	e.serve(5173, appServer(t))
	return e
}

func (e *gwEnv) mint(t *testing.T, owner int64, host string) string {
	t.Helper()
	tok, _, err := e.store.Create(owner, "owner", host, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// gauge reads muster_preview_tunnels{path} from the public listener's
// /metrics on the mesh listener, as a scraper would.
func (e *gwEnv) gauge(t *testing.T, p Path) float64 {
	t.Helper()
	resp, err := http.Get(e.mesh.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	want := `muster_preview_tunnels{path="` + string(p) + `"} `
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, want); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("metric line %q: %v", line, err)
			}
			return f
		}
	}
	t.Fatalf("muster_preview_tunnels{path=%q} not on /metrics:\n%s", p, b)
	return 0
}

// metric reads one sample from /metrics on the mesh listener, as a scraper
// would: series is its exact text up to the value (name{labels}). It
// answers -1 for a series that is not there, so a test tells "0" from
// "absent".
func (e *gwEnv) metric(t *testing.T, series string) float64 {
	t.Helper()
	resp, err := http.Get(e.mesh.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("metric line %q: %v", line, err)
			}
			return f
		}
	}
	return -1
}

// ------------------------------------------------------------ fake host

// fakeHost is a host's end of a tunnel, built on tunnel.Serve: it serves the
// offer stream from offers, records the gateway's answers, and splices
// CONNECTs to loopback.
type fakeHost struct {
	offers   chan wire.Offer
	answers  chan hostAnswer
	boots    chan string
	connects atomic.Int64
	done     chan error
	cancel   context.CancelFunc
}

type hostAnswer struct {
	path string
	a    wire.Answer
}

type dialOpts struct {
	listener string // "public" or "mesh"
	token    string
	hostName string
	version  string // default "1"
	header   http.Header
	maxConns int    // the host's own Serve maxConns; 0 = wire.MaxConns
	hostHdr  string // the request's Host; default tunnel.<domain>
	// holdAnswers: the host never answers the gateway's POSTs (each blocks
	// until the gateway gives up on it).
	holdAnswers bool
}

// dialHost dials e's listener as a host would. A refusal returns the
// response and no host.
func (e *gwEnv) dialHost(t *testing.T, o dialOpts) (*fakeHost, *http.Response, error) {
	t.Helper()
	srv := e.public
	if o.listener == "mesh" {
		srv = e.mesh
	}
	h := http.Header{}
	for k, vs := range o.header {
		h[k] = vs
	}
	if o.token != "" {
		h.Set("Authorization", "Bearer "+o.token)
	}
	if o.version == "" {
		o.version = "1"
	}
	if o.version != "-" {
		h.Set(wire.VersionHeader, o.version)
	}
	h.Set(wire.HostNameHeader, o.hostName)
	if o.hostHdr == "" {
		o.hostHdr = "tunnel." + testDomain
	}
	ctx, cancel := context.WithCancel(context.Background())
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	tr := &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	ws, resp, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(srv.URL, "http")+wire.HostPath, &websocket.DialOptions{
		HTTPHeader: h, HTTPClient: &http.Client{Transport: tr}, Host: o.hostHdr})
	if err != nil {
		cancel()
		return nil, resp, err
	}
	fh := &fakeHost{offers: make(chan wire.Offer, 64), answers: make(chan hostAnswer, 64),
		boots: make(chan string, 4), done: make(chan error, 1), cancel: cancel}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == wire.OfferPath:
			fh.boots <- r.Header.Get(wire.GatewayBootHeader)
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			fl.Flush()
			enc := wire.NewEncoder(w)
			for {
				select {
				case <-r.Context().Done():
					return
				case m := <-fh.offers:
					if err := enc.Encode(m); err != nil {
						return
					}
					fl.Flush()
				}
			}
		case r.Method == http.MethodPost && (r.URL.Path == wire.OpenedPath || r.URL.Path == wire.ClosedPath):
			if o.holdAnswers {
				<-r.Context().Done()
				return
			}
			var a wire.Answer
			if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
				http.Error(w, "bad answer", http.StatusBadRequest)
				return
			}
			fh.answers <- hostAnswer{r.URL.Path, a}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodConnect:
			fh.connects.Add(1)
			port, err := wire.ConnectPort(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if why, ok := e.refuse.Load(port); ok {
				wire.WriteRefusal(w, why.(wire.Reason))
				return
			}
			if real, ok := e.ports.Load(port); ok {
				port = real.(int)
			}
			up, err := net.DialTimeout("tcp", wire.ConnectAuthority(port), 2*time.Second)
			if err != nil {
				wire.WriteRefusal(w, wire.ReasonRefused)
				return
			}
			tunnel.Splice(w, r, up)
		default:
			http.NotFound(w, r)
		}
	})
	go func() {
		fh.done <- tunnel.Serve(ctx, ws, handler, tunnel.HostOptions{MaxConns: o.maxConns, PingInterval: time.Hour})
	}()
	t.Cleanup(cancel)
	return fh, resp, nil
}

// mustDial is dialHost that must succeed and wait until the gateway lists
// the tunnel and its offer stream is open.
func (e *gwEnv) mustDial(t *testing.T, o dialOpts) *fakeHost {
	t.Helper()
	fh, resp, err := e.dialHost(t, o)
	if err != nil {
		st := 0
		if resp != nil {
			st = resp.StatusCode
		}
		t.Fatalf("dial %s as %q: %v (HTTP %d)", o.listener, o.hostName, err, st)
	}
	select {
	case <-fh.boots:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never opened the offer stream")
	}
	return fh
}

// tunnelFor is the gateway's current (non-draining) tunnel for (owner, host).
func (e *gwEnv) tunnelFor(owner int64, host string) *Tunnel {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	return e.g.byKey[tunnelKey{owner, host}]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stillOpen asserts ch stays open for a short real while.
func stillOpen(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s ended", what)
	case <-time.After(100 * time.Millisecond):
	}
}

func closedSoon(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not end", what)
	}
}

// echoServer echoes every connection on loopback; it returns its port.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// appBody is what appServer answers: the app saw the request.
const appBody = "app saw "

// appServer is a loopback HTTP app answering 200 "app saw <path>" to every
// request; it returns its port.
func appServer(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, appBody+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

// roundTrip writes msg on c and reads it back.
func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo: %q, %v", buf, err)
	}
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func isMaxConns(err error) bool {
	var re *tunnel.RefusedError
	return errors.As(err, &re) && re.Reason == wire.ReasonMaxConns
}
