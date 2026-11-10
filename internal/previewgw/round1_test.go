package previewgw

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// A host token revoked while its host is OFFLINE still closes that host's
// previews at the next recheck, and a new token for the same host name starts
// with none: the same window and port get a new id.
func TestRevokeWhileOfflineClosesTheHostsPreviews(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	open := wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r1", Window: "@1", Port: 5173}}
	fh.offers <- open
	old := (<-fh.answers).a.Preview
	if old == "" {
		t.Fatal("no preview id")
	}
	fh.cancel()
	waitFor(t, "the tunnel to go", func() bool { return e.tunnelFor(1, "alpha") == nil })
	for _, rec := range mustList(t, e.store) {
		if _, err := e.store.Revoke(rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.g.Previews(1)); n != 1 {
		t.Fatalf("before the recheck: %d preview(s); want the offline host's 1", n)
	}
	e.clock.Advance(30 * time.Second)
	if n := len(e.g.Previews(1)); n != 0 {
		t.Fatalf("after the recheck: %d preview(s) of a revoked, offline host; want 0", n)
	}
	fh2 := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	open.Request.ID = "r2"
	fh2.offers <- open
	if a := (<-fh2.answers).a; a.Error != "" || a.Preview == old {
		t.Fatalf("open under the new token answered %+v; want a new id, not %q", a, old)
	}
}

// An open line applied after its tunnel stopped being the host's tunnel — it
// was closed, or replaced by a newer one — is refused and stores nothing.
func TestAnOpenAppliedAfterItsTunnelIsGoneIsRefused(t *testing.T) {
	e := newGW(t, gwOpts{})
	tok := e.mint(t, 1, "alpha")
	e.mustDial(t, dialOpts{listener: "public", token: tok, hostName: "alpha"})
	replaced := e.tunnelFor(1, "alpha")
	e.mustDial(t, dialOpts{listener: "mesh", token: tok, hostName: "alpha"})
	closed := e.tunnelFor(1, "alpha")
	req := wire.Request{ID: "r1", Window: "@1", Port: 5173}
	// While the newer tunnel is current, the replaced one's line is refused.
	if a := e.g.openPreview(replaced, req); a.Error != "host_offline" || a.Preview != "" {
		t.Errorf("open on the replaced tunnel answered %+v; want host_offline", a)
	}
	closed.Close()
	waitFor(t, "the newer tunnel to go", func() bool { return e.tunnelFor(1, "alpha") == nil })
	if a := e.g.openPreview(closed, req); a.Error != "host_offline" || a.Preview != "" {
		t.Errorf("open on the closed tunnel answered %+v; want host_offline", a)
	}
	if n := len(e.g.Previews(1)); n != 0 {
		t.Fatalf("%d preview(s) stored; want 0", n)
	}
}

// Request fields are bounded: Window 32, Session 128, Path 512 bytes. At the
// bound an open is accepted; one byte over, it is refused and nothing is
// stored.
func TestOpenRequestFieldsAreBounded(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	ask := func(r wire.Request) wire.Answer {
		fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &r}
		return (<-fh.answers).a
	}
	w32, s128, p512 := "@"+strings.Repeat("1", 31), strings.Repeat("s", 128), "/"+strings.Repeat("p", 511)
	if a := ask(wire.Request{ID: "ok", Window: w32, Session: s128, Path: p512, Port: 3000}); a.Error != "" {
		t.Fatalf("open at every bound answered %+v", a)
	}
	for _, c := range []struct {
		r    wire.Request
		want string
	}{
		{wire.Request{ID: "w", Window: w32 + "1", Port: 3001}, "bad_window"},
		{wire.Request{ID: "s", Window: "@1", Session: s128 + "s", Port: 3002}, "bad_session"},
		{wire.Request{ID: "p", Window: "@1", Path: p512 + "p", Port: 3003}, "bad_path"},
	} {
		if a := ask(c.r); a.Error != c.want || a.Preview != "" {
			t.Errorf("open %s answered %+v; want %s", c.r.ID, a, c.want)
		}
	}
	if n := len(e.g.Previews(1)); n != 1 {
		t.Fatalf("%d preview(s); want only the one at the bounds", n)
	}
	fh.offers <- wire.Offer{Type: wire.TypeClose, Request: &wire.Request{ID: "c", Window: w32 + "1", Preview: "x"}}
	if a := (<-fh.answers).a; a.Error != "bad_request" {
		t.Fatalf("close with an over-long window answered %+v; want bad_request", a)
	}
}

