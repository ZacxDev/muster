package mirror

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// TestACallerLeavingMidWriteDoesNotHangUpTheHost: every request on a host
// shares its one socket, so the context of ONE request ending while its write
// is in progress must end that request only — not close the socket under every
// other terminal, watch and paste on the host (writeFrame).
//
// The write is held in progress deterministically: the "host" does not read,
// so a message larger than the loopback socket buffers cannot finish. The
// caller's context ends while it is stuck; then the host reads. The whole
// message must arrive and the connection must still carry the next one.
// Before writeFrame detached the caller's cancel, coder/websocket closed the
// connection at the cancel and the host read a truncated stream (the
// TestAPerSessionHostIsListedAndAddressedExactlyAsBefore flake, whose window
// was the instant after the flush instead of a blocked write).
func TestACallerLeavingMidWriteDoesNotHangUpTheHost(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- ws
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	host, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.CloseNow() })
	const size = 64 << 20 // far beyond any loopback socket buffer
	host.SetReadLimit(size + 1)
	hub := <-serverConn

	payload := bytes.Repeat([]byte("x"), size)
	caller, leave := context.WithCancel(context.Background())
	wrote := make(chan error, 1)
	go func() { wrote <- writeFrame(caller, hub, websocket.MessageBinary, payload) }()

	// The write is stuck on a host that is not reading.
	select {
	case err := <-wrote:
		t.Fatalf("instrument: a %d-byte write to a host that is not reading finished (%v); it must be stuck to test anything", size, err)
	case <-time.After(200 * time.Millisecond):
	}
	leave()
	time.Sleep(100 * time.Millisecond) // the library's cancel hook has run by now, when there is one

	_, got, err := host.Read(ctx)
	if err != nil {
		t.Fatalf("the host lost its connection when one caller left mid-write: %v", err)
	}
	if len(got) != size {
		t.Fatalf("the host read %d bytes, want %d", len(got), size)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the write = %v, want it finished", err)
	}
	// And the socket still carries the next request.
	if err := writeJSON(context.Background(), hub, Msg{Type: TypePaste, ID: 7}); err != nil {
		t.Fatalf("the next write on the socket: %v", err)
	}
	if _, next, err := host.Read(ctx); err != nil || !bytes.Contains(next, []byte(`"paste"`)) {
		t.Fatalf("the next message = %q, %v", next, err)
	}

	// A caller that has already left sends nothing at all.
	if err := writeJSON(caller, hub, Msg{Type: TypePaste, ID: 8}); err == nil {
		t.Fatal("a caller whose context had ended still wrote")
	}
}

