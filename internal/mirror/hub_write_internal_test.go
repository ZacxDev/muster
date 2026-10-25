package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
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
// Held deterministically, as above: the host does not read, the browser
// "types" far more than the loopback socket buffers hold, so one stdin frame
// is stuck in progress; then the attach's context ends. AttachWindow returns
// and cancels the keystroke write's context while it is stuck.
func TestATerminalClosedMidKeystrokeDoesNotHangUpTheHost(t *testing.T) {
	h := NewHub(log.New(io.Discard, "", 0))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = h.ServeConn(r.Context(), 1, "alpha", ws)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.CloseNow() })
	host.SetReadLimit(1 << 20)
	hello, _ := json.Marshal(Msg{Type: TypeHello, Version: ProtocolVersion, Host: "alpha", Sessions: []string{"main"}, Interactive: true})
	if err := host.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	if _, data, err := host.Read(ctx); err != nil || !bytes.Contains(data, []byte(TypeWelcome)) {
		t.Fatalf("welcome = %q, %v", data, err)
	}
	eventuallyInternal(t, "the host online", func() bool { return h.Online(1, "alpha", "main") })

	// The browser types 64MB into an interactive terminal; the host reads none.
	const typed = 64 << 20
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	typing := make(chan struct{})
	go func() {
		defer close(typing)
		_, _ = pw.Write(bytes.Repeat([]byte("k"), typed))
	}()
	actx, leave := context.WithCancel(ctx)
	attached := make(chan error, 1)
	go func() { attached <- h.AttachWindow(actx, 1, "alpha", "main", "", false, pr, io.Discard, nil) }()
	select {
	case <-typing:
		t.Fatal("instrument: 64MB of keystrokes went through to a host that is not reading; no write was ever stuck")
	case err := <-attached:
		t.Fatalf("instrument: the attach ended before the browser left: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	leave() // the browser closes the terminal mid-keystroke
	time.Sleep(100 * time.Millisecond)

	// The host reads what it was sent: its open, keystrokes, and the terminal's
	// close — on a connection that is still there.
	var got int
	for closed := false; !closed; {
		typ, data, err := host.Read(ctx)
		if err != nil {
			t.Fatalf("the host lost its connection when a terminal closed mid-keystroke (after %d typed bytes): %v", got, err)
		}
		if typ == websocket.MessageBinary {
			_, p, derr := DecodeData(data)
			if derr != nil {
				t.Fatal(derr)
			}
			got += len(p)
			continue
		}
		var m Msg
		_ = json.Unmarshal(data, &m)
		closed = m.Type == TypeClose
	}
	if got == 0 {
		t.Fatal("instrument: the host read no keystroke at all")
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
		_, data, err := host.Read(ctx)
		if err != nil {
			t.Fatalf("the next attach never reached the host: %v", err)
		}
		var m Msg
		if json.Unmarshal(data, &m) == nil && m.Type == TypeOpen {
			break
		}
	}
}