// Answers go out through one worker per tunnel, not one goroutine per line:
// 60 opens to a host that never answers leave the goroutine count about
// where it was.
func TestAnswersUseOneWorkerPerTunnel(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha", holdAnswers: true})
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := range 60 {
		fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: fmt.Sprint(i), Window: "@1", Port: 3000 + i}}
	}
	// Most of the 60 are refused by the open limits; refused or not, each is
	// answered: the worker holds one answer and the other 59 wait in its queue.
	tn := e.tunnelFor(1, "alpha")
	waitFor(t, "60 opens to be applied", func() bool { return len(tn.answers) == 59 })
	time.Sleep(50 * time.Millisecond)
	if grew := runtime.NumGoroutine() - before; grew > 20 {
		t.Fatalf("goroutines grew by %d for 60 unanswered opens; want a bounded worker, not one per line", grew)
	}
}

// A host whose requests outrun the answer queue has its tunnel closed: with
// the host never answering, the worker holds one answer and the queue 64, so
// the 66th open closes the tunnel. Positive control: a host that answers,
// and never has more than answerQueue opens outstanding, gets all 70 answers
// and keeps its tunnel. (Seventy opens in one burst would race the worker:
// whether the queue overflows would depend on how many answers it had sent
// by then, which is exactly the flood the guard is for.)
func TestAFullAnswerQueueClosesTheTunnel(t *testing.T) {
	t.Run("host never answers", func(t *testing.T) {
		e := newGW(t, gwOpts{})
		fh := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha", holdAnswers: true})
		tn := e.tunnelFor(1, "alpha")
		for i := range 70 {
			fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: fmt.Sprint(i), Window: "@1", Port: 3000 + i}}
		}
		closedSoon(t, "the flooding host's tunnel", tn.Done())
	})
	t.Run("positive control: host answers", func(t *testing.T) {
		e := newGW(t, gwOpts{})
		fh := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
		tn := e.tunnelFor(1, "alpha")
		// Batches of answerQueue: each batch fills the queue to its bound even
		// if the worker has taken none yet, and is answered in full before the
		// next is sent, so the queue can never overflow.
		for sent := 0; sent < 70; {
			batch := min(answerQueue, 70-sent)
			for i := sent; i < sent+batch; i++ {
				fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: fmt.Sprint(i), Window: "@1", Port: 3000 + i}}
			}
			for range batch {
				select {
				case <-fh.answers:
				case <-tn.Done():
					t.Fatalf("the tunnel ended with at most %d opens outstanding", answerQueue)
				case <-time.After(5 * time.Second):
					t.Fatal("an answer did not arrive")
				}
			}
			sent += batch
		}
		stillOpen(t, "the answering host's tunnel", tn.Done())
	})
}

// Many distinct failing keys cost about one map operation each: expired keys
// are pruned at most once per window, the map stops growing at its cap
// (new keys share one overflow bucket, throttled together), and a key
// already known keeps its own count.
func TestTheFailLimiterStaysLinearInDistinctKeys(t *testing.T) {
	now := time.Unix(0, 0)
	l := newFailLimiter(10, time.Minute, func() time.Time { return now })
	l.fail("first")
	start := time.Now()
	for i := range 40000 {
		l.fail(fmt.Sprintf("k%d", i))
	}
	d := time.Since(start)
	if l.prunes > 1 {
		t.Fatalf("%d prune scans in one window; want at most 1", l.prunes)
	}
	if d > 2*time.Second {
		t.Fatalf("40000 distinct keys took %v; want well under 2 s", d)
	}
	if n := len(l.by); n > 4096+1 {
		t.Fatalf("the map holds %d keys; want at most the 4096 cap plus the overflow bucket", n)
	}
	if !l.fail("a-new-peer") {
		t.Fatal("a new key past the cap was not throttled with the overflow bucket")
	}
	if l.fail("first") {
		t.Fatal("a known key lost its own count (2 failures throttled)")
	}
	now = now.Add(time.Minute)
	if l.fail("next-window-peer") {
		t.Fatal("a new key in the next window was throttled; the expired keys were not pruned")
	}
	if _, own := l.by["next-window-peer"]; !own || len(l.by) > 2 {
		t.Fatalf("after the next window's prune: own bucket %v, %d keys; want the expired keys gone and the new key its own", own, len(l.by))
	}
	if l.prunes != 2 {
		t.Fatalf("%d prune scans after a second window; want 2", l.prunes)
	}
}

