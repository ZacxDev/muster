package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// HOW A VIEW'S OR A TERMINAL'S SOCKET ENDED, through the real handlers: a page
// that LEFT is client_left, and a page that is THERE BUT NOT READING is a
// timeout — never the other way round, whichever writer hit it.

// streamMirror is the plugin, played as a host that never stops sending: its
// watch writes batches and its terminal writes output until a write to the
// page fails, and records that failure — what the handler's writer returned.
type streamMirror struct {
	*fakeMirror
	chunk int
	// idle: the terminal writes one chunk and then nothing, until it is ended.
	idle bool
	mu   sync.Mutex
	errs []error
}

func (m *streamMirror) record(err error) error {
	m.mu.Lock()
	m.errs = append(m.errs, err)
	m.mu.Unlock()
	return err
}

func (m *streamMirror) lastErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.errs) == 0 {
		return nil
	}
	return m.errs[len(m.errs)-1]
}

func (m *streamMirror) WatchWindow(_ context.Context, _, _, _, _ string, _ int64, out func(mirror.Msg) error) error {
	text := strings.Repeat("x", m.chunk)
	for i := 0; ; i++ {
		ev := cctranscript.Event{Kind: cctranscript.KindText, Text: text}
		if err := out(mirror.Msg{Offset: int64(i + 1), Reset: i == 0, Gen: "g", Events: []cctranscript.Event{ev}}); err != nil {
			return m.record(err)
		}
	}
}

func (m *streamMirror) AttachWindow(ctx context.Context, _, _, _ string, _ bool, _ io.Reader, stdout io.Writer, _ <-chan provision.TermSize) error {
	p := bytes.Repeat([]byte("y"), m.chunk)
	if m.idle {
		if _, err := stdout.Write(p); err != nil {
			return m.record(err)
		}
		<-ctx.Done()
		return nil
	}
	for {
		if _, err := stdout.Write(p); err != nil {
			return m.record(err)
		}
	}
}

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// smallSocket is the kernel buffer size, each way, of the sockets between the
// handler and the page: small, so a page that stops reading stalls the
// handler's next write at once, rather than after the kernel has taken
// megabytes of loopback buffering (which, rendered under -race on a loaded
// runner, took longer than the test was willing to wait).
const smallSocket = 4 << 10

type smallListener struct{ net.Listener }

func (l smallListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(smallSocket)
		_ = tc.SetReadBuffer(smallSocket)
	}
	return c, err
}

// streamServer is a server over m whose log the test reads, with the page
// write timeout shortened to 300 ms and small socket buffers.
func streamServer(t *testing.T, m Mirror) (*Server, *httptest.Server, *lockedLog) {
	t.Helper()
	saved := terminalWriteTimeout
	terminalWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { terminalWriteTimeout = saved })
	lg := &lockedLog{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(lg, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Mirror: m})
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener = smallListener{ts.Listener}
	ts.Start()
	t.Cleanup(ts.Close)
	return s, ts, lg
}

func dialPage(t *testing.T, s *Server, ts *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(s))
	h.Set("Origin", ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(smallSocket)
			_ = tc.SetWriteBuffer(smallSocket)
		}
		return c, err
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dial}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(-1)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

const (
	viewSock     = "/mirror/view/ws?host=ws&session=main"
	terminalSock = "/mirror/terminal/ws?host=ws&session=main&mode=ro&cols=80&rows=24"
)

