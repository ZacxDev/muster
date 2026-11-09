package tunnel

import (
	"bufio"
	"context"
	"crypto/sha256"
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

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// ------------------------------------------------------------- upstreams

// blobReader is n deterministic bytes from seed.
func blobReader(seed uint64, n int64) io.Reader {
	return io.LimitReader(rand.NewChaCha8(seedBytes(seed)), n)
}

func seedBytes(seed uint64) [32]byte {
	var s [32]byte
	for i := range 8 {
		s[i] = byte(seed >> (8 * i))
	}
	return s
}

func blobSHA(seed uint64, n int64) [32]byte {
	h := sha256.New()
	_, _ = io.Copy(h, blobReader(seed, n))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// startBlobServer serves GET /blob?n=&seed=&delay= on loopback.
func startBlobServer(t *testing.T) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
		seed, _ := strconv.ParseUint(r.URL.Query().Get("seed"), 10, 64)
		if d, err := time.ParseDuration(r.URL.Query().Get("delay")); err == nil {
			time.Sleep(d)
		}
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		_, _ = io.Copy(w, blobReader(seed, n))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return portOf(t, srv.Listener.Addr())
}

// startTCP serves each loopback connection with fn.
func startTCP(t *testing.T, fn func(net.Conn)) int {
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
			go func() {
				defer c.Close()
				fn(c)
			}()
		}
	}()
	return portOf(t, ln.Addr())
}

func echoUpstream(c net.Conn) { _, _ = io.Copy(c, c) }

func portOf(t *testing.T, a net.Addr) int {
	t.Helper()
	_, p, err := net.SplitHostPort(a.String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

// -------------------------------------------------------- the edge stand-in

// edgeSim is a LOCAL stand-in for an idle-killing middlebox (the public edge,
// or an overlay-mesh relay): a TCP relay that closes a connection pair after
// idle with no bytes in either direction. With blackhole set it keeps both
// connections open but delivers nothing (a peer that vanished without a FIN).
// It models those two behaviours only.
type edgeSim struct {
	target    string
	idle      time.Duration
	kills     atomic.Int64
	blackhole atomic.Bool
	ln        net.Listener
}

func startEdge(t *testing.T, target string, idle time.Duration) *edgeSim {
	t.Helper()
	e := &edgeSim{target: target, idle: idle}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go e.relay(c)
		}
	}()
	return e
}

func (e *edgeSim) addr() string { return e.ln.Addr().String() }

func (e *edgeSim) relay(a net.Conn) {
	b, err := net.Dial("tcp", e.target)
	if err != nil {
		a.Close()
		return
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done); a.Close(); b.Close() }) }
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 && !e.blackhole.Load() {
				last.Store(time.Now().UnixNano())
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if e.blackhole.Load() {
			// A vanished peer sends no FIN: the other side must find out by
			// its own pings, not by this relay passing the close along.
			src.Close()
			return
		}
		stop()
	}
	go cp(b, a)
	go cp(a, b)
	if e.idle <= 0 {
		<-done
		return
	}
	tk := time.NewTicker(e.idle / 20)
	defer tk.Stop()
	for {
		select {
		case <-done:
			return
		case <-tk.C:
			if !e.blackhole.Load() && time.Since(time.Unix(0, last.Load())) > e.idle {
				e.kills.Add(1)
				stop()
				return
			}
		}
	}
}

// ------------------------------------------------------------- the pair

// pair is one tunnel, both ends in this process: a gateway (httptest server
// accepting wire.HostPath, holding a Client) and a host (DialHost + Serve)
// whose handler splices CONNECTs to any loopback port in allow, refuses the
// rest with port_not_allowed, and serves extra for anything else.
type pair struct {
	client   *Client
	hostDone chan error
	edge     *edgeSim // nil without one
	cancel   context.CancelFunc
	// splicing counts the host's Splice calls still running.
	splicing atomic.Int64
}

type pairOpts struct {
	host     HostOptions
	client   ClientOptions
	allow    []int
	extra    http.Handler
	edgeIdle time.Duration // >0 puts an edgeSim between the host and the gateway
	useEdge  bool
}

