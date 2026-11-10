//go:build linux

package previewhost

// The preview tools against a running Host and a fake gateway: Linux only,
// because preview_open makes the host's listener check, which reads /proc.

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// mcpRig is a Host connected to a fake gateway, and its preview tools.
type mcpRig struct {
	*hostRig
	gt     *gwTunnel
	s      *MCPServer
	probes *atomic.Int32
}

func startMCPRig(t *testing.T, body string, hc HostConfig) *mcpRig {
	t.Helper()
	gw := newFakeGateway(t, "b1")
	rig := startHost(t, body, hc, TunnelConfig{URLs: []string{gw.url}})
	gt := gw.next(5 * time.Second)
	gt.offerOf(wire.TypeConfig, 2*time.Second)
	var probes atomic.Int32
	return &mcpRig{hostRig: rig, gt: gt, probes: &probes, s: &MCPServer{Host: rig.h, Caller: testCaller, probe: countingProbe(&probes)}}
}

// openAsync runs preview_open in the background.
func (m *mcpRig) openAsync(t *testing.T, pane string, args map[string]any) <-chan toolOut {
	ch := make(chan toolOut, 1)
	go func() { ch <- callTool(t, m.s, pane, ToolOpen, args) }()
	return ch
}

// answerNextOpen reads the next open request on the offer stream and answers
// it with preview (or the gateway's refusal errCode).
func (m *mcpRig) answerNextOpen(t *testing.T, preview, errCode string) wire.Request {
	t.Helper()
	_, op := m.gt.offerOf(wire.TypeOpen, 3*time.Second)
	m.gt.answer(wire.OpenedPath, wire.Answer{RequestID: op.Request.ID, Preview: preview, Error: errCode})
	return *op.Request
}

func recv(t *testing.T, ch <-chan toolOut) toolOut {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(10 * time.Second):
		t.Fatal("the tool call never answered")
	}
	return toolOut{}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

// TestPreviewOpenAsksTheGatewayForTheCallersWindow: an open from pane %2 is
// written on the offer stream for window @2 with the port, label and path,
// and the gateway's id comes back; the open is recorded for @2. The same
// call from %1 — another window — is its own request, for @1.
func TestPreviewOpenAsksTheGatewayForTheCallersWindow(t *testing.T) {
	port := webServer(t)
	m := startMCPRig(t, portsJSON(port), HostConfig{})
	ch := m.openAsync(t, "%2", map[string]any{"port": port, "label": "app", "path": "/x"})
	req := m.answerNextOpen(t, "p-two", "")
	if req.Window != "@2" || req.Port != port || req.Label != "app" || req.Path != "/x" {
		t.Fatalf("the open request %+v; want window @2, port %d, label app, path /x", req, port)
	}
	o := recv(t, ch)
	if o.IsError || o.Body["id"] != "p-two" || o.Body["port"] != float64(port) || o.Body["label"] != "app" || o.Body["state"] != "open" {
		t.Fatalf("preview_open = %+v", o)
	}
	ch = m.openAsync(t, "%1", map[string]any{"port": port})
	if req := m.answerNextOpen(t, "p-one", ""); req.Window != "@1" {
		t.Fatalf("the second window's open went out for %q", req.Window)
	}
	if o := recv(t, ch); o.Body["id"] != "p-one" {
		t.Fatalf("the other window's open: %+v", o)
	}
	if n := m.probes.Load(); n != 2 {
		t.Fatalf("%d probes; want one per open asked of the gateway", n)
	}
}