// waitCount waits until the counter for (view, reason) has moved from before.
// The ceiling is generous: it is reached only when the counter never moves.
func waitCount(t *testing.T, view, reason string, before float64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for testutil.ToFloat64(metrics.ViewDisconnects.WithLabelValues(view, reason)) == before {
		if time.Now().After(deadline) {
			t.Fatalf("muster_view_disconnects_total{view=%q,reason=%q} never moved", view, reason)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func disconnects(view string) map[string]float64 {
	out := map[string]float64{}
	for _, r := range []string{telemetry.ReasonClientLeft, telemetry.ReasonTimeout, telemetry.ReasonError} {
		out[r] = testutil.ToFloat64(metrics.ViewDisconnects.WithLabelValues(view, r))
	}
	return out
}

// TestAPageThatStopsReadingIsATimeout: a live page that never reads — the
// view's and the terminal's — fills the socket until a write hits
// terminalWriteTimeout. That end is "timeout" (an error, logged for the
// view), not the browser leaving.
func TestAPageThatStopsReadingIsATimeout(t *testing.T) {
	for _, c := range []struct{ view, path string }{{openViewMirror, viewSock}, {openViewTerminal, terminalSock}} {
		t.Run(c.view, func(t *testing.T) {
			m := &streamMirror{fakeMirror: &fakeMirror{online: true, v2: &fakeMirrorV2{}}, chunk: 64 << 10}
			s, ts, lg := streamServer(t, m)
			before := disconnects(c.view)
			_ = dialPage(t, s, ts, c.path) // and never read
			waitCount(t, c.view, telemetry.ReasonTimeout, before[telemetry.ReasonTimeout])
			after := disconnects(c.view)
			for _, r := range []string{telemetry.ReasonClientLeft, telemetry.ReasonError} {
				if after[r] != before[r] {
					t.Errorf("the stalled page was also counted as %q", r)
				}
			}
			if !errors.Is(m.lastErr(), context.DeadlineExceeded) || errors.Is(m.lastErr(), errPageGone) {
				t.Errorf("the writer's failure is %v: want the timeout, not marked as the page gone", m.lastErr())
			}
			if c.view == openViewMirror && !strings.Contains(lg.String(), "the page stopped reading") {
				t.Errorf("the view's log has no line for the stalled page:\n%s", lg.String())
			}
		})
	}
}

// TestAPageThatLeftIsTheBrowserLeavingAtTheHandler: a page that CLOSED its
// socket while the stream was writing ends client_left, through the real
// handlers — and the write that found it gone is marked so by the handler's
// own writer (the view's writeToPage, the terminal's pageWriter). For the
// view, the window the race lives in is held open: close detection is
// replaced so the stream's context stays live after the page has gone, and
// no error frame or error line is produced for a page that is not there.
func TestAPageThatLeftIsTheBrowserLeavingAtTheHandler(t *testing.T) {
	for _, c := range []struct{ view, path string }{{openViewMirror, viewSock}, {openViewTerminal, terminalSock}} {
		t.Run(c.view, func(t *testing.T) {
			if c.view == openViewMirror {
				saved := viewCloseRead
				viewCloseRead = func(_ *websocket.Conn, ctx context.Context) context.Context { return ctx }
				t.Cleanup(func() { viewCloseRead = saved })
			}
			m := &streamMirror{fakeMirror: &fakeMirror{online: true, v2: &fakeMirrorV2{}}, chunk: 4 << 10}
			s, ts, lg := streamServer(t, m)
			before := disconnects(c.view)
			page := dialPage(t, s, ts, c.path)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if _, _, err := page.Read(ctx); err != nil {
				t.Fatal(err)
			}
			cancel()
			_ = page.CloseNow() // the page leaves while the host is still sending
			waitCount(t, c.view, telemetry.ReasonClientLeft, before[telemetry.ReasonClientLeft])
			after := disconnects(c.view)
			for _, r := range []string{telemetry.ReasonTimeout, telemetry.ReasonError} {
				if after[r] != before[r] {
					t.Errorf("the page that left was also counted as %q", r)
				}
			}
			if !errors.Is(m.lastErr(), errPageGone) {
				t.Errorf("the handler's writer returned %v: want it marked as the page gone", m.lastErr())
			}
			if strings.Contains(lg.String(), "the rendered view ended with an error") || strings.Contains(lg.String(), "mirror: view ws/main:") {
				t.Errorf("a page that left was logged as an error:\n%s", lg.String())
			}
		})
	}
}

// TestAnotherWritersTimeoutMakesTheNextWriteATimeout: the asks relay and the
// suggested replies write to the view's socket with terminalWriteJSON. When
// one of those writes times out (the page is there, not reading), the
// connection is closed — so the NEXT batch write fails as if the page had
// gone. The end is still a timeout: the socket remembers it stalled.
func TestAnotherWritersTimeoutMakesTheNextWriteATimeout(t *testing.T) {
	saved := terminalWriteTimeout
	terminalWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { terminalWriteTimeout = saved })
	c := unreadPage(t)
	live := context.Background()
	big := map[string]string{"type": "asks", "pad": strings.Repeat("z", 256<<10)}
	var aerr error
	for i := 0; i < 400 && aerr == nil; i++ {
		aerr = terminalWriteJSON(live, c, big) // the asks relay's / tell's write
	}
	if !errors.Is(aerr, context.DeadlineExceeded) {
		t.Fatalf("CONTROL: the other writer's write never timed out (%v)", aerr)
	}
	berr := writeToPage(live, c, map[string]string{"type": "batch"}) // the next batch
	if berr == nil {
		t.Fatal("the batch after the timeout was written")
	}
	if !errors.Is(berr, net.ErrClosed) {
		t.Fatalf("CONTROL: the batch did not find the socket closed (%v); that is the case under test", berr)
	}
	if !errors.Is(berr, context.DeadlineExceeded) || errors.Is(berr, errPageGone) {
		t.Errorf("the batch after another writer's timeout returned %v: want the timeout, not marked as the page gone", berr)
	}
	if got := streamEndReason(live, c, berr); got != telemetry.ReasonTimeout {
		t.Errorf("a batch write after another writer's timeout ended as %q, want %q", got, telemetry.ReasonTimeout)
	}
}

// TestAStreamWriteCutByAStalledBeatIsATimeout: the heartbeat is one more
// writer on the terminal's and the view's socket, with its own bound
// (writePing). When the BEAT is the write that stalls — queued behind the
// stream's own write on a full socket, its bound reached while the stream
// write's deadline is still ahead — it notes the stall and closes the
// connection, and the stream's in-flight write fails on the closed connection.
// That write's failure is the page having stopped reading: a timeout, never
// the page gone.
//
// Issue #710: which writer's deadline came first was a race in
// TestCloseReasonsHoldWhileBeating (both 300 ms, the beat's armed while it
// waited for the write lock), and a stream writer that lost it returned "the
// page's socket is closed". Here the order is fixed, not raced: the stream's
// write has 30 s, the beat 100 ms.
func TestAStreamWriteCutByAStalledBeatIsATimeout(t *testing.T) {
	saved := terminalWriteTimeout
	terminalWriteTimeout = 30 * time.Second
	t.Cleanup(func() { terminalWriteTimeout = saved })
	pad := strings.Repeat("z", 256<<10)
	for _, w := range []struct {
		name  string
		write func(context.Context, *websocket.Conn) error
	}{
		{"terminal output", func(ctx context.Context, c *websocket.Conn) error {
			// As the terminal handler wires it (mirror.go): the page writer
			// over the bridge's terminalWriter.
			_, err := (pageWriter{w: &terminalWriter{ctx: ctx, c: c, ready: true}, c: c}).Write([]byte(pad))
			return err
		}},
		{"view batch", func(ctx context.Context, c *websocket.Conn) error {
			return writeToPage(ctx, c, map[string]string{"type": "batch", "pad": pad})
		}},
	} {
		t.Run(w.name, func(t *testing.T) {
			c := unreadPage(t)
			live := context.Background()
			streamErr := make(chan error, 1)
			go func() {
				var err error
				for err == nil {
					err = w.write(live, c) // the host never stops sending
				}
				streamErr <- err
			}()
			// The beat, until one stalls: it succeeds while the socket has
			// room, and once the stream's write holds a full socket it waits
			// behind it and its bound fires.
			var perr error
			for i := 0; i < 10000 && perr == nil; i++ {
				perr = writePing(live, c, 100*time.Millisecond)
			}
			// The stream's write has 30 s, so a note here is the beat's.
			if !pageStalled(c) {
				t.Fatalf("CONTROL: the beat ended (%v) without noting a stall", perr)
			}
			var serr error
			select {
			case serr = <-streamErr:
			case <-time.After(10 * time.Second):
				t.Fatal("the stream's write outlived the connection the beat closed")
			}
			if !errors.Is(serr, net.ErrClosed) {
				t.Fatalf("CONTROL: the stream's write did not fail on the closed connection (%v); that is the case under test", serr)
			}
			if !errors.Is(serr, context.DeadlineExceeded) || errors.Is(serr, errPageGone) {
				t.Errorf("the stream's write cut by a stalled beat returned %v: want the timeout, not marked as the page gone", serr)
			}
			if got := streamEndReason(live, c, serr); got != telemetry.ReasonTimeout {
				t.Errorf("a stream write cut by a stalled beat ended as %q, want %q", got, telemetry.ReasonTimeout)
			}
		})
	}
}

// relayWrite tags the asks relay's write in TestABatchQueuedBehindTheAsksRelaysStallIsATimeout.
type relayWrite struct{}

// TestABatchQueuedBehindTheAsksRelaysStallIsATimeout: the asks relay writes
// to the view's socket with terminalWriteJSON. When ITS write stalls on a page
// that stopped reading while a batch is queued behind it, the relay's bound
// closes the connection and the queued batch fails on the closed connection
// at once — before the relay's write has even returned. That batch's failure
// is the timeout, never the page gone: the stall must be noted BEFORE the
// close can be seen, not after the relay's write returns (audit #713 F1).
//
// The relay is held after its write returns (pageWriteReturned) until the
// batch has failed, so a note written only then is too late, every run.
func TestABatchQueuedBehindTheAsksRelaysStallIsATimeout(t *testing.T) {
	saved := terminalWriteTimeout
	terminalWriteTimeout = 30 * time.Second // the batch's own bound, never reached
	t.Cleanup(func() { terminalWriteTimeout = saved })
	c := unreadPage(t)
	live := context.Background()
	batchErr, batchFailed := make(chan error, 1), make(chan struct{})
	savedHook := pageWriteReturned
	pageWriteReturned = func(ctx context.Context) {
		if ctx.Value(relayWrite{}) == nil {
			return
		}
		select {
		case <-batchFailed:
		case <-time.After(10 * time.Second):
		}
	}
	relayWriting := make(chan struct{})
	savedStart := pageWriteStarting
	pageWriteStarting = func(ctx context.Context) {
		if ctx.Value(relayWrite{}) != nil {
			close(relayWriting)
		}
	}
	// And the relay's stall timer is held — until the batch has failed, or
	// 300 ms, well inside stallGrace — before it notes and closes: a close by
	// anything else at the bound (coder/websocket's own, on the write's
	// context) is then seen by the batch with no note, every run. Only the
	// relay's write can stall here: the batch's bound is 30 s.
	savedFiring := stallFiring
	stallFiring = func() {
		select {
		case <-batchFailed:
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { pageWriteReturned, pageWriteStarting, stallFiring = savedHook, savedStart, savedFiring })

	// The relay: one frame far larger than the socket's buffers, so it holds
	// the write lock mid-frame, bounded at 2 s.
	rctx, rcancel := context.WithTimeout(context.WithValue(live, relayWrite{}, true), 2*time.Second)
	defer rcancel()
	relayErr := make(chan error, 1)
	go func() {
		relayErr <- terminalWriteJSON(rctx, c, map[string]string{"type": "asks", "pad": strings.Repeat("z", 16<<20)})
	}()
	// The batches, once the relay is writing: small, so they cannot fill the
	// socket themselves; one ends up queued behind the relay's frame.
	<-relayWriting
	go func() {
		var err error
		for err == nil {
			err = writeToPage(live, c, map[string]string{"type": "batch"})
		}
		batchErr <- err
		close(batchFailed)
	}()
	var berr, rerr error
	select {
	case berr = <-batchErr:
	case <-time.After(10 * time.Second):
		t.Fatal("the queued batch never failed")
	}
	select {
	case rerr = <-relayErr:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay's write never returned")
	}
	if !errors.Is(rerr, context.DeadlineExceeded) {
		t.Fatalf("CONTROL: the relay's write did not time out (%v)", rerr)
	}
	if !errors.Is(berr, net.ErrClosed) {
		t.Fatalf("CONTROL: the batch did not fail on the closed connection (%v); that is the case under test", berr)
	}
	if !errors.Is(berr, context.DeadlineExceeded) || errors.Is(berr, errPageGone) {
		t.Errorf("the batch queued behind the relay's stall returned %v: want the timeout, not marked as the page gone", berr)
	}
}

// TestAnUnansweredKeepaliveWaitsTheFullWriteTimeout: the terminal keepalive's
// ping, whose frame is sent but which the page never answers, waits
// terminalWriteTimeout (production's 10 s) for the pong before it ends the
// terminal — and the end is noted as a stall. Pinned because bounding the
// ping with armStall (audit #713 round 1) silently cut that wait to 4 s, and
// the shorter wait was declined (2026-11-16): the keepalive stays the one page
// write outside armStall (see bridgeTerminal).
//
// Red at dc6b8ace (the bridge ended at 4.1 s).
func TestAnUnansweredKeepaliveWaitsTheFullWriteTimeout(t *testing.T) {
	savedT, savedI := terminalWriteTimeout, terminalPingInterval
	terminalWriteTimeout, terminalPingInterval = 10*time.Second, 100*time.Millisecond
	t.Cleanup(func() { terminalWriteTimeout, terminalPingInterval = savedT, savedI })
	c := unreadPage(t) // the page never reads, so never answers a ping
	attach := func(ctx context.Context, _ io.Reader, _ io.Writer, _ <-chan provision.TermSize) error {
		<-ctx.Done() // an idle terminal: no output, so the ping is the only writer
		return nil
	}
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- bridgeTerminal(context.Background(), log.New(io.Discard, "", 0), c, "", true, provision.TermSize{}, false, attach)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the unanswered keepalive never ended the terminal")
	}
	took := time.Since(start)
	if !pageStalled(c) {
		t.Errorf("the terminal ended after %v without its unanswered ping noted as a stall", took)
	}
	if took < 9500*time.Millisecond {
		t.Errorf("an unanswered keepalive ended the terminal after %v: want the full write timeout (≥ 9.5 s with a 100 ms ping interval)", took)
	}
}

// unreadPage is the server side of a websocket whose page is alive and never
// reads; its stalled note is forgotten when the test ends.
func unreadPage(t *testing.T) *websocket.Conn {
	t.Helper()
	got := make(chan *websocket.Conn, 1)
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		got <- c
		<-hold
	}))
	t.Cleanup(func() { close(hold); srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = page.CloseNow() }) // alive, and never reads
	c := <-got
	t.Cleanup(func() { forgetPage(c) })
	return c
}

// TestAnIdleTerminalWhosePageStoppedAnsweringIsATimeout: a terminal with
// nothing to print, whose page is there and never reads, is found by the
// keepalive: its ping is never answered and times out, which ends the bridge.
// That is a timeout too — not the browser leaving, which is what the bridge's
// context ending otherwise means.
func TestAnIdleTerminalWhosePageStoppedAnsweringIsATimeout(t *testing.T) {
	saved := terminalPingInterval
	terminalPingInterval = 100 * time.Millisecond
	t.Cleanup(func() { terminalPingInterval = saved })
	m := &streamMirror{fakeMirror: &fakeMirror{online: true}, chunk: 16, idle: true}
	s, ts, _ := streamServer(t, m)
	before := disconnects(openViewTerminal)
	_ = dialPage(t, s, ts, terminalSock) // and never reads, so never answers a ping
	waitCount(t, openViewTerminal, telemetry.ReasonTimeout, before[telemetry.ReasonTimeout])
	if after := disconnects(openViewTerminal); after[telemetry.ReasonClientLeft] != before[telemetry.ReasonClientLeft] {
		t.Error("the unanswering page was also counted as client_left")
	}
}
