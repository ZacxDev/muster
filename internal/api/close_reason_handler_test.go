package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
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

// streamServer is a server over m whose log the test reads, with the page
// write timeout shortened to 300 ms.
func streamServer(t *testing.T, m Mirror) (*Server, *httptest.Server, *lockedLog) {
	t.Helper()
	saved := terminalWriteTimeout
	terminalWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { terminalWriteTimeout = saved })
	lg := &lockedLog{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(lg, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Mirror: m})
	ts := httptest.NewServer(s.Handler())
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
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
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
func waitCount(t *testing.T, view, reason string, before float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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
			m := &streamMirror{fakeMirror: &fakeMirror{online: true, v2: &fakeMirrorV2{}}, chunk: 1 << 20}
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
	defer page.CloseNow() // alive, and never reads
	c := <-got
	defer forgetPage(c)
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
	if errors.Is(berr, context.DeadlineExceeded) {
		t.Fatalf("CONTROL: the batch itself timed out (%v); the case under test is a batch that finds the socket closed", berr)
	}
	if got := streamEndReason(live, c, berr); got != telemetry.ReasonTimeout {
		t.Errorf("a batch write after another writer's timeout ended as %q, want %q", got, telemetry.ReasonTimeout)
	}
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