// TestAnOpenPreviewKeepsItsIDAfterItsServerStops: once (window, port) is
// open, opening it again returns its id even though its web server has
// stopped (and its port is no longer listened on) — the design's Lifecycle
// keeps a preview whose upstream stops answering open, and preview_list
// still lists it, so the two tools agree. Nothing is probed or asked of the
// gateway for it. The control: the same dead port from ANOTHER window is a
// new open, and is refused not_listening.
func TestAnOpenPreviewKeepsItsIDAfterItsServerStops(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = srv.Serve(ln) }()
	m := startMCPRig(t, portsJSON(port), HostConfig{})
	ch := m.openAsync(t, "%1", map[string]any{"port": port, "label": "app"})
	m.answerNextOpen(t, "p-live", "")
	if o := recv(t, ch); o.Body["id"] != "p-live" {
		t.Fatalf("first open %+v", o)
	}
	must(t, srv.Close()) // the server stops; nothing listens on the port
	probes := m.probes.Load()
	o := callTool(t, m.s, "%1", ToolOpen, map[string]any{"port": port})
	if o.IsError || o.Body["id"] != "p-live" || o.Body["label"] != "app" || o.Body["state"] != "open" {
		t.Fatalf("again after the server stopped: %+v; want p-live", o)
	}
	l := callTool(t, m.s, "%1", ToolList, map[string]any{})
	if ps := l.Body["previews"].([]any); len(ps) != 1 || ps[0].(map[string]any)["id"] != "p-live" {
		t.Fatalf("preview_list disagrees: %v", l.Body)
	}
	if n := m.probes.Load(); n != probes {
		t.Fatalf("probed an open preview again")
	}
	m.gt.quiet(300 * time.Millisecond)
	if got := callTool(t, m.s, "%2", ToolOpen, map[string]any{"port": port}).code(t); got != CodeNotListening {
		t.Fatalf("control, another window: %q; want not_listening", got)
	}
}

// TestPreviewOpenIsIdempotentPerWindowAndPort: opening the same port from
// the same window again — from another pane of it too, and two at once —
// returns the same id and asks the gateway nothing more.
func TestPreviewOpenIsIdempotentPerWindowAndPort(t *testing.T) {
	port := webServer(t)
	m := startMCPRig(t, portsJSON(port), HostConfig{})
	ch := m.openAsync(t, "%1", map[string]any{"port": port})
	m.answerNextOpen(t, "p-1", "")
	if o := recv(t, ch); o.Body["id"] != "p-1" {
		t.Fatalf("first open %+v", o)
	}
	for _, pane := range []string{"%1", "%3"} { // %3 is another pane of @1
		if o := callTool(t, m.s, pane, ToolOpen, map[string]any{"port": port}); o.IsError || o.Body["id"] != "p-1" {
			t.Fatalf("again from %s: %+v; want p-1", pane, o)
		}
	}
	m.gt.quiet(300 * time.Millisecond)

	// Two at once, on a port not yet open: one request reaches the gateway.
	port2 := webServer(t)
	writePreviews(t, m.file, portsJSON(port, port2))
	m.gt.offerOf(wire.TypeConfig, 3*time.Second)
	a, b := m.openAsync(t, "%2", map[string]any{"port": port2}), m.openAsync(t, "%2", map[string]any{"port": port2})
	m.answerNextOpen(t, "p-2", "")
	if oa, ob := recv(t, a), recv(t, b); oa.Body["id"] != "p-2" || ob.Body["id"] != "p-2" {
		t.Fatalf("concurrent opens: %+v and %+v; want p-2 both", oa, ob)
	}
	m.gt.quiet(300 * time.Millisecond)
}