func newPair(t *testing.T, o pairOpts) *pair {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clients := make(chan *Client, 1)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.HostPath {
			http.NotFound(w, r)
			return
		}
		if _, ok := wire.AcceptVersion(r.Header.Get(wire.VersionHeader)); !ok {
			w.Header().Set(wire.VersionRangeHeader, wire.VersionRange())
			http.Error(w, "version", http.StatusUpgradeRequired)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		cl, err := NewClient(ctx, c, o.client)
		if err != nil {
			t.Errorf("NewClient: %v", err)
			return
		}
		clients <- cl
	}))
	t.Cleanup(gw.Close)
	target := gw.Listener.Addr().String()
	p := &pair{hostDone: make(chan error, 1), cancel: cancel}
	if o.useEdge || o.edgeIdle > 0 {
		p.edge = startEdge(t, target, o.edgeIdle)
		target = p.edge.addr()
	}
	allow := map[int]bool{}
	for _, a := range o.allow {
		allow[a] = true
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			if o.extra != nil {
				o.extra.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		port, err := wire.ConnectPort(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !allow[port] {
			wire.WriteRefusal(w, wire.ReasonPortNotAllowed)
			return
		}
		up, err := net.DialTimeout("tcp", wire.ConnectAuthority(port), 2*time.Second)
		if err != nil {
			wire.WriteRefusal(w, wire.ReasonRefused)
			return
		}
		p.splicing.Add(1)
		defer p.splicing.Add(-1)
		Splice(w, r, up)
	})
	hdr := http.Header{wire.VersionHeader: {strconv.Itoa(wire.Version)}, wire.HostNameHeader: {"test-host"}}
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	c, _, err := DialHost(dctx, "ws://"+target+wire.HostPath, hdr)
	if err != nil {
		cancel()
		t.Fatalf("DialHost: %v", err)
	}
	go func() { p.hostDone <- Serve(ctx, c, h, o.host) }()
	select {
	case p.client = <-clients:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("gateway never got a client")
	}
	t.Cleanup(func() {
		p.client.Close()
		cancel()
	})
	return p
}

// httpClient is an HTTP/1.1 client whose every request is its own CONNECT
// stream on the tunnel (no keep-alive, so streams map 1:1 to requests).
func (p *pair) httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			_, ps, _ := net.SplitHostPort(addr)
			port, _ := strconv.Atoi(ps)
			return p.client.Dial(ctx, port)
		},
	}}
}

// pausedDownload is GET /blob of size bytes, read for first bytes and then
// left unread.
type pausedDownload struct {
	resp      *http.Response
	h         io.Writer
	sum       func() []byte
	n, size   int64
	seed      uint64
	closeOnce sync.Once
}

func startPaused(t *testing.T, p *pair, port int, size int64, seed uint64, first int64) *pausedDownload {
	t.Helper()
	d, err := tryPaused(t, p, port, size, seed, first, 10*time.Second)
	if err != nil {
		t.Fatalf("paused download: %v", err)
	}
	return d
}

// tryPaused is startPaused that gives up after budget without its first
// bytes: a stalled tunnel can stall the setup itself.
func tryPaused(t *testing.T, p *pair, port int, size int64, seed uint64, first int64, budget time.Duration) (*pausedDownload, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	timer := time.AfterFunc(budget, cancel)
	defer timer.Stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/blob?n=%d&seed=%d", port, size, seed), nil)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	d := &pausedDownload{resp: resp, h: h, sum: func() []byte { return h.Sum(nil) }, size: size, seed: seed}
	t.Cleanup(d.close)
	n, err := io.CopyN(h, resp.Body, first)
	d.n = n
	if err != nil {
		return nil, fmt.Errorf("first %d bytes: %w", first, err)
	}
	if !timer.Stop() {
		return nil, fmt.Errorf("first %d bytes took over %v", first, budget)
	}
	return d, nil
}

func (d *pausedDownload) close() { d.closeOnce.Do(func() { d.resp.Body.Close() }) }

// resume reads the rest and reports whether it was byte-exact.
func (d *pausedDownload) resume() (bool, int64, error) {
	defer d.close()
	n, err := io.Copy(d.h, d.resp.Body)
	d.n += n
	want := blobSHA(d.seed, d.size)
	return d.n == d.size && string(d.sum()) == string(want[:]), d.n, err
}

// echoRoundTrips sends n small messages on one stream to an echo upstream,
// each waiting for its echo, before deadline.
func echoRoundTrips(p *pair, port, n int, deadline time.Time) (int, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	c, err := p.client.Dial(ctx, port)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	br := bufio.NewReader(c)
	for i := range n {
		msg := fmt.Sprintf("hmr %d %s\n", i, strings.Repeat("x", 48))
		if _, err := io.WriteString(c, msg); err != nil {
			return i, err
		}
		got, err := br.ReadString('\n')
		if err != nil {
			return i, err
		}
		if got != msg {
			return i, errors.New("echo mismatch")
		}
	}
	return n, nil
}
