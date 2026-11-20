package api

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/telemetry"
	"github.com/ZacxDev/muster/internal/ui"
)

// EVERY LIVE STREAM A PAGE WATCHES FOR STALENESS BEATS WHILE IT HAS NOTHING TO
// SAY (ui/live.go). A page that has heard nothing for about two heartbeats
// replaces the stream, so a stream that is alive and idle must still speak:
// the Rendered view's socket, both terminals' sockets, the agent chat's socket
// and the /events stream. Each is opened idle here — no batch, no output, no
// turn, no event — and must deliver the heartbeat, exactly as the page
// compares it (ui.LivePingFrame, ui.LivePingEvent), more than once.
//
// Red at origin/main baf851ac: none of them sent anything while idle.

// fastBeat is how often a test's streams beat: on its own servers' sockets
// (socketSettings), never the process's.
const fastBeat = 40 * time.Millisecond

// wantPings reads c until it has seen n heartbeat frames, skipping anything
// else (a terminal's ready and output). A heartbeat must be the exact frame
// the page compares against.
func wantPings(t *testing.T, name string, c *websocket.Conn, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := 0
	for got < n {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("%s: %d of %d heartbeats before %v", name, got, n, err)
		}
		if typ == websocket.MessageText && string(data) == ui.LivePingFrame {
			got++
		}
	}
}

func TestEveryLiveSocketBeatsWhileIdle(t *testing.T) {
	t.Run("rendered view", func(t *testing.T) {
		// fakeMirror with a v2 and no batches: the watch opens and says nothing.
		s, ts, _ := streamServerWith(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}}, &socketSettings{every: fastBeat})
		wantPings(t, "rendered view", dialPage(t, s, ts, viewSock), 3)
	})
	t.Run("mirror terminal", func(t *testing.T) {
		// One chunk of output, then nothing.
		m := &streamMirror{fakeMirror: &fakeMirror{online: true}, chunk: 16, idle: true}
		s, ts, _ := streamServerWith(t, m, &socketSettings{every: fastBeat})
		wantPings(t, "mirror terminal", dialPage(t, s, ts, terminalSock), 3)
	})
	f := newStreamFixtureWith(t, &socketSettings{every: fastBeat})
	at, rt, _ := f.fake.Mint(f.sub)
	dial := func(t *testing.T, path string) *websocket.Conn {
		h := http.Header{}
		h.Set("Cookie", cookieHeader(at, rt))
		h.Set("Origin", f.ts.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatalf("dial %s: %v", path, err)
		}
		t.Cleanup(func() { c.CloseNow() })
		return c
	}
	t.Run("agent terminal", func(t *testing.T) {
		wantPings(t, "agent terminal", dial(t, "/agents/keen-wren/terminal/ws"), 3)
	})
	t.Run("agent chat", func(t *testing.T) {
		wantPings(t, "agent chat", dial(t, "/agents/keen-wren/ws"), 3)
	})
	t.Run("events", func(t *testing.T) {
		req, _ := http.NewRequest("GET", f.ts.URL+"/events", nil)
		req.Header.Set("Cookie", cookieHeader(at, rt))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		got := 0
		for got < 3 {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("events: %d of 3 heartbeats before %v", got, err)
			}
			if strings.TrimRight(line, "\n") == "event: "+ui.LivePingEvent {
				got++
			}
		}
	})
}

// TestCloseReasonsHoldWhileBeating: the heartbeat is one more writer on the
// view's and the terminal's sockets, and HOW those sockets ended must still be
// told apart (#288): a page that left is client_left and a page there but not
// reading is a timeout, whichever writer — a batch, output, or a beat — hit
// it. The close-reason tests, with a beat every 40 ms.
func TestCloseReasonsHoldWhileBeating(t *testing.T) {
	t.Run("stopped reading", func(t *testing.T) { pageThatStopsReadingIsATimeout(t, fastBeat) })
	t.Run("left", func(t *testing.T) { pageThatLeftIsTheBrowserLeavingAtTheHandler(t, fastBeat) })
	t.Run("idle terminal not answering", func(t *testing.T) { idleTerminalWhosePageStoppedAnsweringIsATimeout(t, fastBeat) })
}