// /self giving no verdict for 9 checks in a row keeps the owner served; at
// the 10th the owner is closed and its new tunnels refused; one later live
// verdict serves it again.
func TestTenMinutesWithoutAVerdictStopsServingTheOwner(t *testing.T) {
	e := newGW(t, gwOpts{})
	e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	tn := e.tunnelFor(1, "alpha")
	e.fm.set("mpl_owner1", http.StatusServiceUnavailable, Self{})
	for i := 1; i <= 9; i++ {
		e.clock.Advance(time.Minute)
		select {
		case <-tn.Done():
			t.Fatalf("the tunnel ended after %d check(s) without a verdict; want it kept through 9", i)
		default:
		}
	}
	stillOpen(t, "the tunnel after 9 checks without a verdict", tn.Done())
	e.clock.Advance(time.Minute)
	closedSoon(t, "the tunnel at the 10th check without a verdict", tn.Done())
	_, resp, err := e.dialHost(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "beta"), hostName: "beta"})
	if err == nil || statusOf(resp) != http.StatusForbidden {
		t.Fatalf("a new tunnel while unverified: err %v, HTTP %d; want 403", err, statusOf(resp))
	}
	e.fm.set("mpl_owner1", http.StatusOK, viewerSelf(1, "one@example.invalid"))
	e.clock.Advance(time.Minute)
	e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "gamma"), hostName: "gamma"})
	// The live verdict restarted the count: one more check without a verdict
	// keeps the owner served.
	tg := e.tunnelFor(1, "gamma")
	e.fm.set("mpl_owner1", http.StatusServiceUnavailable, Self{})
	e.clock.Advance(time.Minute)
	stillOpen(t, "the restored owner's tunnel after one check without a verdict (the count restarted)", tg.Done())
}

// Both listeners get a header timeout and an idle timeout, and no whole-
// request deadline that would cut a long-lived tunnel.
func TestListenersHaveHeaderAndIdleTimeoutsOnly(t *testing.T) {
	e := newGW(t, gwOpts{})
	e.g.cfg.MeshListen = ":0"
	ss := e.g.servers()
	if len(ss) != 2 {
		t.Fatalf("%d servers; want public and mesh", len(ss))
	}
	for _, s := range ss {
		if s.ReadHeaderTimeout != 10*time.Second || s.IdleTimeout != 5*time.Minute || s.ReadTimeout != 0 || s.WriteTimeout != 0 {
			t.Errorf("server %q: header %v idle %v read %v write %v; want 10s, 5m, 0, 0",
				s.Addr, s.ReadHeaderTimeout, s.IdleTimeout, s.ReadTimeout, s.WriteTimeout)
		}
	}
}

// The (owner, host, window, port) index follows closes: reopening a closed
// preview's window and port mints a new id, and the repeat of an open still
// answers the same id.
func TestThePreviewIndexFollowsCloses(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	open := func(id string) string {
		fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: id, Window: "@1", Port: 5173}}
		return (<-fh.answers).a.Preview
	}
	first := open("r1")
	if again := open("r2"); again != first {
		t.Fatalf("repeat open: %q; want %q", again, first)
	}
	fh.offers <- wire.Offer{Type: wire.TypeClose, Request: &wire.Request{ID: "c", Window: "@1", Preview: first}}
	<-fh.answers
	if next := open("r3"); next == "" || next == first || len(e.g.Previews(1)) != 1 {
		t.Fatalf("reopen after close: %q (previews %d); want one new id, not %q", next, len(e.g.Previews(1)), first)
	}
}
