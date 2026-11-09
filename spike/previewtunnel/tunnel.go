// Package previewtunnel is a THROWAWAY spike (phase 0 of docs/design/previews.md).
// It is not production code and nothing in muster imports it.
//
// It builds the host→gateway preview tunnel twice over one WebSocket
// (coder/websocket NetConn): once carrying HTTP/2 CONNECT streams
// (golang.org/x/net/http2, roles reversed: the host runs http2.Server.ServeConn,
// the gateway holds an http2.ClientConn) and once carrying yamux streams.
package previewtunnel

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"golang.org/x/net/http2"
)

// Transport selects the candidate under test.
type Transport string

const (
	H2    Transport = "h2connect"
	Yamux Transport = "yamux"
)

// StreamWindow is the per-stream receive window used by BOTH candidates, so
// criterion 2's "twice the HTTP/2 window" bound is the same number for each.
// It is x/net/http2's own Transport default (transportDefaultStreamFlow).
const StreamWindow = 4 << 20

// Faults are deliberate breakages used ONLY by negative controls.
type Faults struct {
	HostWriteDelay      time.Duration // C1: host sleeps before every write into the tunnel
	HostTruncateAfter   int64         // C2: host stops copying upstream→tunnel after N bytes (0 = off)
	HostUnboundedBuffer bool          // C2: host reads upstream eagerly into an unbounded queue
	H2ConnWindow        int32         // C3: gateway's HTTP/2 connection-level receive window (0 = x/net default, 1 GiB)
	NoPings             bool          // C4: no WebSocket pings on the tunnel or the browser leg
	HostNoFlush         bool          // C5: host never flushes its tunnel writer
}

// HostStats are counters the host keeps so tests can observe it.
type HostStats struct {
	Streams     atomic.Int64 // currently open streams
	PeakStreams atomic.Int64
	Held        atomic.Int64 // bytes read from upstream, not yet handed to the tunnel writer
	PeakHeld    atomic.Int64
}

func (s *HostStats) open() {
	n := s.Streams.Add(1)
	for {
		p := s.PeakStreams.Load()
		if n <= p || s.PeakStreams.CompareAndSwap(p, n) {
			return
		}
	}
}

func (s *HostStats) held(d int64) {
	n := s.Held.Add(d)
	for {
		p := s.PeakHeld.Load()
		if n <= p || s.PeakHeld.CompareAndSwap(p, n) {
			return
		}
	}
}

// ---------------------------------------------------------------- host side

// Host dials the gateway and serves streams to loopback upstreams.
type Host struct {
	Transport    Transport
	Allow        map[int]bool // allowlisted ports
	Faults       Faults
	PingInterval time.Duration
	Stats        HostStats
	Header       http.Header // extra headers on the tunnel request (remote mode: secret, port)
}

// Run dials wsURL and serves the tunnel until ctx ends or the tunnel drops.
func (h *Host) Run(ctx context.Context, wsURL string) error {
	hdr := http.Header{"X-Muster-Preview-Version": {"1"}, "X-Spike-Transport": {string(h.Transport)}}
	for k, v := range h.Header {
		hdr[k] = v
	}
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: hdr,
		HTTPClient: http1Client(), // HTTP/1.1: a WebSocket upgrade through a TLS edge must not negotiate h2
	})
	if err != nil {
		return err
	}
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	defer nc.Close()
	pingCtx, stop := context.WithCancel(ctx)
	defer stop()
	if !h.Faults.NoPings {
		go pingLoop(pingCtx, c, h.PingInterval)
	}
	switch h.Transport {
	case H2:
		srv := &http2.Server{
			MaxConcurrentStreams:     1000,
			MaxUploadBufferPerStream: StreamWindow,
		}
		srv.ServeConn(nc, &http2.ServeConnOpts{Context: ctx, Handler: http.HandlerFunc(h.serveH2)})
		return errors.New("h2 tunnel ended")
	case Yamux:
		cfg := yamux.DefaultConfig()
		cfg.MaxStreamWindowSize = StreamWindow
		cfg.LogOutput = io.Discard
		sess, err := yamux.Server(nc, cfg)
		if err != nil {
			return err
		}
		for {
			s, err := sess.Accept()
			if err != nil {
				return err
			}
			go h.serveYamux(s)
		}
	}
	return fmt.Errorf("unknown transport %q", h.Transport)
}

