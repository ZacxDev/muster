// Package tunnel is the preview tunnel's transport: one WebSocket from a host
// to the gateway, carrying HTTP/2 with the roles reversed — the host runs the
// HTTP/2 SERVER (Serve), the gateway holds the CLIENT (NewClient) and opens one
// CONNECT stream per upstream TCP connection (Client.Dial). The protocol on
// top (paths, headers, refusal reasons, the offer stream) is
// internal/preview/wire. See docs/design/previews.md ("The host tunnel").
//
// STREAMS: a host's maxConns counts its UPSTREAM connections (CONNECT
// streams) only. The control streams — the long-lived GET wire.OfferPath and
// the POST wire.OpenedPath / wire.ClosedPath answers — ride on ControlStreams
// more HTTP/2 streams above it, so they never queue behind data. The cap on
// data is kept at both ends: the gateway's Dial takes one of maxConns slots,
// waiting at most ClientOptions.DialWait for one to free and then failing
// with max_conns (429); the host refuses a CONNECT past its maxConns with
// max_conns too, as a backstop.
//
// 🔴 PINNED INVARIANT: the gateway's connection receive window is LARGER than
// (wire.MaxConns + ControlStreams) × its stream receive window (ConnWindow,
// StreamWindow) — every stream the host's SETTINGS_MAX_CONCURRENT_STREAMS
// admits. x/net's HTTP/2 client returns connection-level flow credit only as a
// stream's body is read, so a reader that pauses keeps its whole stream window
// out of the connection window until it resumes; once paused streams × stream
// window reach the connection window, EVERY other stream on the tunnel stalls,
// the control streams included. The same holds in the other direction for the
// host's upload windows (UploadConnWindow, UploadStreamWindow), where an
// upstream that stops reading a request body is the paused reader.
// TestFlowControlInvariantIsPinned and TestPausedReadersCannotStallTheTunnel
// pin the numbers and the behaviour together.
package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/http2"

	"github.com/ZacxDev/muster/internal/noredirect"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

// ControlStreams is the HTTP/2 stream headroom above maxConns for the control
// streams: one long-lived GET wire.OfferPath, plus up to three POST answers
// (wire.OpenedPath / wire.ClosedPath) in flight at once. An answer is one
// small request that completes in a round trip, so a fourth concurrent one
// waits a round trip behind the others — never behind a data stream, which
// cannot take these slots (the gateway's Dial is capped at maxConns).
const ControlStreams = 4

// Flow-control windows, in bytes. See the package comment for why the
// connection windows are sized from every stream the host admits
// (wire.MaxConns + ControlStreams), plus one stream of margin.
const (
	// StreamWindow is the gateway's per-stream receive window (host → gateway,
	// the response bytes a browser downloads). It is x/net's own client default.
	StreamWindow = 4 << 20
	// ConnWindow is the gateway's connection receive window: (64+4+1) × 4 MiB
	// = 276 MiB. It is also the most gateway memory one host's paused readers
	// can hold.
	ConnWindow = (wire.MaxConns + ControlStreams + 1) * StreamWindow
	// UploadStreamWindow is the host's per-stream receive window (gateway →
	// host, request bodies and WebSocket frames a browser sends).
	UploadStreamWindow = 1 << 20
	// UploadConnWindow is the host's connection receive window:
	// (64+4+1) × 1 MiB = 69 MiB, the most host memory one tunnel's stalled
	// uploads can hold.
	UploadConnWindow = (wire.MaxConns + ControlStreams + 1) * UploadStreamWindow
)

// DefaultDialWait is how long Dial waits for one of the host's maxConns
// upstream slots before refusing with max_conns.
const DefaultDialWait = 10 * time.Second

// ErrPeerUnresponsive ends a tunnel whose peer did not answer a WebSocket ping
// within one ping interval.
var ErrPeerUnresponsive = errors.New("tunnel: peer did not answer a ping")

// ErrClosed ends a tunnel that the peer or the transport closed.
var ErrClosed = errors.New("tunnel: closed")

// keepalive pings c every interval. A ping not answered within one interval
// means the peer is gone (or a middlebox swallowed the connection), so it
// calls dead and stops: waiting for TCP to notice can take many minutes.
func keepalive(ctx context.Context, c *websocket.Conn, every time.Duration, dead func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, every)
			err := c.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				dead()
				return
			}
		}
	}
}

func interval(d time.Duration) time.Duration {
	if d <= 0 {
		return wire.PingInterval
	}
	return d
}

// ------------------------------------------------------------------ host

