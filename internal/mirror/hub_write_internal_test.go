package mirror

import (
	"bytes"
	"context"
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