// TestPreviewOpenRefusesEachWayTheDesignNames: each error of the design's
// table comes back as its code, as a tool error. The host's own refusals —
// a port off the allowlist, an always-refused port that IS listed, another
// user's listener, nobody listening — are made BEFORE the gateway is asked
// (nothing on the offer stream) and before the probe; the gateway's
// refusal (max_open) is passed through.
func TestPreviewOpenRefusesEachWayTheDesignNames(t *testing.T) {
	ok := webServer(t)
	foreign := webServer(t)
	mcp := webServer(t)
	offList := webServer(t)
	absent := freePort(t)
	uid := os.Getuid()
	proc := newProcFixture(t, tcpHeader+fixtureRow(0, ok, uid, 1)+fixtureRow(1, foreign, uid+1, 2)+fixtureRow(2, mcp, uid, 3)+fixtureRow(3, offList, uid, 4), "")
	refused := func() map[int]string { return map[int]string{mcp: "the MCP endpoint"} }
	m := startMCPRig(t, portsJSON(ok, foreign, mcp, absent), HostConfig{Proc: proc.root, Refused: refused})
	for _, c := range []struct {
		name string
		port int
		want string
	}{
		{"off the allowlist", offList, CodePortNotAllowed},
		{"always refused, though listed", mcp, CodePortNotAllowed},
		{"another user's listener", foreign, CodeForeignListener},
		{"nobody listening", absent, CodeNotListening},
	} {
		if got := callTool(t, m.s, "%1", ToolOpen, map[string]any{"port": c.port}).code(t); got != c.want {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
	m.gt.quiet(300 * time.Millisecond)
	if n := m.probes.Load(); n != 0 {
		t.Fatalf("%d probes for ports the host refuses itself", n)
	}

	// The gateway's own refusal.
	ch := m.openAsync(t, "%1", map[string]any{"port": ok})
	m.answerNextOpen(t, "", "max_open")
	if got := recv(t, ch).code(t); got != "max_open" {
		t.Fatalf("the gateway's max_open came back as %q", got)
	}
	// A pane in no mirrored window.
	if got := callTool(t, m.s, "%99", ToolOpen, map[string]any{"port": ok}).code(t); got != CodeNoWindow {
		t.Fatalf("a pane in no window: %q", got)
	}
}

// silentServer accepts connections on 127.0.0.1 and never writes a byte: a
// server still starting, or one that is not HTTP and waits for its own
// protocol's first word.
func silentServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestTheProbeRefusesAPortWithNoWebServer: /proc says this user listens on
// the port (so the listener check passes), yet the probe finds no web
// server, and nothing is asked of the gateway. Two codes, by what the dial
// met: nothing accepting the connection is not_listening; a connection that
// gives no HTTP answer — bytes that are not HTTP, or silence for the whole
// 5 s probe (and not much longer) — is not_answering, which a retry can
// fix. The control, a web server answering 404, is asked.
func TestTheProbeRefusesAPortWithNoWebServer(t *testing.T) {
	dead := freePort(t)
	echo, _ := echoServer(t, "127.0.0.1:0") // a TCP server that is not HTTP
	silent := silentServer(t)
	web := webServer(t)
	uid := os.Getuid()
	proc := newProcFixture(t, tcpHeader+fixtureRow(0, dead, uid, 1)+fixtureRow(1, echo, uid, 2)+fixtureRow(2, web, uid, 3)+fixtureRow(3, silent, uid, 4), "")
	m := startMCPRig(t, portsJSON(dead, echo, web, silent), HostConfig{Proc: proc.root})
	for _, c := range []struct {
		name string
		port int
		want string
	}{{"no listener", dead, CodeNotListening}, {"not HTTP", echo, CodeNotAnswering}, {"silent", silent, CodeNotAnswering}} {
		start := time.Now()
		o := callTool(t, m.s, "%1", ToolOpen, map[string]any{"port": c.port})
		took := time.Since(start)
		if got := o.code(t); got != c.want {
			t.Errorf("%s (port %d): %q %v; want %s", c.name, c.port, got, o.Body["message"], c.want)
		}
		if c.name == "silent" && (took < ProbeWait-500*time.Millisecond || took > ProbeWait+3*time.Second) {
			t.Errorf("silent: answered after %v; want after the %v probe", took, ProbeWait)
		}
	}
	m.gt.quiet(300 * time.Millisecond)
	if n := m.probes.Load(); n != 3 {
		t.Fatalf("%d probes; want 3", n)
	}
	ch := m.openAsync(t, "%1", map[string]any{"port": web})
	m.answerNextOpen(t, "p-web", "")
	if o := recv(t, ch); o.Body["id"] != "p-web" {
		t.Fatalf("control: %+v", o)
	}
}

// TestTheGatewaysAbsenceAndSilenceAreTheirOwnCodes: with no tunnel the open
// is gateway_not_connected at once; with a gateway that never answers it is
// gateway_timeout after 5 s (the default AnswerWait), not sooner.
func TestTheGatewaysAbsenceAndSilenceAreTheirOwnCodes(t *testing.T) {
	port := webServer(t)
	cfg := Config{Ports: mustPorts(t, strconv.Itoa(port)), MaxOpen: 8}
	h := NewHost(HostConfig{Config: cfg, UID: os.Getuid()})
	s := &MCPServer{Host: h, Caller: testCaller}
	if got := callTool(t, s, "%1", ToolOpen, map[string]any{"port": port}).code(t); got != "gateway_not_connected" {
		t.Fatalf("no tunnel: %q", got)
	}

	m := startMCPRig(t, portsJSON(port), HostConfig{}) // AnswerWait: the default
	start := time.Now()
	o := callTool(t, m.s, "%1", ToolOpen, map[string]any{"port": port})
	took := time.Since(start)
	if got := o.code(t); got != "gateway_timeout" || took < 4500*time.Millisecond || took > 8*time.Second {
		t.Fatalf("silent gateway: %q after %v; want gateway_timeout after 5s", got, took)
	}
}

// TestPreviewListIsTheCallersWindowIncludingClickOpens: the list holds the
// window's agent opens AND the owner's click-opens (the gateway's unsolicited
// "opened"), and nothing of another window's.
func TestPreviewListIsTheCallersWindowIncludingClickOpens(t *testing.T) {
	port := webServer(t)
	other := webServer(t)
	m := startMCPRig(t, portsJSON(port, other), HostConfig{})
	ch := m.openAsync(t, "%1", map[string]any{"port": port, "label": "app"})
	m.answerNextOpen(t, "p-agent", "")
	recv(t, ch)
	m.gt.answer(wire.OpenedPath, wire.Answer{Preview: "p-click", Window: "@1", Port: other})
	m.gt.answer(wire.OpenedPath, wire.Answer{Preview: "p-elsewhere", Window: "@2", Port: other})

	ids := func(pane string) []string {
		o := callTool(t, m.s, pane, ToolList, map[string]any{})
		if o.IsError {
			t.Fatalf("preview_list from %s: %v", pane, o.Body)
		}
		var out []string
		for _, p := range o.Body["previews"].([]any) {
			out = append(out, p.(map[string]any)["id"].(string))
		}
		return out
	}
	if got := ids("%1"); strings.Join(got, ",") != "p-agent,p-click" {
		t.Fatalf("@1 lists %v; want p-agent,p-click", got)
	}
	if got := ids("%2"); strings.Join(got, ",") != "p-elsewhere" {
		t.Fatalf("@2 lists %v; want p-elsewhere", got)
	}
}

// TestPreviewCloseClosesTheCallersOwnOnly: another window's id is
// not_found, exactly like an unknown one, and nothing is asked of the
// gateway; the window's own id is closed through the gateway.
func TestPreviewCloseClosesTheCallersOwnOnly(t *testing.T) {
	port := webServer(t)
	m := startMCPRig(t, portsJSON(port), HostConfig{})
	m.gt.answer(wire.OpenedPath, wire.Answer{Preview: "p-mine", Window: "@1", Port: port})
	for _, c := range []struct{ pane, id string }{{"%2", "p-mine"}, {"%1", "p-nothing"}} {
		if got := callTool(t, m.s, c.pane, ToolClose, map[string]any{"id": c.id}).code(t); got != CodeNotFound {
			t.Errorf("close %s from %s: %q; want not_found", c.id, c.pane, got)
		}
	}
	m.gt.quiet(300 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(1)
	var o toolOut
	go func() { defer wg.Done(); o = callTool(t, m.s, "%1", ToolClose, map[string]any{"id": "p-mine"}) }()
	_, cl := m.gt.offerOf(wire.TypeClose, 3*time.Second)
	if cl.Request.Preview != "p-mine" || cl.Request.Window != "@1" || cl.Request.Port != port {
		t.Fatalf("the close request %+v", cl.Request)
	}
	m.gt.answer(wire.ClosedPath, wire.Answer{RequestID: cl.Request.ID, Preview: "p-mine"})
	wg.Wait()
	if o.IsError || o.Body["closed"] != true {
		t.Fatalf("preview_close = %+v", o)
	}
	if got := m.h.Opens(); len(got) != 0 {
		t.Fatalf("still open: %+v", got)
	}
}

// TestEveryToolSaysPreviewsOffWhileTheFileIsBad: an invalid previews.json
// turns previews off, and each tool answers previews_off.
func TestEveryToolSaysPreviewsOffWhileTheFileIsBad(t *testing.T) {
	port := webServer(t)
	m := startMCPRig(t, portsJSON(port), HostConfig{})
	must(t, os.WriteFile(m.file, []byte(`{"ports": []}`), 0o600))
	_, c := m.gt.offerOf(wire.TypeConfig, 3*time.Second)
	if c.Config.Disabled == "" {
		t.Fatalf("not disabled: %+v", c.Config)
	}
	for _, c := range []struct {
		tool string
		args map[string]any
	}{{ToolOpen, map[string]any{"port": port}}, {ToolList, map[string]any{}}, {ToolClose, map[string]any{"id": "p-1"}}} {
		if got := callTool(t, m.s, "%1", c.tool, c.args).code(t); got != CodePreviewsOff {
			t.Errorf("%s: %q; want previews_off", c.tool, got)
		}
	}
}
