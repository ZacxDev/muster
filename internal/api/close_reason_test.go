package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/telemetry"
)

// THE BROWSER LEAVING IS NEVER AN ERROR. A page that closes its socket while
// the server is writing makes the write fail ("use of closed network
// connection") BEFORE the socket's reader cancels the stream's context, so
// the end must be read off the failed write itself, not off the context.

// pageSocket is a real server-side socket whose page has CLOSED it, and whose
// stream context is still live — exactly the window the race lives in: no
// CloseRead runs on this side, so nothing cancels anything.
func pageSocket(t *testing.T) *websocket.Conn {
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
	server := <-got
	_ = page.CloseNow() // the page leaves
	return server
}

// TestAWriteToAClosedPageIsTheBrowserLeaving: the error a write to a page
// that has gone returns is classified client_left with the stream's context
// still live — and shutdown when muster is shutting down. The control: the
// SAME write's raw error, unmarked, is what was classified "error".
func TestAWriteToAClosedPageIsTheBrowserLeaving(t *testing.T) {
	c := pageSocket(t)
	live := context.Background()
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for err == nil {
		// The first writes may land in the socket's buffers; the page's close
		// surfaces within a few.
		if err = writeToPage(live, c, map[string]string{"type": "batch"}); err == nil {
			if time.Now().After(deadline) {
				t.Fatal("a write to a closed page never failed")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !errors.Is(err, errPageGone) {
		t.Fatalf("writeToPage's failure is not marked as the page gone: %v", err)
	}
	if got := streamEndReason(live, c, err); got != telemetry.ReasonClientLeft {
		t.Errorf("a write to a closed page, context live, ended as %q, want %q", got, telemetry.ReasonClientLeft)
	}
	// CONTROL: unmarked, the very same failure is what read as a fault.
	raw := terminalWriteJSON(live, c, map[string]string{"type": "batch"})
	if raw == nil {
		t.Fatal("CONTROL: a second write to the closed page succeeded")
	}
	if got := streamEndReason(live, c, raw); got == telemetry.ReasonClientLeft {
		t.Errorf("CONTROL: the raw write error already classifies as %q; this test would pass without the mark", got)
	}
	// A deploy that cut the stream is still a deploy.
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(telemetry.ErrShuttingDown)
	if got := streamEndReason(ctx, c, err); got != telemetry.ReasonShutdown {
		t.Errorf("a failed write during a shutdown ended as %q, want %q", got, telemetry.ReasonShutdown)
	}
	// And the terminal's writer marks its failures the same way.
	if _, werr := (pageWriter{failingWriter{}}).Write([]byte("x")); !errors.Is(werr, errPageGone) {
		t.Errorf("pageWriter's failure is not marked as the page gone: %v", werr)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("use of closed network connection")
}
