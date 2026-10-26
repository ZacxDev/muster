package mirrorhost

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestARefusedConnectLeavesNoConnectionOpen (wire.go): a connect muster
// refuses before the upgrade (a 401) is answered on a pooled HTTP/1.1
// connection, and the transport every connect shares closes it at once — it
// is never left idle for a later connect to reuse, nor open until the pool's
// idle timeout.
func TestARefusedConnectLeavesNoConnectionOpen(t *testing.T) {
	var mu sync.Mutex
	states := map[net.Conn]http.ConnState{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mirror: bad token", http.StatusUnauthorized)
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) { mu.Lock(); states[c] = s; mu.Unlock() }
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, Token: mirror.TokenPrefix + "x", Host: "laptop", Backend: &windowsBackend{}, AllowHTTP: true,
		Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	tr := c.tunnelTr
	for i := 0; i < 2; i++ {
		if err := c.RunOnce(context.Background()); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("connect %d: %v, want ErrUnauthorized", i, err)
		}
	}
	if c.tunnel.Transport != tr || tr.TLSNextProto == nil || tr.ForceAttemptHTTP2 {
		t.Fatal("the connects did not share one HTTP/1.1-only transport")
	}
	waitUntil(t, "every refused connect's connection closed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(states) == 0 {
			return false
		}
		for _, s := range states {
			if s != http.StateClosed {
				return false
			}
		}
		return true
	})
}