func pingLoop(ctx context.Context, c *websocket.Conn, every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, every)
			_ = c.Ping(pctx)
			cancel()
		}
	}
}

func (h *Host) dial(port int) (net.Conn, int) {
	if !h.Allow[port] {
		return nil, http.StatusForbidden
	}
	// Loopback only, the port only: never the requested address.
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		return nil, http.StatusBadGateway
	}
	return c, http.StatusOK
}

func (h *Host) serveH2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	_, ps, err := net.SplitHostPort(r.Host)
	port, _ := strconv.Atoi(ps)
	if err != nil || port == 0 {
		http.Error(w, "bad authority", http.StatusBadRequest)
		return
	}
	up, code := h.dial(port)
	if code != http.StatusOK {
		w.WriteHeader(code)
		return
	}
	defer up.Close()
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	fl.Flush()
	h.splice(up, r.Body, w, fl.Flush)
}

func (h *Host) serveYamux(s net.Conn) {
	defer s.Close()
	var hdr [2]byte
	if _, err := io.ReadFull(s, hdr[:]); err != nil {
		return
	}
	up, code := h.dial(int(binary.BigEndian.Uint16(hdr[:])))
	if code != http.StatusOK {
		_, _ = s.Write([]byte{byte(code / 100)})
		return
	}
	defer up.Close()
	if _, err := s.Write([]byte{2}); err != nil {
		return
	}
	h.splice(up, s, s, func() {})
}