// DialHost opens the host's tunnel WebSocket to a gateway endpoint (ws:// or
// wss://<gateway>/v1/host), sending hdr (the token, wire.VersionHeader,
// wire.HostNameHeader). The upgrade is forced onto HTTP/1.1: a non-nil, empty
// TLSNextProto is net/http's documented way to turn HTTP/2 negotiation off,
// and a WebSocket upgrade cannot ride HTTP/2. A refused dial returns the
// gateway's response (with wire.VersionRangeHeader on a version refusal).
// The client never follows a redirect (noredirect): one would re-send the
// host token to the Location.
func DialHost(ctx context.Context, endpoint string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	tr := &http.Transport{
		Proxy:        http.ProxyFromEnvironment,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: hdr.Clone(),
		HTTPClient: noredirect.Client(&http.Client{Transport: tr}),
	})
}

// HostOptions configures the host end of a tunnel.
type HostOptions struct {
	// MaxConns is the host's maxConns from previews.json: the most UPSTREAM
	// connections (CONNECT streams) the tunnel carries at once. The control
	// streams are not counted; they have ControlStreams of their own. 0 means
	// wire.MaxConns; more than wire.MaxConns is refused.
	MaxConns int
	// PingInterval is the WebSocket ping interval; 0 means wire.PingInterval.
	PingInterval time.Duration
	// ErrorLog receives the HTTP/2 server's own errors; nil means the log
	// package's standard logger.
	ErrorLog *log.Logger

	// Tests only: no pings, and other upload windows (the negative controls).
	noPings                              bool
	uploadConnWindow, uploadStreamWindow int32
}

// maxConns is o.MaxConns with its default, or an error outside 1..cap.
func (o HostOptions) maxConns() (int, error) {
	n := o.MaxConns
	if n == 0 {
		n = wire.MaxConns
	}
	if n < 0 || n > wire.MaxConns {
		return 0, fmt.Errorf("tunnel: maxConns %d outside 1..%d", o.MaxConns, wire.MaxConns)
	}
	return n, nil
}

// server is the HTTP/2 server Serve runs, built in one place so the invariant
// test reads the same values Serve uses. Its stream limit is maxConns plus
// ControlStreams; Serve's handler wrapper keeps the CONNECTs to maxConns.
func (o HostOptions) server() (*http2.Server, error) {
	n, err := o.maxConns()
	if err != nil {
		return nil, err
	}
	s := &http2.Server{
		MaxConcurrentStreams:         uint32(n + ControlStreams),
		MaxUploadBufferPerStream:     UploadStreamWindow,
		MaxUploadBufferPerConnection: UploadConnWindow,
	}
	if o.uploadStreamWindow != 0 {
		s.MaxUploadBufferPerStream = o.uploadStreamWindow
	}
	if o.uploadConnWindow != 0 {
		s.MaxUploadBufferPerConnection = o.uploadConnWindow
	}
	return s, nil
}

// Serve runs the host end of a tunnel on c (from DialHost) until ctx ends or
// the tunnel does, handing every stream the gateway opens to h: GET
// wire.OfferPath, POST wire.OpenedPath / wire.ClosedPath, and CONNECT (answer
// with Splice, or wire.WriteRefusal). A CONNECT past maxConns never reaches h:
// Serve answers it max_conns. It returns ctx's error, ErrPeerUnresponsive, or
// ErrClosed, and closes c.
func Serve(ctx context.Context, c *websocket.Conn, h http.Handler, o HostOptions) error {
	srv, err := o.server()
	if err != nil {
		c.CloseNow()
		return err
	}
	n, _ := o.maxConns() // already checked by server
	h = capConnects(h, n)
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	nc := websocket.NetConn(tctx, c, websocket.MessageBinary)
	defer nc.Close()
	var unresponsive bool
	var mu sync.Mutex
	if !o.noPings {
		go keepalive(tctx, c, interval(o.PingInterval), func() {
			mu.Lock()
			unresponsive = true
			mu.Unlock()
			cancel()
			c.CloseNow()
		})
	}
	srv.ServeConn(nc, &http2.ServeConnOpts{
		Context:    tctx,
		Handler:    h,
		BaseConfig: &http.Server{ErrorLog: o.ErrorLog},
	})
	mu.Lock()
	defer mu.Unlock()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case unresponsive:
		return ErrPeerUnresponsive
	}
	return ErrClosed
}

// capConnects refuses a CONNECT with max_conns while n are being served. The
// gateway caps its own Dials at maxConns, so this is a backstop: it fires when
// the gateway believes in more slots than the host has, or when a slot the
// gateway just freed is still winding down here (Dial retries that case).
func capConnects(h http.Handler, n int) http.Handler {
	var mu sync.Mutex
	active := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			h.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		if active >= n {
			mu.Unlock()
			wire.WriteRefusal(w, wire.ReasonMaxConns)
			return
		}
		active++
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()
		h.ServeHTTP(w, r)
	})
}

