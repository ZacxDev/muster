package mirrorhost

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestEachAcceptedConnectionRunsOnConnected: the hook muster-mirror wires to
// the MCP endpoint's KickTools (a reconnect is when an upgraded muster's tool
// set arrives) runs once per connection muster ACCEPTS — after its welcome,
// never for a connection it has not yet accepted — and again on the next one.
func TestEachAcceptedConnectionRunsOnConnected(t *testing.T) {
	f := newFakeMuster(t)
	b := &windowsBackend{}
	b.set(WindowInfo{Session: "work", ID: "@1", Index: 0, Active: true, Pane: "%1", PanePID: 1, Command: "zsh"})
	c, err := New(Config{URL: f.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b,
		AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	c.OnConnected(func() { n.Add(1) })
	for want := int32(1); want <= 2; want++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { _ = c.RunOnce(ctx); close(done) }()
		waitUntil(t, "the hook", func() bool { return n.Load() == want })
		time.Sleep(100 * time.Millisecond)
		if got := n.Load(); got != want {
			t.Fatalf("connection %d ran the hook %d time(s) in all", want, got)
		}
		cancel()
		<-done
	}
}
