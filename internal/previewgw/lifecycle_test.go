package previewgw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// limEnv drives the open limits through either opener: an agent's
// preview_open on the offer stream, or Open on an offer on the session page.
type limEnv struct {
	e    *gwEnv
	app  int
	fhs  map[string]*fakeHost
	ls   map[string][]wire.Listener
	toks map[string]string
	n    int
}

// host dials host for owner; maxOpen > 0 is sent as its config's maxOpen,
// 0 sends no config (the gateway's default applies).
func (l *limEnv) host(t *testing.T, owner int64, host string, maxOpen int) {
	t.Helper()
	fh := l.e.mustDial(t, dialOpts{listener: "public", token: l.e.mint(t, owner, host), hostName: host})
	if maxOpen > 0 {
		fh.offers <- wire.Offer{Type: wire.TypeConfig, Config: &wire.Config{Ports: []string{"5000-5999"}, Offer: true, MaxOpen: maxOpen, MaxConns: 64}}
		waitFor(t, "the config", func() bool { return l.e.tunnelFor(owner, host).stateConfig() != nil })
	}
	l.fhs[host] = fh
}

type opener func(t *testing.T, l *limEnv, owner int64, host, window string, port int) (id, code string)

func agentOpener(t *testing.T, l *limEnv, owner int64, host, window string, port int) (string, string) {
	t.Helper()
	l.n++
	rid := "r" + strconv.Itoa(l.n)
	fh := l.fhs[host]
	fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: rid, Window: window, Port: port}}
	for {
		select {
		case a := <-fh.answers:
			if a.a.RequestID == rid {
				return a.a.Preview, a.a.Error
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no answer to open %s", rid)
		}
	}
}

func offerOpener(t *testing.T, l *limEnv, owner int64, host, window string, port int) (string, string) {
	t.Helper()
	l.e.serve(port, l.app)
	l.ls[host] = append(l.ls[host], wire.Listener{Port: port, Process: "vite", Window: window})
	l.e.relist(t, l.fhs[host], owner, host, l.ls[host]...)
	key := fmt.Sprint(owner, host, window)
	tok, ok := l.toks[key]
	if !ok {
		a := l.e.enter(t, l.e.fm.handoff("mpl_owner"+itoa(owner), owner, host, "main", window), owner*10)
		tok = pageToken(t, a)
		l.toks[key] = tok
	}
	a := l.e.do(t, "POST", webHost(), openPath, bearer(tok), `{"port":`+strconv.Itoa(port)+`}`)
	if a.status == http.StatusOK {
		var out struct{ Preview string }
		_ = json.Unmarshal([]byte(a.body), &out)
		return out.Preview, ""
	}
	return "", a.body
}

func (l *limEnv) mustOpen(t *testing.T, open opener, owner int64, host, window string, port int) string {
	t.Helper()
	id, code := open(t, l, owner, host, window, port)
	if code != "" || id == "" {
		t.Fatalf("open %s %s :%d: %q; want it opened", host, window, port, code)
	}
	return id
}

func (l *limEnv) mustRefuse(t *testing.T, open opener, owner int64, host, window string, port int, why string) {
	t.Helper()
	if id, code := open(t, l, owner, host, window, port); code != codeMaxOpen {
		t.Fatalf("open %s %s :%d past %s: id %q code %q; want %s", host, window, port, why, id, code, codeMaxOpen)
	}
}