// Splice answers a CONNECT the host has decided to serve: it writes 200, then
// copies bytes both ways between the stream and up (a loopback connection the
// host dialed) until the upstream closes, the stream is reset, or the tunnel
// ends. Every chunk read from up is flushed at once, so a server-sent event
// is not held back. End of the gateway's side half-closes up (CloseWrite), so
// a request-then-response upstream still answers. It closes up.
func Splice(w http.ResponseWriter, r *http.Request, up net.Conn) {
	defer up.Close()
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "tunnel: stream cannot flush", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	// A reset stream or a dead tunnel cancels r's context; closing up then
	// unblocks both copies.
	stop := context.AfterFunc(r.Context(), func() { up.Close() })
	defer stop()
	go func() {
		_, err := io.Copy(up, r.Body)
		if cw, ok := up.(interface{ CloseWrite() error }); ok && err == nil {
			_ = cw.CloseWrite()
			return
		}
		up.Close()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := up.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			fl.Flush()
		}
		if err != nil {
			return
		}
	}
}

// --------------------------------------------------------------- gateway

// ClientOptions configures the gateway end of a tunnel.
type ClientOptions struct {
	// PingInterval is the WebSocket ping interval; 0 means wire.PingInterval.
	PingInterval time.Duration
	// MaxConns is how many upstream connections Dial lets be open at once
	// before it waits: the host's maxConns, which the gateway learns from the
	// offer stream after the tunnel is up (SetMaxConns, which states the
	// rule this must follow). 0 means wire.MaxConns, the cap; any other value
	// is clamped into 1..wire.MaxConns as SetMaxConns does.
	MaxConns int
	// DialWait bounds how long Dial waits for a free upstream slot before
	// refusing with max_conns; 0 means DefaultDialWait.
	DialWait time.Duration

	// Tests only: no pings, and other receive windows (the negative controls).
	noPings                  bool
	connWindow, streamWindow int
}

// http2Config is the client's flow-control configuration, built in one place
// so the invariant test reads the same values NewClient uses.
func (o ClientOptions) http2Config() *http.HTTP2Config {
	c := &http.HTTP2Config{
		MaxReceiveBufferPerStream:     StreamWindow,
		MaxReceiveBufferPerConnection: ConnWindow,
	}
	if o.streamWindow != 0 {
		c.MaxReceiveBufferPerStream = o.streamWindow
	}
	if o.connWindow != 0 {
		c.MaxReceiveBufferPerConnection = o.connWindow
	}
	return c
}

// Client is the gateway's handle on one host's tunnel. It is an
// http.RoundTripper for the control streams (wire.OfferPath and friends; any
// URL host, the path is what routes) and dials CONNECT streams with Dial.
type Client struct {
	slots    slots
	dialWait time.Duration
	cc       *http2.ClientConn
	ws       *websocket.Conn
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	err      error
}

// NewClient starts the gateway end of a tunnel on c, a WebSocket the gateway
// accepted on wire.HostPath (after checking the token, the host name and the
// version). The tunnel lives until ctx ends, Close, or the transport drops;
// Done reports which.
func NewClient(ctx context.Context, c *websocket.Conn, o ClientOptions) (*Client, error) {
	tctx, cancel := context.WithCancel(ctx)
	cl := &Client{ws: c, cancel: cancel, done: make(chan struct{}), dialWait: o.DialWait}
	if cl.dialWait <= 0 {
		cl.dialWait = DefaultDialWait
	}
	cl.slots.init(o.MaxConns)
	nc := &watchedConn{Conn: websocket.NetConn(tctx, c, websocket.MessageBinary), end: cl.end}
	t1 := &http.Transport{DisableCompression: true, HTTP2: o.http2Config()}
	t2, err := http2.ConfigureTransports(t1)
	if err != nil {
		cancel()
		c.CloseNow()
		return nil, err
	}
	// Wait for a free stream rather than fail: a raw ClientConn otherwise
	// answers "client conn not usable" once the host's
	// MAX_CONCURRENT_STREAMS are in use (measured; there is no second
	// connection to fall over to). Data streams cannot get there (Dial's
	// slots stop them at maxConns), so what this protects is a burst of
	// more than ControlStreams control requests: they queue a round trip
	// instead of failing (TestControlRequestsPastTheReserveQueue).
	t2.StrictMaxConcurrentStreams = true
	cc, err := t2.NewClientConn(nc)
	if err != nil {
		cancel()
		c.CloseNow()
		return nil, err
	}
	cl.cc = cc
	context.AfterFunc(tctx, func() { cl.end(ErrClosed) })
	if !o.noPings {
		go keepalive(tctx, c, interval(o.PingInterval), func() { cl.end(ErrPeerUnresponsive) })
	}
	return cl, nil
}