// TestATerminalClosedMidKeystrokeDoesNotHangUpTheHost is the terminal's own
// path to writeFrame: AttachWindow types stdin on the attach's context, so the
// browser leaving while a keystroke frame is in progress cancels that write.
// It must end the terminal only — the host's connection stays, and the next
// caller (another attach) still reaches it.
//
// 🔴 "IN PROGRESS" IS A SIGNAL HERE, NEVER A WAIT. The hub's side of the
// socket is a gatedConn: armed once the host has read the terminal's open, the
// next write the hub makes — the keystroke frame, there being no other writer
// — reports that it has entered the socket and then blocks there. Only after
// that report does the browser leave, so the keystroke write is in progress
// when its context is cancelled on every run, whatever the load. (The first
// version waited 300ms for the write to get stuck behind a full socket; on a
// loaded two-core runner no keystroke had been written by then, and its own
// instrument check failed. Its sleep is gone with it.)
func TestATerminalClosedMidKeystrokeDoesNotHangUpTheHost(t *testing.T) {
	h := NewHub(log.New(io.Discard, "", 0))
	gate := newWriteGate()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = h.ServeConn(r.Context(), 1, "alpha", ws)
	}))
	srv.Listener = gatedListener{srv.Listener, gate}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(gate.open)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.CloseNow() })
	hello, _ := json.Marshal(Msg{Type: TypeHello, Version: ProtocolVersion, Host: "alpha", Sessions: []string{"main"}, Interactive: true})
	if err := host.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	if _, data, err := host.Read(ctx); err != nil || !bytes.Contains(data, []byte(TypeWelcome)) {
		t.Fatalf("welcome = %q, %v", data, err)
	}
	eventuallyInternal(t, "the host online", func() bool { return h.Online(1, "alpha", "main") })

	// An interactive terminal; the host reads its open.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	actx, leave := context.WithCancel(ctx)
	attached := make(chan error, 1)
	go func() { attached <- h.AttachWindow(actx, 1, "alpha", "main", "", false, pr, io.Discard, nil) }()
	if m := readText(ctx, t, host); m.Type != TypeOpen {
		t.Fatalf("instrument: the host's first message after the welcome is %q, want the terminal's open", m.Type)
	}

	// The browser types a key; its frame enters the hub's socket and is held
	// there. Then the browser leaves, then the socket lets the frame through.
	gate.arm()
	go func() { _, _ = pw.Write([]byte("k")) }()
	select {
	case <-gate.entered:
	case err := <-attached:
		t.Fatalf("instrument: the attach ended before its keystroke reached the socket: %v", err)
	case <-ctx.Done():
		t.Fatal("instrument: the keystroke never reached the hub's socket")
	}
	leave() // the browser closes the terminal mid-keystroke
	gate.open()

	// The host reads the keystroke and the terminal's close, on a connection
	// that is still there.
	var got []byte
	for closed := false; !closed; {
		typ, data, err := host.Read(ctx)
		if err != nil {
			t.Fatalf("the host lost its connection when a terminal closed mid-keystroke (after %q): %v", got, err)
		}
		if typ == websocket.MessageBinary {
			_, p, derr := DecodeData(data)
			if derr != nil {
				t.Fatal(derr)
			}
			got = append(got, p...)
			continue
		}
		var m Msg
		_ = json.Unmarshal(data, &m)
		closed = m.Type == TypeClose
	}
	if string(got) != "k" {
		t.Fatalf("instrument: the host read keystrokes %q, want the one held mid-write (\"k\")", got)
	}
	if err := <-attached; err != nil {
		t.Fatalf("the attach = %v, want nil (the browser left)", err)
	}

	// The next caller still reaches the host.
	if !h.Online(1, "alpha", "main") {
		t.Fatal("the host is offline after one of its terminals closed")
	}
	nctx, ncancel := context.WithCancel(ctx)
	defer ncancel()
	go func() { _ = h.AttachWindow(nctx, 1, "alpha", "main", "", true, strings.NewReader(""), io.Discard, nil) }()
	for {
		m, err := readMsg(ctx, host)
		if err != nil {
			t.Fatalf("the next attach never reached the host: %v", err)
		}
		if m.Type == TypeOpen {
			break
		}
	}
	if gate.wasClosed() {
		t.Fatal("the hub closed the host's socket when a terminal closed mid-keystroke")
	}
}

// readMsg reads the host's next text message (binary frames are skipped).
func readMsg(ctx context.Context, ws *websocket.Conn) (Msg, error) {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return Msg{}, err
		}
		if typ != websocket.MessageText {
			continue
		}
		var m Msg
		_ = json.Unmarshal(data, &m)
		return m, nil
	}
}

func readText(ctx context.Context, t *testing.T, ws *websocket.Conn) Msg {
	t.Helper()
	m, err := readMsg(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// writeGate holds the hub's next socket write once armed: entered is closed
// when that write reaches the socket, which then blocks until open — or until
// the socket is closed, as a real one stuck mid-write would be.
type writeGate struct {
	mu       sync.Mutex
	armed    bool
	entered  chan struct{}
	release  chan struct{}
	closed   chan struct{}
	opened   sync.Once
	shutDown sync.Once
}

func newWriteGate() *writeGate {
	return &writeGate{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
}

func (g *writeGate) arm()  { g.mu.Lock(); g.armed = true; g.mu.Unlock() }
func (g *writeGate) open() { g.opened.Do(func() { close(g.release) }) }
func (g *writeGate) wasClosed() bool {
	select {
	case <-g.closed:
		return true
	default:
		return false
	}
}

type gatedListener struct {
	net.Listener
	g *writeGate
}

func (l gatedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &gatedConn{Conn: c, g: l.g}, nil
}

type gatedConn struct {
	net.Conn
	g *writeGate
}

func (c *gatedConn) Write(p []byte) (int, error) {
	c.g.mu.Lock()
	held := c.g.armed
	c.g.armed = false
	c.g.mu.Unlock()
	if held {
		close(c.g.entered)
		select {
		case <-c.g.release:
		case <-c.g.closed:
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

func (c *gatedConn) Close() error {
	c.g.shutDown.Do(func() { close(c.g.closed) })
	return c.Conn.Close()
}