// 🔴 The open limits hold on BOTH openers: 4 per window, 16 per owner, and
// the host's maxOpen (7 here, from its config; 8 when it sent none). Each
// limit is reached with the others slack (another window, owner or host
// still opens), and a repeat of an open preview answers its id uncounted.
func TestTheOpenLimitsHoldOnBothOpeners(t *testing.T) {
	for name, open := range map[string]opener{"agent": agentOpener, "offer": offerOpener} {
		setup := func(t *testing.T) *limEnv {
			e := newGW(t, gwOpts{tokens: []string{"mpl_owner1", "mpl_owner2"}})
			return &limEnv{e: e, app: appServer(t), fhs: map[string]*fakeHost{}, ls: map[string][]wire.Listener{}, toks: map[string]string{}}
		}
		t.Run(name+"/per window", func(t *testing.T) {
			l := setup(t)
			l.host(t, 1, "alpha", 30)
			first := l.mustOpen(t, open, 1, "alpha", "@1", 5001)
			for p := 5002; p <= 5004; p++ {
				l.mustOpen(t, open, 1, "alpha", "@1", p)
			}
			if id := l.mustOpen(t, open, 1, "alpha", "@1", 5001); id != first {
				t.Fatalf("a repeat open answered %q; want the open preview's %q", id, first)
			}
			l.mustRefuse(t, open, 1, "alpha", "@1", 5005, "4 in the window")
			l.mustOpen(t, open, 1, "alpha", "@2", 5005)
		})
		t.Run(name+"/per owner", func(t *testing.T) {
			l := setup(t)
			l.host(t, 1, "alpha", 30)
			l.host(t, 1, "beta", 30)
			l.host(t, 2, "gamma", 30)
			port := 5100
			for _, hw := range []struct{ host, window string }{{"alpha", "@1"}, {"alpha", "@2"}, {"alpha", "@3"}, {"beta", "@1"}} {
				for range 4 {
					port++
					l.mustOpen(t, open, 1, hw.host, hw.window, port)
				}
			}
			l.mustRefuse(t, open, 1, "beta", "@2", 5200, "16 for the owner")
			l.mustOpen(t, open, 2, "gamma", "@1", 5201)
		})
		t.Run(name+"/per host, from its config", func(t *testing.T) {
			l := setup(t)
			l.host(t, 1, "alpha", 7)
			l.host(t, 1, "beta", 30)
			port := 5300
			for _, c := range []struct {
				window string
				n      int
			}{{"@1", 4}, {"@2", 3}} {
				for range c.n {
					port++
					l.mustOpen(t, open, 1, "alpha", c.window, port)
				}
			}
			l.mustRefuse(t, open, 1, "alpha", "@3", 5400, "the host's maxOpen of 7")
			l.mustOpen(t, open, 1, "beta", "@3", 5401)
		})
		t.Run(name+"/per host, default", func(t *testing.T) {
			l := setup(t)
			l.host(t, 1, "alpha", 0)
			l.host(t, 1, "beta", 30)
			port := 5500
			for _, w := range []string{"@1", "@2"} {
				for range 4 {
					port++
					l.mustOpen(t, open, 1, "alpha", w, port)
				}
			}
			l.mustRefuse(t, open, 1, "alpha", "@3", 5600, "the default maxOpen of 8")
			l.mustOpen(t, open, 1, "beta", "@3", 5601)
		})
	}
}

