package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

func newGet(ctx context.Context, u string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
}

func TestDialSplicesToTheAllowedPortAndRefusesTheRest(t *testing.T) {
	echo := startTCP(t, echoUpstream)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := portOf(t, ln.Addr())
	ln.Close() // allowed, but nothing listens
	p := newPair(t, pairOpts{allow: []int{echo, dead}})

	if n, err := echoRoundTrips(p, echo, 5, time.Now().Add(5*time.Second)); n != 5 || err != nil {
		t.Fatalf("echo through the tunnel: %d/5 %v", n, err)
	}
	for _, c := range []struct {
		port   int
		status int
		reason wire.Reason
	}{
		{echo + 1, 403, wire.ReasonPortNotAllowed},
		{dead, 502, wire.ReasonRefused},
	} {
		_, err := p.client.Dial(context.Background(), c.port)
		var re *RefusedError
		if !errors.As(err, &re) || re.Status != c.status || re.Reason != c.reason || re.Port != c.port {
			t.Errorf("Dial(:%d) = %v, want a %d %s refusal", c.port, err, c.status, c.reason)
		}
	}
	for _, bad := range []int{0, -1, 65536} {
		if _, err := p.client.Dial(context.Background(), bad); err == nil {
			t.Errorf("Dial(%d) accepted", bad)
		}
	}
}

// TestHalfCloseReachesTheUpstream: the gateway's CloseWrite is the upstream's
// EOF, and the upstream's answer after it still arrives in full.
func TestHalfCloseReachesTheUpstream(t *testing.T) {
	up := startTCP(t, func(c net.Conn) {
		b, _ := io.ReadAll(c) // until the gateway's CloseWrite
		_, _ = io.WriteString(c, "got "+strconv.Itoa(len(b)))
	})
	p := newPair(t, pairOpts{allow: []int{up}})
	c, err := p.client.Dial(context.Background(), up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, strings.Repeat("a", 100000)); err != nil {
		t.Fatal(err)
	}
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "got 100000" {
		t.Fatalf("after CloseWrite: %q %v", got, err)
	}
	if err := c.SetDeadline(time.Now()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("SetDeadline = %v, want ErrUnsupported (a silent no-op would be believed)", err)
	}
}

// TestCloseOfAHalfClosedStreamEndsTheSplice: after the gateway's CloseWrite,
// a silent upstream gives the host nothing to read or write, so only the
// stream's reset can end the host's Splice. It must, or every such stream
// leaks a goroutine and an upstream connection.
func TestCloseOfAHalfClosedStreamEndsTheSplice(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	up := startTCP(t, func(c net.Conn) {
		_, _ = io.ReadAll(c) // until CloseWrite, then silent
		<-release
	})
	p := newPair(t, pairOpts{allow: []int{up}})
	c, err := p.client.Dial(context.Background(), up)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.(interface{ CloseWrite() error }).CloseWrite()
	time.Sleep(200 * time.Millisecond)
	if n := p.splicing.Load(); n != 1 {
		t.Fatalf("control: %d splices running before Close, want 1", n)
	}
	c.Close()
	for end := time.Now().Add(3 * time.Second); p.splicing.Load() != 0; {
		if time.Now().After(end) {
			t.Fatal("the host's Splice is still running after the gateway closed the stream")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestEveryChunkIsFlushed: a small write the upstream makes (a server-sent
// event, an HMR message) reaches the gateway while the upstream waits, not
// when a buffer fills or the stream ends.
func TestEveryChunkIsFlushed(t *testing.T) {
	release := make(chan struct{})
	up := startTCP(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "data: hello\n\n")
		<-release
	})
	defer close(release)
	p := newPair(t, pairOpts{allow: []int{up}})
	c, err := p.client.Dial(context.Background(), up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(c).ReadString('\n')
		got <- l
	}()
	select {
	case l := <-got:
		if l != "data: hello\n" {
			t.Fatalf("got %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a 13-byte event was held back by the host")
	}
}

// TestCloseReachesTheUpstream: the gateway's Close resets the stream, and the
// host closes its upstream connection.
func TestCloseReachesTheUpstream(t *testing.T) {
	closed := make(chan struct{})
	up := startTCP(t, func(c net.Conn) {
		_, _ = io.Copy(io.Discard, c)
		close(closed)
	})
	p := newPair(t, pairOpts{allow: []int{up}})
	c, err := p.client.Dial(context.Background(), up)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "hello")
	c.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream was never closed after the gateway closed the stream")
	}
}

// TestControlStreamDeliversLinesAsWritten: the offer stream is a long-lived
// response; each flushed line reaches the gateway while the response is still
// open.
func TestControlStreamDeliversLinesAsWritten(t *testing.T) {
	release := make(chan struct{})
	extra := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.OfferPath || r.Header.Get(wire.GatewayBootHeader) != "boot-1" {
			http.NotFound(w, r)
			return
		}
		enc := wire.NewEncoder(w)
		_ = enc.Encode(wire.Offer{Type: wire.TypeWindows, Windows: []string{"@1"}})
		w.(http.Flusher).Flush()
		<-release
		_ = enc.Encode(wire.Offer{Type: wire.TypeWindows, Windows: []string{"@1", "@2"}})
	})
	p := newPair(t, pairOpts{extra: extra})
	req, _ := newGet(context.Background(), wire.OfferPath)
	req.URL.Host, req.URL.Scheme = "", ""
	req.Header.Set(wire.GatewayBootHeader, "boot-1")
	resp, err := p.client.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dec := wire.NewDecoder(resp.Body)
	got := make(chan wire.Offer, 1)
	go func() {
		m, _ := dec.Next()
		got <- m
	}()
	select {
	case m := <-got:
		if len(m.Windows) != 1 {
			t.Fatalf("first line = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first line did not arrive while the response was open")
	}
	close(release)
	m, err := dec.Next()
	if err != nil || len(m.Windows) != 2 {
		t.Fatalf("second line = %+v %v", m, err)
	}
}

