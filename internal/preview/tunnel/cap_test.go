package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// controlHandler serves the control streams the way a host does: GET
// wire.OfferPath writes one line and stays open until the test ends, POST
// wire.OpenedPath / wire.ClosedPath answer 204.
func controlHandler(t *testing.T) http.Handler {
	t.Helper()
	end := make(chan struct{})
	t.Cleanup(func() { close(end) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == wire.OfferPath:
			_ = wire.NewEncoder(w).Encode(wire.Offer{Type: wire.TypeWindows, Windows: []string{"@1"}})
			w.(http.Flusher).Flush()
			select {
			case <-end:
			case <-r.Context().Done():
			}
		case r.Method == http.MethodPost && (r.URL.Path == wire.OpenedPath || r.URL.Path == wire.ClosedPath):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

// openOffer opens the long-lived offer stream and reads its first line, so
// the stream is certainly holding an HTTP/2 stream slot.
func openOffer(t *testing.T, p *pair) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, wire.OfferPath, nil)
	resp, err := p.client.RoundTrip(req)
	if err != nil {
		t.Fatalf("offer stream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if _, err := wire.NewDecoder(resp.Body).Next(); err != nil {
		t.Fatalf("offer stream first line: %v", err)
	}
}

func isMaxConns(err error) bool {
	var re *RefusedError
	return errors.As(err, &re) && re.Reason == wire.ReasonMaxConns && re.Status == http.StatusTooManyRequests
}

// dialWithin dials with its own ctx of d; a dial still waiting after d is an
// error, so a wait that should have been bounded by DialWait shows as one.
func dialWithin(p *pair, port int, d time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return p.client.Dial(ctx, port)
}

// TestDialAtTheCapWaitsThenRefusesMaxConns: with every upstream slot held, a
// Dial waits DialWait and then refuses with max_conns (429) — not the
// caller's ctx expiring, and not at once. The positive control frees a slot
// inside the wait, and that Dial gets the slot.
func TestDialAtTheCapWaitsThenRefusesMaxConns(t *testing.T) {
	const wait = 300 * time.Millisecond
	for _, c := range []struct {
		name       string
		hostConns  int
		clientMax  int
		freeAfter  time.Duration // 0: never free a slot
		wantMaxCon bool
	}{
		{"gateway cap, nothing frees", 2, 2, 0, true},
		{"positive control: gateway cap, a slot frees inside the wait", 2, 2, 100 * time.Millisecond, false},
		// The gateway believes in more slots than the host has: the host's
		// backstop refuses, and Dial retries it within the same wait.
		{"host backstop, nothing frees", 2, 4, 0, true},
		{"positive control: host backstop, a slot frees inside the wait", 2, 4, 100 * time.Millisecond, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			echo := startTCP(t, echoUpstream)
			w := wait
			if c.freeAfter > 0 {
				w = 2 * time.Second
			}
			p := newPair(t, pairOpts{
				host:   HostOptions{MaxConns: c.hostConns},
				client: ClientOptions{MaxConns: c.clientMax, DialWait: w},
				allow:  []int{echo},
			})
			var held []net.Conn
			for range c.hostConns {
				conn, err := dialWithin(p, echo, 2*time.Second)
				if err != nil {
					t.Fatalf("filling the cap: %v", err)
				}
				held = append(held, conn)
			}
			defer func() {
				for _, h := range held {
					h.Close()
				}
			}()
			if c.freeAfter > 0 {
				time.AfterFunc(c.freeAfter, func() { held[0].Close() })
			}
			t0 := time.Now()
			conn, err := dialWithin(p, echo, 5*time.Second)
			took := time.Since(t0)
			t.Logf("RESULT took %v err %v", took.Round(time.Millisecond), err)
			if c.wantMaxCon {
				if err == nil {
					conn.Close()
					t.Fatal("a Dial past the cap succeeded")
				}
				if !isMaxConns(err) {
					t.Fatalf("Dial past the cap = %v, want a 429 max_conns refusal", err)
				}
				if took < wait-20*time.Millisecond || took > wait+2*time.Second {
					t.Fatalf("refused after %v, want after about the %v wait", took, wait)
				}
				return
			}
			if err != nil {
				t.Fatalf("a slot freed after %v, but Dial = %v", c.freeAfter, err)
			}
			conn.Close()
			if took < c.freeAfter-20*time.Millisecond {
				t.Fatalf("Dial took %v, before the slot freed at %v", took, c.freeAfter)
			}
		})
	}
}

// TestMaxConnsCountsOnlyUpstreamStreams: maxConns is upstream connections.
// With the offer stream open, exactly maxConns CONNECTs open at once, and the
// next is max_conns; with maxConns 1 the offer stream does not take the one.
func TestMaxConnsCountsOnlyUpstreamStreams(t *testing.T) {
	for _, n := range []int{1, 3} {
		t.Run(strings.Repeat("I", n), func(t *testing.T) {
			echo := startTCP(t, echoUpstream)
			p := newPair(t, pairOpts{
				host:   HostOptions{MaxConns: n},
				client: ClientOptions{MaxConns: n, DialWait: 300 * time.Millisecond},
				allow:  []int{echo},
				extra:  controlHandler(t),
			})
			openOffer(t, p)
			for i := range n {
				conn, err := dialWithin(p, echo, 2*time.Second)
				if err != nil {
					t.Fatalf("maxConns %d, offer stream open: CONNECT %d of %d = %v", n, i+1, n, err)
				}
				defer conn.Close()
			}
			if conn, err := dialWithin(p, echo, 3*time.Second); err == nil {
				conn.Close()
				t.Fatalf("CONNECT %d opened past maxConns %d", n+1, n)
			} else if !isMaxConns(err) {
				t.Fatalf("CONNECT %d = %v, want max_conns", n+1, err)
			}
		})
	}
}

// TestControlRequestsDoNotQueueBehindAFullCap: every upstream slot is held by
// a paused download and the offer stream is open; an open and a close answer
// (POST wire.OpenedPath / wire.ClosedPath) still complete at once, because
// the control streams have slots of their own.
func TestControlRequestsDoNotQueueBehindAFullCap(t *testing.T) {
	const n = 4
	blob := startBlobServer(t)
	p := newPair(t, pairOpts{
		host:   HostOptions{MaxConns: n},
		client: ClientOptions{MaxConns: n, DialWait: 300 * time.Millisecond},
		allow:  []int{blob},
		extra:  controlHandler(t),
	})
	openOffer(t, p)
	for i := range n {
		if _, err := tryPaused(t, p, blob, 64<<20, uint64(300+i), 1<<20, 2*time.Second); err != nil {
			t.Fatalf("filling the cap with paused downloads: %d of %d: %v", i+1, n, err)
		}
	}
	for _, path := range []string{wire.OpenedPath, wire.ClosedPath, wire.OpenedPath} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(`{"request_id":"r1"}`))
		resp, err := p.client.RoundTrip(req)
		if err != nil {
			cancel()
			t.Fatalf("POST %s with the cap full of paused streams: %v", path, err)
		}
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST %s = %d", path, resp.StatusCode)
		}
	}
}
