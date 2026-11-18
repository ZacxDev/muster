package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// controlHandler serves the control streams the way a host does: GET
// wire.OfferPath writes one line and stays open until the test ends, POST
// wire.OpenedPath / wire.ClosedPath answer 204.
func controlHandler(t *testing.T) http.Handler { return slowControlHandler(t, 0) }

// slowControlHandler is controlHandler whose POST answers take slow.
func slowControlHandler(t *testing.T, slow time.Duration) http.Handler {
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
			time.Sleep(slow)
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

// TestControlRequestsPastTheReserveQueue: with the offer stream open and every
// upstream slot held, a burst of more control requests than ControlStreams
// (six slow answers at once) all succeed — the ones past the reserve wait for
// a stream rather than fail. This is what StrictMaxConcurrentStreams is for
// now that Dial keeps data streams under the host's limit.
func TestControlRequestsPastTheReserveQueue(t *testing.T) {
	const n, burst = 2, ControlStreams + 2
	echo := startTCP(t, echoUpstream)
	p := newPair(t, pairOpts{
		host:   HostOptions{MaxConns: n},
		client: ClientOptions{MaxConns: n, DialWait: 300 * time.Millisecond},
		allow:  []int{echo},
		extra:  slowControlHandler(t, 200*time.Millisecond),
	})
	openOffer(t, p)
	for range n {
		conn, err := dialWithin(p, echo, 2*time.Second)
		if err != nil {
			t.Fatalf("filling the cap: %v", err)
		}
		defer conn.Close()
	}
	errs := make(chan error, burst)
	var wg sync.WaitGroup
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, wire.OpenedPath, strings.NewReader(`{}`))
			resp, err := p.client.RoundTrip(req)
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				errs <- errors.New(resp.Status)
			}
		}()
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		failed++
		t.Logf("control request failed: %v", err)
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent control requests failed past the reserve of %d", failed, burst, ControlStreams)
	}
}

// TestRefusedDialsFreeTheirSlot: refusals other than max_conns (a port off the
// allowlist, nothing listening) return their slot at once, so repeating them
// never eats the cap.
func TestRefusedDialsFreeTheirSlot(t *testing.T) {
	echo := startTCP(t, echoUpstream)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := portOf(t, ln.Addr())
	ln.Close()
	p := newPair(t, pairOpts{
		client: ClientOptions{MaxConns: 2, DialWait: 300 * time.Millisecond},
		allow:  []int{echo, dead},
	})
	for i := range 5 {
		for _, port := range []int{echo + 1, dead} {
			if _, err := dialWithin(p, port, 2*time.Second); err == nil || isMaxConns(err) {
				t.Fatalf("round %d, :%d = %v, want a non-max_conns refusal", i, port, err)
			}
		}
	}
	if u := p.client.slots.inUse(); u != 0 {
		t.Fatalf("%d slots in use after 10 refused dials, want 0", u)
	}
	for range 2 { // and the whole cap is still there
		conn, err := dialWithin(p, echo, 2*time.Second)
		if err != nil {
			t.Fatalf("after the refusals: %v", err)
		}
		defer conn.Close()
	}
}

// TestBackstopRetriesFreeTheirSlot: a Dial that the host's backstop keeps
// refusing frees its gateway slot whether it ends on the wait's deadline or
// on its caller's ctx. The gateway believes in 3 slots, the host has 1 and
// it is held, so every Dial lands in the retry loop.
func TestBackstopRetriesFreeTheirSlot(t *testing.T) {
	echo := startTCP(t, echoUpstream)
	p := newPair(t, pairOpts{
		host:   HostOptions{MaxConns: 1},
		client: ClientOptions{MaxConns: 3, DialWait: 300 * time.Millisecond},
		allow:  []int{echo},
	})
	held, err := dialWithin(p, echo, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for i := range 3 {
		if _, err := dialWithin(p, echo, 5*time.Second); !isMaxConns(err) {
			t.Fatalf("deadline case %d: %v, want max_conns", i, err)
		}
		if u := p.client.slots.inUse(); u != 1 {
			t.Fatalf("deadline case %d: %d slots in use, want 1 (the held stream)", i, u)
		}
	}
	for i := range 5 {
		// The caller's ctx (110 ms) ends inside the 300 ms wait, between
		// 25 ms retries.
		if _, err := dialWithin(p, echo, 110*time.Millisecond); err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ctx case %d: %v, want the ctx's deadline", i, err)
		}
		if u := p.client.slots.inUse(); u != 1 {
			t.Fatalf("ctx case %d: %d slots in use, want 1 (the held stream)", i, u)
		}
	}
}