// TestMaxConnsBoundsConcurrentStreams: with MaxConns 2 a third Dial waits for
// a free stream (and its ctx ends the wait); closing one lets it through.
func TestMaxConnsBoundsConcurrentStreams(t *testing.T) {
	echo := startTCP(t, echoUpstream)
	p := newPair(t, pairOpts{host: HostOptions{MaxConns: 2}, allow: []int{echo}})
	a, err := p.client.Dial(context.Background(), echo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.client.Dial(context.Background(), echo)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	third := make(chan error, 1)
	go func() {
		c, err := p.client.Dial(ctx, echo)
		if err == nil {
			c.Close()
		}
		third <- err
	}()
	select {
	case err := <-third:
		if err == nil {
			t.Fatal("a third stream opened past MaxConns 2")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("third Dial = %v, want the ctx's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the third Dial ignored its ctx and is still waiting")
	}
	a.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	c, err := p.client.Dial(ctx2, echo)
	if err != nil {
		t.Fatalf("after a stream closed, Dial = %v", err)
	}
	c.Close()
}

func TestServeRefusesACapAboveTheWindowsSizing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err == nil {
			_, _, _ = c.Read(context.Background())
		}
	}))
	defer srv.Close()
	c, _, err := DialHost(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), c, http.NotFoundHandler(), HostOptions{MaxConns: wire.MaxConns + 1}); err == nil || errors.Is(err, ErrClosed) {
		t.Fatalf("Serve with maxConns %d = %v, want a refusal", wire.MaxConns+1, err)
	}
}

// TestDialHostSeesAVersionRefusal: a gateway refusing the version answers
// with the range it speaks, and the host's dial error carries the response.
func TestDialHostSeesAVersionRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := wire.AcceptVersion(r.Header.Get(wire.VersionHeader)); !ok {
			w.Header().Set(wire.VersionRangeHeader, wire.VersionRange())
			http.Error(w, "version", http.StatusUpgradeRequired)
			return
		}
		t.Error("version 9 accepted")
	}))
	defer srv.Close()
	_, resp, err := DialHost(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+wire.HostPath,
		http.Header{wire.VersionHeader: {"9"}})
	if err == nil || resp == nil || resp.Header.Get(wire.VersionRangeHeader) != "1-1" {
		t.Fatalf("DialHost = %v, resp %v", err, resp)
	}
}

// --------------------------------------------------------- keepalive

// idleThenUse holds an echo stream open across idle with nothing sent, then
// checks the stream, a new stream, and that neither end of the tunnel ended.
type idleResult struct {
	echoAfter, newAfter, alive bool
	kills                      int64
}