// end records why the tunnel ended (the first reason wins) and tears it down.
func (c *Client) end(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
		c.cancel()
		c.ws.CloseNow()
	})
}

// Done is closed when the tunnel has ended; Err then says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the tunnel ended (ErrClosed or ErrPeerUnresponsive), or nil
// while it is up.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the tunnel.
func (c *Client) Close() error {
	c.end(ErrClosed)
	return c.cc.Close()
}

// RoundTrip sends one control request over the tunnel. A URL with no scheme
// or host is given "http://tunnel".
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "" || req.URL.Host == "" {
		u := *req.URL
		u.Scheme, u.Host = "http", "tunnel"
		req = req.Clone(req.Context())
		req.URL = &u
	}
	return c.cc.RoundTrip(req)
}

// RefusedError is Dial's error when the host answered the CONNECT with
// anything but 200. Reason is "" when the host gave none it may give for
// that status (wire.RefusalOf).
type RefusedError struct {
	Port   int
	Status int
	Reason wire.Reason
}

func (e *RefusedError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("tunnel: CONNECT :%d refused: %d %s", e.Port, e.Status, e.Reason)
	}
	return fmt.Sprintf("tunnel: CONNECT :%d refused: %d", e.Port, e.Status)
}

// SetMaxConns sets how many upstream connections Dial lets be open at once:
// the host's maxConns, from its config on the offer stream. n is clamped
// into 1..wire.MaxConns: below 1 becomes 1, not the default, because a
// limit this side believes is LARGER than the host's only turns waits into
// host refusals, while a smaller one is merely slower. Streams already open
// stay open; a Dial waiting for a slot sees a raised limit at once.
//
// 🔴 THE RULE FOR THE CALLER: this limit must never exceed the maxConns the
// host's Serve was started with, which is fixed for the life of the tunnel
// (its HTTP/2 SETTINGS were sent at the start). So call it at most with the
// first config the offer stream carries; a later config with a different
// maxConns means the host must reconnect for it to take effect, and until
// then the gateway keeps the tunnel's original value.
func (c *Client) SetMaxConns(n int) { c.slots.setLimit(n) }

// Dial opens one CONNECT stream to the host's loopback port and returns it as
// a net.Conn, for an http.Transport's DialContext or a raw relay.
//
// It first takes one of the host's maxConns upstream slots. With all of them
// in use it WAITS, for at most DialWait, and then refuses with a
// *RefusedError whose Reason is max_conns (status 429) — so a page's burst of
// requests queues briefly instead of failing, and a host whose slots are held
// by paused readers fails fast enough to say so. A max_conns from the host
// itself (its backstop; a slot this side freed may still be winding down
// there) is retried within the same wait.
//
// ctx bounds only the dial; the stream then lives until Close, the host's
// upstream closing, or the tunnel ending, and its slot is freed on Close.
// CloseWrite half-closes it. Deadlines are not supported (Set*Deadline
// returns errors.ErrUnsupported); Close is how to abort a stream.
func (c *Client) Dial(ctx context.Context, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("tunnel: port %d out of range", port)
	}
	deadline := time.Now().Add(c.dialWait)
	maxConns := &RefusedError{Port: port, Status: wire.ReasonMaxConns.Status(), Reason: wire.ReasonMaxConns}
	switch err := c.slots.acquire(ctx, deadline, c.done); {
	case errors.Is(err, errSlotWait):
		return nil, maxConns
	case err != nil:
		return nil, fmt.Errorf("tunnel: CONNECT :%d: %w", port, err)
	}
	for {
		conn, err := c.dialOnce(ctx, port)
		if err == nil {
			conn.release = c.slots.release
			return conn, nil
		}
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != wire.ReasonMaxConns {
			c.slots.release()
			return nil, err
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			c.slots.release()
			return nil, maxConns
		}
		t := time.NewTimer(min(wait, 25*time.Millisecond))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			c.slots.release()
			return nil, fmt.Errorf("tunnel: CONNECT :%d: %w", port, context.Cause(ctx))
		case <-c.done:
			t.Stop()
			c.slots.release()
			return nil, fmt.Errorf("tunnel: CONNECT :%d: %w", port, ErrClosed)
		}
	}
}

