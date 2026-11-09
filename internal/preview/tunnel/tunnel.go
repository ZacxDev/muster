// Package tunnel is the preview tunnel's transport: one WebSocket from a host
// to the gateway, carrying HTTP/2 with the roles reversed — the host runs the
// HTTP/2 SERVER (Serve), the gateway holds the CLIENT (NewClient) and opens one
// CONNECT stream per upstream TCP connection (Client.Dial). The protocol on
// top (paths, headers, refusal reasons, the offer stream) is
// internal/preview/wire. See docs/design/previews.md ("The host tunnel").
//
// 🔴 PINNED INVARIANT: the gateway's connection receive window is LARGER than
// wire.MaxConns × its stream receive window (ConnWindow, StreamWindow). x/net's
// HTTP/2 client returns connection-level flow credit only as a stream's body is
// read, so a reader that pauses keeps its whole stream window out of the
// connection window until it resumes; once paused streams × stream window
// reach the connection window, EVERY other stream on the tunnel stalls. The
// host's SETTINGS_MAX_CONCURRENT_STREAMS is wire.MaxConns or less, so at most
// that many streams can be paused. The same holds in the other direction for
// the host's upload windows (UploadConnWindow, UploadStreamWindow), where an
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

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// Flow-control windows, in bytes. See the package comment for why the
// connection windows are sized from wire.MaxConns, plus one stream of margin.
const (
	// StreamWindow is the gateway's per-stream receive window (host → gateway,
	// the response bytes a browser downloads). It is x/net's own client default.
	StreamWindow = 4 << 20
	// ConnWindow is the gateway's connection receive window: (64+1) × 4 MiB =
	// 260 MiB. It is also the most gateway memory one host's paused readers
	// can hold.
	ConnWindow = (wire.MaxConns + 1) * StreamWindow
	// UploadStreamWindow is the host's per-stream receive window (gateway →
	// host, request bodies and WebSocket frames a browser sends).
	UploadStreamWindow = 1 << 20
	// UploadConnWindow is the host's connection receive window: (64+1) × 1 MiB
	// = 65 MiB, the most host memory one tunnel's stalled uploads can hold.
	UploadConnWindow = (wire.MaxConns + 1) * UploadStreamWindow
)

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
func DialHost(ctx context.Context, endpoint string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	tr := &http.Transport{
		Proxy:        http.ProxyFromEnvironment,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: hdr.Clone(),
		HTTPClient: &http.Client{Transport: tr},
	})
}

// HostOptions configures the host end of a tunnel.
type HostOptions struct {
	// MaxConns is the host's maxConns from previews.json: the most streams
	// the tunnel carries at once, control streams included. 0 means
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

// server is the HTTP/2 server Serve runs, built in one place so the invariant
// test reads the same values Serve uses.
func (o HostOptions) server() (*http2.Server, error) {
	n := o.MaxConns
	if n == 0 {
		n = wire.MaxConns
	}
	if n < 0 || n > wire.MaxConns {
		return nil, fmt.Errorf("tunnel: maxConns %d outside 1..%d", o.MaxConns, wire.MaxConns)
	}
	s := &http2.Server{
		MaxConcurrentStreams:         uint32(n),
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
// with Splice, or wire.WriteRefusal). It returns ctx's error,
// ErrPeerUnresponsive, or ErrClosed, and closes c.
func Serve(ctx context.Context, c *websocket.Conn, h http.Handler, o HostOptions) error {
	srv, err := o.server()
	if err != nil {
		c.CloseNow()
		return err
	}
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
	cc     *http2.ClientConn
	ws     *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

// NewClient starts the gateway end of a tunnel on c, a WebSocket the gateway
// accepted on wire.HostPath (after checking the token, the host name and the
// version). The tunnel lives until ctx ends, Close, or the transport drops;
// Done reports which.
func NewClient(ctx context.Context, c *websocket.Conn, o ClientOptions) (*Client, error) {
	tctx, cancel := context.WithCancel(ctx)
	cl := &Client{ws: c, cancel: cancel, done: make(chan struct{})}
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
	// connection to fall over to).
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

// Dial opens one CONNECT stream to the host's loopback port and returns it as
// a net.Conn, for an http.Transport's DialContext or a raw relay. ctx bounds
// only the dial (waiting for a free stream and the host's answer); the stream
// then lives until Close, the host's upstream closing, or the tunnel ending.
// CloseWrite half-closes it. Deadlines are not supported (Set*Deadline
// returns errors.ErrUnsupported); Close is how to abort a stream.
func (c *Client) Dial(ctx context.Context, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("tunnel: port %d out of range", port)
	}
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
	fail := func(e error) (net.Conn, error) {
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

// streamConn is one CONNECT stream as a net.Conn: reads are the response
// body, writes the request body.
type streamConn struct {
	r      io.ReadCloser
	w      *io.PipeWriter
	cancel context.CancelFunc
	port   int
	once   sync.Once
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