func idleThenUse(t *testing.T, ping time.Duration, noPings bool, edgeIdle, idle time.Duration) idleResult {
	t.Helper()
	echo := startTCP(t, echoUpstream)
	p := newPair(t, pairOpts{
		host:     HostOptions{PingInterval: ping, noPings: noPings},
		client:   ClientOptions{PingInterval: ping, noPings: noPings},
		allow:    []int{echo},
		edgeIdle: edgeIdle,
	})
	c, err := p.client.Dial(context.Background(), echo)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	roundtrip := func() bool {
		done := make(chan bool, 1)
		go func() {
			if _, err := io.WriteString(c, "x\n"); err != nil {
				done <- false
				return
			}
			l, err := br.ReadString('\n')
			done <- err == nil && l == "x\n"
		}()
		select {
		case ok := <-done:
			return ok
		case <-time.After(3 * time.Second):
			return false
		}
	}
	if !roundtrip() {
		t.Fatal("first echo failed")
	}
	time.Sleep(idle)
	var r idleResult
	r.echoAfter = roundtrip()
	if n, err := echoRoundTrips(p, echo, 1, time.Now().Add(3*time.Second)); n == 1 && err == nil {
		r.newAfter = true
	}
	select {
	case <-p.hostDone:
	case <-p.client.Done():
	default:
		r.alive = true
	}
	r.kills = p.edge.kills.Load()
	t.Logf("RESULT idle=%v edge idle=%v ping=%v noPings=%v: %+v", idle, edgeIdle, ping, noPings, r)
	return r
}

// TestPingsKeepAnIdleTunnelThroughAnIdleKillingEdge: scaled from the real
// numbers (pings every 30 s under an edge that cuts after ~100 s quiet): the
// edge stand-in cuts after 3 s, pings go every 1 s, and the tunnel sits idle
// for 8 s. The negative control turns pings off at both ends, and the edge
// must then have killed the tunnel.
func TestPingsKeepAnIdleTunnelThroughAnIdleKillingEdge(t *testing.T) {
	t.Parallel()
	t.Run("pings", func(t *testing.T) {
		t.Parallel()
		r := idleThenUse(t, time.Second, false, 3*time.Second, 8*time.Second)
		if !r.echoAfter || !r.newAfter || !r.alive || r.kills != 0 {
			t.Fatalf("the pinged tunnel did not survive the idle edge: %+v", r)
		}
	})
	t.Run("negative control: no pings", func(t *testing.T) {
		t.Parallel()
		r := idleThenUse(t, time.Second, true, 3*time.Second, 8*time.Second)
		if r.echoAfter && r.newAfter && r.alive {
			t.Fatalf("negative control stayed GREEN: %+v", r)
		}
		if r.kills == 0 {
			t.Fatalf("red, but the edge stand-in killed nothing: %+v", r)
		}
	})
}

// TestAnUnansweredPingEndsBothEnds: a path that swallows everything without a
// FIN (the edge stand-in's blackhole) is detected by the pings, and both ends
// end with ErrPeerUnresponsive within a few intervals rather than waiting for
// TCP. The control is the same tunnel left alone for the same time.
func TestAnUnansweredPingEndsBothEnds(t *testing.T) {
	const ping = 300 * time.Millisecond
	p := newPair(t, pairOpts{
		host:    HostOptions{PingInterval: ping},
		client:  ClientOptions{PingInterval: ping},
		useEdge: true,
	})
	select {
	case <-p.client.Done():
		t.Fatalf("control: the tunnel ended on its own: %v", p.client.Err())
	case err := <-p.hostDone:
		t.Fatalf("control: the host ended on its own: %v", err)
	case <-time.After(4 * ping):
	}
	p.edge.blackhole.Store(true)
	select {
	case <-p.client.Done():
		if !errors.Is(p.client.Err(), ErrPeerUnresponsive) {
			t.Errorf("gateway end: %v, want ErrPeerUnresponsive", p.client.Err())
		}
	case <-time.After(10 * ping):
		t.Fatal("the gateway end never noticed the dead path")
	}
	select {
	case err := <-p.hostDone:
		if !errors.Is(err, ErrPeerUnresponsive) {
			t.Errorf("host end: %v, want ErrPeerUnresponsive", err)
		}
	case <-time.After(10 * ping):
		t.Fatal("the host end never noticed the dead path")
	}
}