// dialOnce sends one CONNECT and reads the host's answer.
func (c *Client) dialOnce(ctx context.Context, port int) (*streamConn, error) {
	pr, pw := io.Pipe()
	sctx, scancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, scancel)
	auth := wire.ConnectAuthority(port)
	req := (&http.Request{
		Method:        http.MethodConnect,
		URL:           &url.URL{Host: auth},
		Host:          auth,
		Header:        http.Header{},
		Body:          pr,
		ContentLength: -1,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
	}).WithContext(sctx)
	resp, err := c.cc.RoundTrip(req)
	fail := func(e error) (*streamConn, error) {
		if resp != nil {
			resp.Body.Close()
		}
		pw.Close()
		scancel()
		return nil, e
	}
	if !stop() {
		return fail(fmt.Errorf("tunnel: CONNECT :%d: %w", port, context.Cause(ctx)))
	}
	if err != nil {
		return fail(fmt.Errorf("tunnel: CONNECT :%d: %w", port, err))
	}
	if resp.StatusCode != http.StatusOK {
		return fail(&RefusedError{Port: port, Status: resp.StatusCode, Reason: wire.RefusalOf(resp.StatusCode, resp.Header)})
	}
	return &streamConn{r: resp.Body, w: pw, cancel: scancel, port: port}, nil
}

// errSlotWait is slots.acquire's error when the deadline passed first.
var errSlotWait = errors.New("tunnel: no upstream slot within the wait")

// slots counts the open upstream streams against the host's maxConns.
type slots struct {
	mu      sync.Mutex
	used    int
	limit   int
	changed chan struct{} // closed and replaced on every release or new limit
}

// clampConns puts n into 1..wire.MaxConns.
func clampConns(n int) int {
	return min(max(n, 1), wire.MaxConns)
}

// init sets the starting limit: ClientOptions.MaxConns, where 0 means
// wire.MaxConns (the default, before the host's config arrives).
func (s *slots) init(n int) {
	if n == 0 {
		n = wire.MaxConns
	}
	s.limit = clampConns(n)
	s.changed = make(chan struct{})
}

// inUse is how many slots are taken.
func (s *slots) inUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

func (s *slots) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *slots) setLimit(n int) {
	s.mu.Lock()
	s.limit = clampConns(n)
	s.broadcastLocked()
	s.mu.Unlock()
}

func (s *slots) release() {
	s.mu.Lock()
	s.used--
	s.broadcastLocked()
	s.mu.Unlock()
}

// acquire takes a slot, waiting until deadline, ctx or done.
func (s *slots) acquire(ctx context.Context, deadline time.Time, done <-chan struct{}) error {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		s.mu.Lock()
		if s.used < s.limit {
			s.used++
			s.mu.Unlock()
			return nil
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-t.C:
			return errSlotWait
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-done:
			return ErrClosed
		}
	}
}

// streamConn is one CONNECT stream as a net.Conn: reads are the response
// body, writes the request body.
type streamConn struct {
	r       io.ReadCloser
	w       *io.PipeWriter
	cancel  context.CancelFunc
	port    int
	release func() // frees the stream's upstream slot
	once    sync.Once
}

func (s *streamConn) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *streamConn) Write(p []byte) (int, error) { return s.w.Write(p) }

// CloseWrite ends the gateway's side of the stream (END_STREAM); reads go on.
func (s *streamConn) CloseWrite() error { return s.w.Close() }

func (s *streamConn) Close() error {
	s.once.Do(func() {
		s.w.Close()
		s.r.Close()
		s.cancel()
		if s.release != nil {
			s.release()
		}
	})
	return nil
}

func (s *streamConn) LocalAddr() net.Addr { return tunnelAddr("tunnel") }
func (s *streamConn) RemoteAddr() net.Addr {
	return tunnelAddr(net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port)))
}
func (s *streamConn) SetDeadline(time.Time) error      { return errors.ErrUnsupported }
func (s *streamConn) SetReadDeadline(time.Time) error  { return errors.ErrUnsupported }
func (s *streamConn) SetWriteDeadline(time.Time) error { return errors.ErrUnsupported }

type tunnelAddr string

func (tunnelAddr) Network() string  { return "tunnel" }
func (a tunnelAddr) String() string { return string(a) }

// watchedConn reports the end of the tunnel's transport: the HTTP/2 client's
// read loop is always reading, so its first read error is the tunnel's end.
type watchedConn struct {
	net.Conn
	end func(error)
}

func (w *watchedConn) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if err != nil {
		w.end(ErrClosed)
	}
	return n, err
}