// TestTheHeartbeatSettingIsBoundedAndDefaulted: MUSTER_LIVE_HEARTBEAT unset is
// the default; a value outside the bounds refuses (loadConfig refuses to
// boot), and the page is told the value in force (appScript's data-heartbeat,
// from which it derives its staleness bounds).
func TestTheHeartbeatSettingIsBoundedAndDefaulted(t *testing.T) {
	if d, err := ui.ParseLiveHeartbeat(""); err != nil || d != 0 {
		t.Errorf(`ParseLiveHeartbeat("") = %v, %v; want 0 (the default)`, d, err)
	}
	if d, err := ui.ParseLiveHeartbeat("2s"); err != nil || d != 2*time.Second {
		t.Errorf(`ParseLiveHeartbeat("2s") = %v, %v`, d, err)
	}
	for _, bad := range []string{"10ms", "6m", "soon"} {
		if _, err := ui.ParseLiveHeartbeat(bad); err == nil {
			t.Errorf("ParseLiveHeartbeat(%q) was accepted", bad)
		}
	}
	if ui.LiveHeartbeat() != ui.DefaultLiveHeartbeat {
		t.Fatalf("the process default is %v, want %v", ui.LiveHeartbeat(), ui.DefaultLiveHeartbeat)
	}
	if err := ui.SetLiveHeartbeat(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ui.SetLiveHeartbeat(ui.DefaultLiveHeartbeat) })
	s, ts := mirrorServer(t, &fakeMirror{online: true, interactive: true}, newFakeTerminal())
	_, page := mirrorGet(t, s, ts, mainPage)
	if !strings.Contains(page, ui.LiveHeartbeatAttr+`="3000"`) {
		t.Errorf("the page is not told the heartbeat in force (%s=\"3000\")", ui.LiveHeartbeatAttr)
	}
	if err := ui.SetLiveHeartbeat(time.Millisecond); err == nil {
		t.Error("SetLiveHeartbeat(1ms) was accepted")
	}
}