// splice copies bytes both ways between the upstream TCP conn and one tunnel
// stream, half-closing each direction on EOF. It returns when the
// upstream→tunnel direction ends (the upstream closed).
func (h *Host) splice(up net.Conn, fromTunnel io.Reader, toTunnel io.Writer, flushStream func()) {
	h.Stats.open()
	defer h.Stats.Streams.Add(-1)

	go func() {
		_, _ = io.Copy(up, fromTunnel)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	bw := bufio.NewWriterSize(toTunnel, 64<<10)
	writes := 0
	flush := func() {
		writes++
		if h.Faults.HostNoFlush && writes > 1 {
			return // BROKEN on purpose: only the first chunk (the response head) is flushed
		}
		_ = bw.Flush()
		flushStream()
	}
	defer func() {
		if !h.Faults.HostNoFlush {
			_ = bw.Flush()
			flushStream()
		}
	}()

	write := func(p []byte) error {
		if h.Faults.HostWriteDelay > 0 {
			time.Sleep(h.Faults.HostWriteDelay)
		}
		if _, err := bw.Write(p); err != nil {
			return err
		}
		flush()
		return nil
	}

	var sent int64
	limit := h.Faults.HostTruncateAfter

	if h.Faults.HostUnboundedBuffer {
		// BROKEN on purpose: read ahead of the tunnel with no bound.
		var mu sync.Mutex
		var q [][]byte
		done := false
		cond := sync.NewCond(&mu)
		go func() {
			for {
				buf := make([]byte, 32<<10)
				n, err := up.Read(buf)
				mu.Lock()
				if n > 0 {
					q = append(q, buf[:n])
					h.Stats.held(int64(n))
				}
				if err != nil {
					done = true
				}
				cond.Signal()
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		for {
			mu.Lock()
			for len(q) == 0 && !done {
				cond.Wait()
			}
			if len(q) == 0 && done {
				mu.Unlock()
				return
			}
			p := q[0]
			q = q[1:]
			mu.Unlock()
			h.Stats.held(-int64(len(p)))
			if write(p) != nil {
				return
			}
		}
	}

	buf := make([]byte, 32<<10)
	for {
		n, err := up.Read(buf)
		if n > 0 {
			h.Stats.held(int64(n))
			p := buf[:n]
			if limit > 0 && sent+int64(n) > limit {
				p = p[:max(0, limit-sent)]
			}
			werr := write(p)
			h.Stats.held(-int64(n))
			sent += int64(len(p))
			if werr != nil || (limit > 0 && sent >= limit) {
				return // BROKEN on purpose when limit > 0: stream ends early
			}
		}
		if err != nil {
			return
		}
	}
}

// ------------------------------------------------------------- gateway side

// Tunnel is the gateway's handle on one host's tunnel.
type Tunnel interface {
	Open(ctx context.Context, port int) (net.Conn, error)
	Close() error
}

// AcceptTunnel upgrades the host's request and returns the gateway end.
func AcceptTunnel(w http.ResponseWriter, r *http.Request, f Faults, ping time.Duration) (Tunnel, error) {
	tr := Transport(r.Header.Get("X-Spike-Transport"))
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	if !f.NoPings {
		go pingLoop(ctx, c, ping)
	}
	switch tr {
	case H2:
		t1 := &http.Transport{HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerStream:     StreamWindow,
			MaxReceiveBufferPerConnection: int(f.H2ConnWindow), // 0 => x/net default (1 GiB)
		}}
		t2, err := http2.ConfigureTransports(t1)
		if err != nil {
			cancel()
			return nil, err
		}
		cc, err := t2.NewClientConn(nc)
		if err != nil {
			cancel()
			return nil, err
		}
		return &h2Tunnel{cc: cc, cancel: cancel, nc: nc}, nil
	case Yamux:
		cfg := yamux.DefaultConfig()
		cfg.MaxStreamWindowSize = StreamWindow
		cfg.LogOutput = io.Discard
		sess, err := yamux.Client(nc, cfg)
		if err != nil {
			cancel()
			return nil, err
		}
		return &yamuxTunnel{s: sess, cancel: cancel}, nil
	}
	cancel()
	return nil, fmt.Errorf("unknown transport %q", tr)
}

type h2Tunnel struct {
	cc     *http2.ClientConn
	nc     net.Conn
	cancel func()
}

func (t *h2Tunnel) Close() error { t.cancel(); return t.cc.Close() }

func (t *h2Tunnel) Open(ctx context.Context, port int) (net.Conn, error) {
	pr, pw := io.Pipe()
	auth := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	req := &http.Request{
		Method:        http.MethodConnect,
		URL:           &url.URL{Host: auth},
		Host:          auth,
		Header:        http.Header{},
		Body:          pr,
		ContentLength: -1,
		Proto:         "HTTP/2.0", ProtoMajor: 2,
	}
	// The stream outlives ctx (which is only the dial's); use Background.
	resp, err := t.cc.RoundTrip(req.WithContext(context.Background()))
	if err != nil {
		pw.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		pw.Close()
		return nil, fmt.Errorf("CONNECT %s: %s", auth, resp.Status)
	}
	return &streamConn{r: resp.Body, w: pw, closeR: resp.Body.Close}, nil
}

type yamuxTunnel struct {
	s      *yamux.Session
	cancel func()
}

func (t *yamuxTunnel) Close() error { t.cancel(); return t.s.Close() }

func (t *yamuxTunnel) Open(ctx context.Context, port int) (net.Conn, error) {
	s, err := t.s.OpenStream()
	if err != nil {
		return nil, err
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(port))
	if _, err := s.Write(hdr[:]); err != nil {
		s.Close()
		return nil, err
	}
	var st [1]byte
	if _, err := io.ReadFull(s, st[:]); err != nil || st[0] != 2 {
		s.Close()
		return nil, fmt.Errorf("yamux open %d: status %d %v", port, st[0], err)
	}
	return s, nil
}

// streamConn adapts an HTTP/2 CONNECT stream (response body + request body
// pipe) to net.Conn, so http.Transport can dial through it.
type streamConn struct {
	r      io.Reader
	w      *io.PipeWriter
	closeR func() error
	once   sync.Once
}

func (c *streamConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *streamConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *streamConn) CloseWrite() error           { return c.w.Close() }
func (c *streamConn) Close() error {
	c.once.Do(func() { c.w.Close(); c.closeR() })
	return nil
}
func (c *streamConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *streamConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tunnel" }
func (dummyAddr) String() string  { return "tunnel" }
