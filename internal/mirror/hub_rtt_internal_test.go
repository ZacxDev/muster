package mirror

import (
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

// TestTheFirstRoundTripIsSampledAfterTheHostsFirstMessage (hostConn.rtt): the
// link's first RTT sample waits until the read loop has handled the host's
// first message after the welcome — the burst a joining host sends — rather
// than being taken at registration, while that burst queues ahead of the
// pong. A host that has sent nothing yet has no sample; one that has, gets
// one at once.
func TestTheFirstRoundTripIsSampledAfterTheHostsFirstMessage(t *testing.T) {
	h := NewHub(log.New(io.Discard, "", 0))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = h.ServeConn(r.Context(), 1, "alpha", ws)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	write := func(m Msg) {
		b, _ := json.Marshal(m)
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	write(Msg{Type: TypeHello, Version: ProtocolVersion, Host: "alpha", Sessions: []string{"main"}})
	if _, _, err := ws.Read(ctx); err != nil { // the welcome
		t.Fatal(err)
	}
	// From here the host only answers pings (CloseRead reads, and pongs, in
	// the background).
	ws.CloseRead(ctx)
	eventuallyInternal(t, "the host online", func() bool { return h.Online(1, "alpha", "main") })
	rtt := func() int64 {
		h.mu.Lock()
		hc := h.hosts[hostKey{1, "alpha"}]
		h.mu.Unlock()
		if hc == nil {
			return -1
		}
		return hc.rtt.Load()
	}
	time.Sleep(300 * time.Millisecond)
	if got := rtt(); got != 0 {
		t.Fatalf("a round trip (%d ns) was sampled before the host sent anything after its welcome", got)
	}
	write(Msg{Type: TypeSessions, Sessions: []string{"main"}})
	eventuallyInternal(t, "the first round trip, once the host's first message is handled", func() bool { return rtt() > 0 })
}