// stalledCount is how many sockets stalledPages holds.
func stalledCount() int {
	n := 0
	stalledPages.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestATimedOutBeatIsForgottenWhenItsSocketEnds: a heartbeat that times out
// notes its socket in the process-wide stalledPages (so the handler can call
// the end a timeout), and the note must go when the socket's handler is done
// — on EVERY socket that beats, including the agent chat's, whose handler
// classifies nothing and never forgot a page itself. Red before the beat's
// stop owned the forget: the chat socket's entry stayed for good (audit #299
// round 1, 🟡1).
func TestATimedOutBeatIsForgottenWhenItsSocketEnds(t *testing.T) {
	// A beat every millisecond to a page that never reads, over small socket
	// buffers: the buffers fill and a beat times out (streamServer's 300 ms
	// write bound) — a real stall, not a forced error.
	var noted atomic.Int64
	s, ts, _ := streamServerWith(t, &fakeMirror{online: true}, &socketSettings{every: time.Millisecond, stallNoted: func() { noted.Add(1) }})
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: chatAgents{}, SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), Gateway: stubGateway{}, Mirror: &fakeMirror{online: true}})
	base := stalledCount()
	c := dialPage(t, s, ts, "/agents/keen-wren/ws") // and never reads
	// Positive control: a beat DID time out (its note may be forgotten
	// moments later, once the handler is done).
	deadline := time.Now().Add(10 * time.Second)
	for noted.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("control: no beat to the unread chat socket ever timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = c.CloseNow()
	deadline = time.Now().Add(5 * time.Second)
	for stalledCount() > base {
		if time.Now().After(deadline) {
			t.Fatalf("the chat socket's stalled note outlived its handler: %d entries, want %d", stalledCount(), base)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAnIdlePageThatStopsReadingIsATimeoutWhenOnlyTheBeatWrites: an idle
// Rendered view (a quiet conversation) or terminal (a TUI printing nothing)
// whose page is there but not reading has ONE writer — the heartbeat. Its
// timed-out write notes the socket and ends it, and the handler must still
// count that end as a timeout (#288), not the page leaving. That holds only
// while the note outlives the handler's classification: the beat's stop
// forgets it, and is deferred in the HANDLER (audit #299 round 2, 🟡1).
//
// The terminal's own keepalive ping is pushed out of the test's time, so the
// beat is the only writer. A single socket races the handler against the
// note's forget under a broken ordering (not every time), so SOCKETS of each
// kind are opened at once and EVERY end must be a timeout.
//
// ⚠ The agent pod's terminal is not covered: its handler classifies no end.
func TestAnIdlePageThatStopsReadingIsATimeoutWhenOnlyTheBeatWrites(t *testing.T) {
	const sockets = 64
	for _, c := range []struct {
		view, path string
		m      Mirror
	}{
		{openViewMirror, viewSock, &fakeMirror{online: true, v2: &fakeMirrorV2{}}},
		{openViewTerminal, terminalSock, &streamMirror{fakeMirror: &fakeMirror{online: true}, chunk: 16, idle: true}},
	} {
		t.Run(c.view, func(t *testing.T) {
			var noted atomic.Int64
			// A beat every millisecond; the terminal's keepalive pushed out of the test's time.
			s, ts, _ := streamServerWith(t, c.m, &socketSettings{every: time.Millisecond, ping: time.Hour, stallNoted: func() { noted.Add(1) }})
			before := disconnects(c.view)
			for i := 0; i < sockets; i++ {
				_ = dialPage(t, s, ts, c.path) // and never reads
			}
			deadline := time.Now().Add(20 * time.Second)
			for {
				after := disconnects(c.view)
				ended := 0.0
				for r := range after {
					ended += after[r] - before[r]
				}
				if ended >= sockets {
					if got := after[telemetry.ReasonTimeout] - before[telemetry.ReasonTimeout]; got != sockets {
						t.Errorf("%v of %d idle sockets whose page stopped reading ended as a timeout; the rest: client_left %v, error %v",
							got, sockets, after[telemetry.ReasonClientLeft]-before[telemetry.ReasonClientLeft], after[telemetry.ReasonError]-before[telemetry.ReasonError])
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("only %v of %d sockets ended (beats that timed out: %d)", ended, sockets, noted.Load())
				}
				time.Sleep(10 * time.Millisecond)
			}
			// Control: it was the BEAT that timed out (the only writer).
			if noted.Load() == 0 {
				t.Error("control: no beat ever timed out")
			}
		})
	}
}

// TestAHealthyBeatIsNotSlowedAndLeavesNothingBehind: the beat's stall timer
// (writePing) must cost a page that IS reading nothing. Over a second of beats
// every 5 ms — more than three times the 300 ms stall bound — each beat is
// written on time, no stall is ever noted (a timer left running would fire at
// its bound, note a stall and close the socket), the socket stays open, and
// once it ends every goroutine it started is gone.
func TestAHealthyBeatIsNotSlowedAndLeavesNothingBehind(t *testing.T) {
	var noted atomic.Int64
	s, ts, _ := streamServerWith(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}}, &socketSettings{every: 5 * time.Millisecond, stallNoted: func() { noted.Add(1) }})
	time.Sleep(100 * time.Millisecond)
	g0 := runtime.NumGoroutine()
	c := dialPage(t, s, ts, viewSock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	beats := 0
	var worst time.Duration
	last := start
	for time.Since(start) < time.Second {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("the socket ended after %d beats: %v", beats, err)
		}
		if typ == websocket.MessageText && string(data) == ui.LivePingFrame {
			now := time.Now()
			if gap := now.Sub(last); gap > worst {
				worst = gap
			}
			last = now
			beats++
		}
	}
	if beats < 50 {
		t.Errorf("%d beats in a second at one per 5 ms: the beat is slowed", beats)
	}
	if worst > 250*time.Millisecond {
		t.Errorf("a %v gap between beats: a healthy beat waited on something", worst)
	}
	if n := noted.Load(); n != 0 {
		t.Errorf("%d stalls noted on a page that reads every beat: a stall timer outlived its write", n)
	}
	_ = c.CloseNow()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > g0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after the socket ended, %d before it opened", runtime.NumGoroutine(), g0)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAStallCallbackFinishesBeforeTheBeatReturns: when a beat's stall timer
// has FIRED (Stop reports false), its callback has started — not necessarily
// finished. A beat that returned then could be forgotten (stop's forgetPage)
// before the callback's note landed, leaving the socket in stalledPages for
// good, or noting a stall after the handler classified its end (audit #299
// round 3, 🟡1). So writePing waits for the callback. The order is forced:
// the write starts only once the callback has begun, and the callback is held
// before it notes; the beat must not return until it is let go, and the note
// is there when it does.
func TestAStallCallbackFinishesBeforeTheBeatReturns(t *testing.T) {
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
	defer page.CloseNow()
	go func() { // a page that reads: the write itself succeeds
		for {
			if _, _, err := page.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	c := <-got
	defer forgetPage(c)

	entered, release := make(chan struct{}), make(chan struct{})
	seams := &socketSettings{
		stallFiring: func() { close(entered); <-release },
		beatWrite: func() { // the timer has fired before the write
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Error("the stall timer never fired before the beat's write (its stallFiring seam never ran)")
			}
		},
	}

	done := make(chan error, 1)
	go func() { done <- writePing(withSocketSettings(context.Background(), seams), c, time.Nanosecond) }()
	select {
	case <-done:
		close(release)
		t.Fatal("the beat returned while its stall callback had not yet noted the stall")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-done; err == nil {
		t.Error("a beat whose stall timer fired reported success")
	}
	if !pageStalled(c) {
		t.Error("the beat returned without its stall noted")
	}
}

// TestAWriteSeamReachesOnlyTheWritesMadeOnItsContext: a beat another test
// left running (socketHeartbeat's stop does not wait for its goroutine, and
// nothing waits for a hijacked socket's handler) must never run THIS test's
// seams. When the seams were package variables it did, racing the test that
// assigned them (TestAStallCallbackFinishesBeforeTheBeatReturns on CI: the
// test's assignment against writePing's call from socketHeartbeat's
// goroutine). Here a foreign beat writes every millisecond on the same socket
// while this test makes one write of its own on a context carrying seams: the
// seam runs exactly once.
func TestAWriteSeamReachesOnlyTheWritesMadeOnItsContext(t *testing.T) {
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
	defer page.CloseNow()
	var pings atomic.Int64
	go func() { // a page that reads, and counts the beats
		for {
			_, data, err := page.Read(context.Background())
			if err != nil {
				return
			}
			if string(data) == ui.LivePingFrame {
				pings.Add(1)
			}
		}
	}()
	c := <-got
	defer forgetPage(c)

	foreign := withSocketSettings(context.Background(), &socketSettings{every: time.Millisecond})
	stop := socketHeartbeat(foreign, log.New(io.Discard, "", 0), c) // the foreign beat
	t.Cleanup(stop)

	var ran atomic.Int64
	seams := &socketSettings{beatWrite: func() { ran.Add(1) }}
	if err := writePing(withSocketSettings(context.Background(), seams), c, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for pings.Load() < 20 { // the foreign beat has written well past this test's write
		if time.Now().After(deadline) {
			t.Fatalf("control: the foreign beat wrote %d beats, want 20", pings.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the seam ran %d times over one write of its own and %d beats of another's: want 1", n, pings.Load())
	}
}

// TestTheViewsCloseDetectionIsItsOwnServers: a server's replaced close
// detection (socketSettings.closeRead) is what its own view sockets open with,
// and no other server's: sockets opened on a server without one never run it,
// and each socket opened on the server with one runs it exactly once — also
// after this server's sockets have opened with it. It was a package variable a
// test reassigned while another test's view handler read it as its socket
// opened — a data race (close_reason_handler_test.go against
// handleMirrorViewWS).
func TestTheViewsCloseDetectionIsItsOwnServers(t *testing.T) {
	var ran atomic.Int64
	own := &socketSettings{closeRead: func(c *websocket.Conn, ctx context.Context) context.Context {
		ran.Add(1)
		return c.CloseRead(ctx)
	}}
	s, ts, _ := streamServerWith(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}}, own)
	other, ots, _ := streamServerWith(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}}, &socketSettings{every: fastBeat})
	waitRan := func(n int64, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for ran.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%s: this server's close detection ran %d times, want %d", what, ran.Load(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	_ = dialPage(t, s, ts, viewSock)
	waitRan(1, "this server's first view socket")
	// Another server's view sockets, opened after this server's: open and
	// beating (so their handlers are past the open), never running the seam.
	for i := 0; i < 3; i++ {
		wantPings(t, "other server's view", dialPage(t, other, ots, viewSock), 1)
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("1 view socket here and 3 on another server ran this server's close detection %d times: want 1", n)
	}
	_ = dialPage(t, s, ts, viewSock)
	waitRan(2, "this server's second view socket")
	time.Sleep(50 * time.Millisecond)
	if n := ran.Load(); n != 2 {
		t.Errorf("2 view sockets here ran this server's close detection %d times: want 2", n)
	}
}