// TestSetMaxConnsWakesAWaitingDial: a Dial waiting at the gateway's limit
// takes a slot as soon as SetMaxConns raises the limit, not at the end of its
// wait.
func TestSetMaxConnsWakesAWaitingDial(t *testing.T) {
	echo := startTCP(t, echoUpstream)
	p := newPair(t, pairOpts{
		host:   HostOptions{MaxConns: 3},
		client: ClientOptions{MaxConns: 1, DialWait: 3 * time.Second},
		allow:  []int{echo},
	})
	held, err := dialWithin(p, echo, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	time.AfterFunc(100*time.Millisecond, func() { p.client.SetMaxConns(2) })
	t0 := time.Now()
	conn, err := dialWithin(p, echo, 5*time.Second)
	took := time.Since(t0)
	if err != nil {
		t.Fatalf("after SetMaxConns(2): %v (took %v)", err, took)
	}
	conn.Close()
	if took > time.Second {
		t.Fatalf("the waiting Dial took %v; the raised limit did not wake it", took)
	}
}

// TestSetMaxConnsClamps: the limit stays within 1..wire.MaxConns, and below 1
// is 1 (never the default, which could exceed the host's).
func TestSetMaxConnsClamps(t *testing.T) {
	var s slots
	s.init(0)
	if s.limit != 64 {
		t.Errorf("init(0) = %d, want the default 64", s.limit)
	}
	// Each case on a fresh limiter: the first value is also the ceiling
	// (TestSetMaxConnsNeverRisesAboveTheFirst).
	for _, c := range []struct{ in, want int }{{-5, 1}, {0, 1}, {1, 1}, {7, 7}, {64, 64}, {65, 64}, {1000, 64}} {
		var s slots
		s.init(0)
		s.setLimit(c.in)
		if s.limit != c.want {
			t.Errorf("setLimit(%d) = %d, want %d", c.in, s.limit, c.want)
		}
	}
}

// TestSetMaxConnsNeverRisesAboveTheFirst: the host's Serve fixes its maxConns
// for the tunnel's life, so the first SetMaxConns is a ceiling the client
// keeps itself — a later, larger value is held at it; a smaller one lowers the
// limit, and a later one may raise it back, up to the ceiling only. The
// provisional ClientOptions.MaxConns (5 here, distinct from every value
// below) is not the ceiling. Checked through Dial too: a third stream at a
// ceiling of 2 is refused max_conns, though the host admits 9.
func TestSetMaxConnsNeverRisesAboveTheFirst(t *testing.T) {
	var s slots
	s.init(5)
	for _, c := range []struct{ in, want int }{{3, 3}, {11, 3}, {2, 2}, {3, 3}, {64, 3}} {
		s.setLimit(c.in)
		if s.limit != c.want {
			t.Errorf("setLimit(%d) after a first of 3 = %d, want %d", c.in, s.limit, c.want)
		}
	}

	echo := startTCP(t, echoUpstream)
	p := newPair(t, pairOpts{
		host:   HostOptions{MaxConns: 9},
		client: ClientOptions{MaxConns: 5, DialWait: 200 * time.Millisecond},
		allow:  []int{echo},
	})
	p.client.SetMaxConns(2)
	p.client.SetMaxConns(7)
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for range 2 {
		c, err := dialWithin(p, echo, 2*time.Second)
		if err != nil {
			t.Fatalf("a stream within the ceiling of 2: %v", err)
		}
		held = append(held, c)
	}
	_, err := p.client.Dial(context.Background(), echo)
	var re *RefusedError
	if !errors.As(err, &re) || re.Reason != wire.ReasonMaxConns {
		t.Fatalf("a third stream after SetMaxConns(2) then (7): %v; want max_conns (the first value is the ceiling)", err)
	}
}
