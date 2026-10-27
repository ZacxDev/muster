package api

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// fastBeat makes every stream beat every 40 ms for the test.
func fastBeat(t *testing.T) {
	t.Helper()
	saved := liveHeartbeatEvery
	liveHeartbeatEvery = func() time.Duration { return 40 * time.Millisecond }
	t.Cleanup(func() { liveHeartbeatEvery = saved })
}

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
	fastBeat(t)
	t.Run("rendered view", func(t *testing.T) {
		// fakeMirror with a v2 and no batches: the watch opens and says nothing.
		s, ts, _ := streamServer(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}})
		wantPings(t, "rendered view", dialPage(t, s, ts, viewSock), 3)
	})
	t.Run("mirror terminal", func(t *testing.T) {
		// One chunk of output, then nothing.
		m := &streamMirror{fakeMirror: &fakeMirror{online: true}, chunk: 16, idle: true}
		s, ts, _ := streamServer(t, m)
		wantPings(t, "mirror terminal", dialPage(t, s, ts, terminalSock), 3)
	})
	f := newStreamFixture(t)
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
	fastBeat(t)
	t.Run("stopped reading", TestAPageThatStopsReadingIsATimeout)
	t.Run("left", TestAPageThatLeftIsTheBrowserLeavingAtTheHandler)
	t.Run("idle terminal not answering", TestAnIdleTerminalWhosePageStoppedAnsweringIsATimeout)
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