// 🔴 Open on an offer probes the port on the host's tunnel BEFORE it records
// the preview: nothing listening is not_listening, a port the host refuses is
// its reason, and neither leaves a preview or tells the host. A listening
// port opens (positive control).
func TestOpenOnAnOfferProbesBeforeRecording(t *testing.T) {
	e := newGW(t, gwOpts{})
	dead := closedPort(t)
	e.refuse.Store(5702, wire.ReasonPortNotAllowed)
	e.serve(5703, appServer(t))
	fh := e.hostFor(t, 1, "alpha", wire.Listener{Port: dead, Process: "gone", Window: "@1"},
		wire.Listener{Port: 5702, Process: "vite", Window: "@1"}, wire.Listener{Port: 5703, Process: "vite", Window: "@1"})
	tok := e.session(t, "@1")
	for _, c := range []struct {
		port   int
		status int
		code   string
	}{{dead, http.StatusBadGateway, codeNotListening}, {5702, http.StatusForbidden, string(wire.ReasonPortNotAllowed)}} {
		a := e.do(t, "POST", webHost(), openPath, bearer(tok), `{"port":`+strconv.Itoa(c.port)+`}`)
		if a.status != c.status || a.body != c.code {
			t.Errorf("open :%d: HTTP %d %q; want %d %s", c.port, a.status, a.body, c.status, c.code)
		}
	}
	if ps := e.g.Previews(1); len(ps) != 0 {
		t.Fatalf("a failed probe recorded previews: %+v", ps)
	}
	if n := fh.connects.Load(); n != 2 {
		t.Fatalf("%d CONNECTs for two refused opens; want one probe each", n)
	}
	if _, id, a := e.openURL(t, tok, `{"port":5703}`); id == "" {
		t.Fatalf("positive control: a listening offer: HTTP %d %s", a.status, a.body)
	}
	select {
	case got := <-fh.answers:
		if got.a.Port != 5703 {
			t.Fatalf("the host was told of a refused open: %+v", got.a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the host was not told of the open")
	}
}

// waitClosed waits until preview id is gone from the gateway and the host is
// told, unsolicited.
func waitClosed(t *testing.T, e *gwEnv, fh *fakeHost, id string) {
	t.Helper()
	waitFor(t, "the preview to close", func() bool {
		for _, p := range e.g.Previews(1) {
			if p.ID == id {
				return false
			}
		}
		return true
	})
	for {
		select {
		case got := <-fh.answers:
			if got.path == wire.ClosedPath && got.a.Preview == id && got.a.RequestID == "" {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the host was not told of the close")
		}
	}
}

func isOpen(e *gwEnv, id string) bool {
	for _, p := range e.g.Previews(1) {
		if p.ID == id {
			return true
		}
	}
	return false
}

// A preview with no request for the idle period (37m10s here, a literal
// between two lifecycle checks) closes at the next check and its host is
// told; a request resets the period; an open WebSocket is a request in
// progress, so its preview stays open past the period and closes one period
// after it ends.
func TestAnIdlePreviewCloses(t *testing.T) {
	const idle = 37*time.Minute + 10*time.Second
	t.Run("no requests", func(t *testing.T) {
		e := newGW(t, gwOpts{idle: idle, ttl: 20 * time.Hour})
		fh, id, cookie, _ := e.admitted(t, 4201, appServer(t))
		if a := e.do(t, "GET", pHost(id), "/", own(id, cookie), ""); a.status != 200 {
			t.Fatalf("HTTP %d", a.status)
		}
		e.clock.Advance(37 * time.Minute)
		if !isOpen(e, id) {
			t.Fatal("closed before the idle period passed")
		}
		e.clock.Advance(40 * time.Second)
		waitClosed(t, e, fh, id)
	})
	t.Run("a request resets it", func(t *testing.T) {
		e := newGW(t, gwOpts{idle: idle, ttl: 20 * time.Hour})
		fh, id, cookie, _ := e.admitted(t, 4202, appServer(t))
		e.clock.Advance(20 * time.Minute)
		if a := e.do(t, "GET", pHost(id), "/", own(id, cookie), ""); a.status != 200 {
			t.Fatalf("HTTP %d", a.status)
		}
		e.clock.Advance(37 * time.Minute) // 57m from the open, 37m from the request
		if !isOpen(e, id) {
			t.Fatal("closed though a request came 37m ago")
		}
		e.clock.Advance(40 * time.Second)
		waitClosed(t, e, fh, id)
	})
	t.Run("an open WebSocket keeps it open", func(t *testing.T) {
		e := newGW(t, gwOpts{idle: idle, ttl: 20 * time.Hour})
		app := wsEchoApp(t, nil)
		fh, id, cookie, _ := e.admitted(t, 4203, app.port)
		c, _, err := e.wsDial(t, id, "/", own(id, cookie), nil)
		if err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(3 * time.Hour)
		if !isOpen(e, id) {
			t.Fatal("a preview with an open WebSocket closed as idle")
		}
		wsEcho(t, c, "still here")
		c.Close(websocket.StatusNormalClosure, "")
		waitFor(t, "the WebSocket's end", func() bool {
			e.g.mu.Lock()
			defer e.g.mu.Unlock()
			return e.g.previews[id].active == 0
		})
		e.clock.Advance(37 * time.Minute)
		if !isOpen(e, id) {
			t.Fatal("closed less than the idle period after the WebSocket ended")
		}
		e.clock.Advance(40 * time.Second)
		waitClosed(t, e, fh, id)
	})
}

// A preview open longer than the TTL (5h11m10s here) closes even while in
// use: its open WebSocket is closed (1008, preview_closed), and its host is
// told.
func TestAPreviewPastItsTTLCloses(t *testing.T) {
	e := newGW(t, gwOpts{idle: 20 * time.Hour, ttl: 5*time.Hour + 11*time.Minute + 10*time.Second})
	app := wsEchoApp(t, nil)
	fh, id, cookie, _ := e.admitted(t, 4204, app.port)
	c, _, err := e.wsDial(t, id, "/", own(id, cookie), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	e.clock.Advance(5*time.Hour + 11*time.Minute)
	if !isOpen(e, id) {
		t.Fatal("closed before its TTL")
	}
	wsEcho(t, c, "before the TTL")
	e.clock.Advance(40 * time.Second)
	if code, reason := wsClosedWith(t, c); code != websocket.StatusPolicyViolation || reason != codePreviewClosed {
		t.Fatalf("past the TTL the WebSocket closed %d %q; want 1008 %s", code, reason, codePreviewClosed)
	}
	waitClosed(t, e, fh, id)
	if a := e.do(t, "GET", pHost(id), "/", own(id, cookie), ""); a.status != http.StatusNotFound {
		t.Fatalf("past the TTL: HTTP %d; want 404", a.status)
	}
}
