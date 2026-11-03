package clankmailplugin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// held_test.go — a pinned peer's authentic message read while that peer is
// still `unverified` (a fresh or lost STATE_DIR, a relabel, a boot where the
// handles route fails) is HELD, never judged finally; and a pinned peer's
// message the hub put in `requests` is delivered once the peer verifies.

// lostState: the hub's allow list still names the peer (a previous plugin put
// it there), but this plugin's state does not know it: the peer is
// `unverified` and its next message lands in our `inbox` queue.
func (e *env) lostState(peers ...party) {
	e.t.Helper()
	for _, pp := range peers {
		e.must(e.us.hub.Allow(bg, KeyIDOf(pp.chief)))
	}
}

// postedIDs is every external_id muster was POSTed, in order.
func (e *env) postedIDs() []string {
	var out []string
	for _, raw := range e.muster.messagePosts() {
		id, _ := decodeMsg(e.t, raw)["external_id"].(string)
		out = append(out, id)
	}
	return out
}

func (e *env) postsByID() map[string]int {
	n := map[string]int{}
	for _, id := range e.postedIDs() {
		n[id]++
	}
	return n
}

// F1, deterministic: the inbox is polled BEFORE the first peer sync, on a
// fresh state. The peer's message is not delivered yet, and not judged:
// nothing is recorded for it. After the sync it is delivered, exactly once.
func TestUnverifiedPeerMessageIsHeldThenDeliveredOnceAfterVerify(t *testing.T) {
	e := newEnv(t)
	e.lostState(e.peer)
	us := KeyIDOf(e.us.chief)
	a := e.send(e.peer.chief, []string{us}, "hi", "before the sync", "")
	if got := e.queue(e.us.chief, "inbox"); len(got) != 1 {
		t.Fatalf("precondition: the peer's message should be in our inbox queue, got %d", len(got))
	}
	p := e.plugin("")
	if _, status, _ := p.peerFor(KeyIDOf(e.peer.chief)); status != peerUnverified {
		t.Fatalf("precondition: the peer is %q, want unverified", status)
	}
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.muster.messagePosts()); n != 0 {
		t.Fatalf("an unverified peer's message reached muster before its chain verified (%d posts)", n)
	}
	if got := outcomeOf(p, a); got != "" {
		t.Fatalf("an unverified peer's message was judged finally: recorded %q", got)
	}
	if err := p.SyncPeers(bg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := p.PollInbox(bg); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.postsByID(); len(got) != 1 || got[a] != 1 {
		t.Fatalf("after the peer verified: muster posts by id %v, want %s exactly once", got, a)
	}
	m := decodeMsg(t, e.muster.messagePosts()[0])
	if m["sender"] != "alice/chief" || m["sender_verified"] != true || m["thread"] != a {
		t.Fatalf("the held message was delivered as %v", m)
	}
	if got := outcomeOf(p, a); got != outDelivered {
		t.Fatalf("recorded %q, want %q", got, outDelivered)
	}
}

// A peer that stays unverified (its handle route down) does not wedge the
// inbox for OTHER peers; its own messages keep their order once it verifies.
func TestStuckUnverifiedPeerDoesNotBlockOtherPeers(t *testing.T) {
	e := newEnv(t)
	bob := e.newParty("bob")
	e.lostState(e.peer, bob)
	us := KeyIDOf(e.us.chief)
	p := e.plugin("", e.peerConf(e.peer), e.peerConf(bob))
	// The hub's handle route fails for alice only: bob verifies, alice cannot.
	e.setTamper(func(r *http.Request, body []byte) []byte {
		if r.URL.Path == "/v1/handles/alice" {
			return []byte("not json")
		}
		return body
	})
	if err := p.SyncPeers(bg); err == nil {
		t.Fatal("setup: alice's broken handle route should make the sync report an error")
	}
	if _, s, _ := p.peerFor(KeyIDOf(bob.chief)); s != peerOK {
		t.Fatalf("setup: bob is %q, want ok", s)
	}
	if _, s, _ := p.peerFor(KeyIDOf(e.peer.chief)); s != peerUnverified {
		t.Fatalf("setup: alice is %q, want unverified", s)
	}
	a1 := e.send(e.peer.chief, []string{us}, "", "alice 1", "")
	b1 := e.send(bob.chief, []string{us}, "", "bob 1", "")
	a2 := e.send(e.peer.chief, []string{us}, "", "alice 2", a1)
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.postedIDs(); len(got) != 1 || got[0] != b1 {
		t.Fatalf("with alice stuck, muster got %v, want only bob's %s", got, b1)
	}
	b2 := e.send(bob.chief, []string{us}, "", "bob 2", "")
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.postedIDs(); len(got) != 2 || got[1] != b2 {
		t.Fatalf("a later bob message behind alice's held ones: muster got %v", got)
	}
	e.setTamper(nil)
	if err := p.SyncPeers(bg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := p.PollInbox(bg); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{b1, b2, a1, a2}
	if got := e.postedIDs(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("muster got %v, want %v (each once, alice's in order)", got, want)
	}
	if m := decodeMsg(t, e.muster.messagePosts()[3]); m["thread"] != a1 {
		t.Fatalf("alice 2's thread %v, want %s", m["thread"], a1)
	}
}

// F1 through the real Run, repeated: a fresh state whose peer is already on
// the hub allow list, a message waiting in the inbox at boot. Every run
// delivers it, exactly once.
func TestRunOnFreshStateNeverLosesAPinnedPeersMessage(t *testing.T) {
	const runs = 20
	lost := 0
	for i := 0; i < runs; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			e := newEnv(t)
			e.lostState(e.peer)
			a := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "waiting at boot", "")
			p, err := New(e.config(), Options{Now: e.clk.Now, Log: slog.New(slog.NewJSONHandler(e.logs, nil))})
			e.must(err)
			defer p.Close()
			ctx, cancel := context.WithCancel(bg)
			done := make(chan error, 1)
			go func() { done <- p.Run(ctx) }()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && len(e.muster.messagePosts()) == 0 {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond) // a few more poll cycles: no second post
			cancel()
			<-done
			if got := e.postsByID(); len(got) != 1 || got[a] != 1 {
				lost++
				t.Errorf("muster posts by id %v (outcome %q), want %s exactly once", got, outcomeOf(p, a), a)
			}
		})
	}
	if lost != 0 {
		t.Fatalf("lost or duplicated %d/%d", lost, runs)
	}
}

// Run makes one peer-sync attempt before the first inbox or requests poll.
func TestRunSyncsPeersBeforeTheFirstPoll(t *testing.T) {
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			e := newEnv(t)
			p, err := New(e.config(), Options{Now: e.clk.Now, Log: slog.New(slog.NewJSONHandler(e.logs, nil))})
			e.must(err)
			defer p.Close()
			ctx, cancel := context.WithCancel(bg)
			done := make(chan error, 1)
			go func() { done <- p.Run(ctx) }()
			waitFor(t, "an inbox poll", func() bool {
				for _, c := range e.hubCalls() {
					if c.Path == "/v1/inbox" {
						return true
					}
				}
				return false
			})
			cancel()
			<-done
			allow, inbox := -1, -1
			for i, c := range e.hubCalls() {
				if c.Method == http.MethodPost && c.Path == "/v1/allow" && allow < 0 {
					allow = i
				}
				if c.Path == "/v1/inbox" && inbox < 0 {
					inbox = i
				}
			}
			if allow < 0 || inbox < allow {
				t.Fatalf("the first inbox read (call %d) came before the first peer sync finished (allow at call %d)", inbox, allow)
			}
		})
	}
}

// Only `disabled` is final: a disabled peer's inbox message is recorded
// peer_disabled, and re-enabling the peer later does not deliver it.
func TestDisabledPeerInboxMessageIsFinal(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	m := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "while disabled", "")
	e.must(p.st.update(func(s *State) error { s.Peers[KeyIDOf(e.peer.chief)].Status = peerDisabled; return nil }))
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(p, m); got != outPeerDisabled {
		t.Fatalf("recorded %q, want %q", got, outPeerDisabled)
	}
	if _, err := p.VerifyPeer(bg, "alice/chief", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := p.PollInbox(bg); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.muster.messagePosts()); n != 0 {
		t.Fatalf("a message judged peer_disabled was delivered after the peer was re-enabled (%d posts)", n)
	}
}

// F2: the peer's message reached the hub's `requests` queue before our allow
// call did. Once the peer verifies it is delivered exactly like an inbox
// message: once, as alice/chief, sender_verified, on the derived thread.
func TestPinnedPeerEarlyRequestDeliveredOnceVerified(t *testing.T) {
	e := newEnv(t)
	us := KeyIDOf(e.us.chief)
	a := e.send(e.peer.chief, []string{us}, "early", "before the allow", "")
	if got := e.queue(e.us.chief, "requests"); len(got) != 1 || got[0].ID != a {
		t.Fatalf("precondition: the peer's message should be in our requests queue, got %d", len(got))
	}
	p := e.plugin("")
	if err := p.PollRequests(bg); err != nil { // before the sync: held, not delivered
		t.Fatal(err)
	}
	if n := len(e.muster.messagePosts()); n != 0 {
		t.Fatalf("delivered before the peer verified (%d posts)", n)
	}
	if err := p.SyncPeers(bg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := p.PollRequests(bg); err != nil {
			t.Fatal(err)
		}
		if err := p.PollInbox(bg); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.postsByID(); len(got) != 1 || got[a] != 1 {
		t.Fatalf("muster posts by id %v, want %s exactly once", got, a)
	}
	m := decodeMsg(t, e.muster.messagePosts()[0])
	if m["sender"] != "alice/chief" || m["sender_verified"] != true || m["thread"] != a || m["subject"] != "early" {
		t.Fatalf("delivered as %v", m)
	}
	if got := outcomeOf(p, a); got != outDelivered {
		t.Fatalf("recorded %q, want delivered", got)
	}
	// A reply to it is accepted (it is a delivered message like any other).
	e.muster.queueDraft(5, a, "got it")
	if _, err := p.ClaimOnce(bg); err != nil {
		t.Fatal(err)
	}
	if res := e.muster.resultsFor("5"); len(res) != 1 || res[0]["ok"] != true {
		t.Fatalf("a reply to a delivered request: %v", res)
	}
}

// F2: the same with the peer already verified when the requests poll first
// reads it (the message reached `requests` before the allow; the sync ran
// first): delivered directly.
func TestPinnedPeerRequestReadAfterVerifyIsDelivered(t *testing.T) {
	e := newEnv(t)
	a := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "early", "")
	p := e.ready("")
	if err := p.PollRequests(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.postsByID(); len(got) != 1 || got[a] != 1 {
		t.Fatalf("muster posts by id %v, want %s once", got, a)
	}
	if n := p.RequestsList()["count"]; n != 0 {
		t.Fatalf("a delivered peer message is also listed as an undelivered request (count %v)", n)
	}
}

// Order within a peer: a message from a peer that is ok NOW is held behind
// that peer's earlier held message (the race where the peer verifies between
// the held walk and the page read), so muster gets them in order.
func TestOkPeerMessageWaitsBehindItsOwnHeldMessage(t *testing.T) {
	e := newEnv(t)
	e.lostState(e.peer)
	us := KeyIDOf(e.us.chief)
	p := e.plugin("")
	a1 := e.send(e.peer.chief, []string{us}, "", "one", "")
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if !p.isHeld(a1) {
		t.Fatal("setup: a1 is not held")
	}
	a2 := e.send(e.peer.chief, []string{us}, "", "two", "")
	items, _, err := e.us.hub.Inbox(bg, "inbox", "", 10)
	e.must(err)
	if len(items) != 2 {
		t.Fatalf("setup: %d inbox items, want 2", len(items))
	}
	e.must(p.st.update(func(s *State) error { s.Peers[KeyIDOf(e.peer.chief)].Status = peerOK; return nil }))
	if err := p.handleItem(bg, queueInbox, items[1]); err != nil {
		t.Fatal(err)
	}
	if n := len(e.muster.messagePosts()); n != 0 {
		t.Fatalf("a2 overtook the held a1: %d posts", n)
	}
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.postedIDs(); fmt.Sprint(got) != fmt.Sprint([]string{a1, a2}) {
		t.Fatalf("muster got %v, want [a1 a2] = %v", got, []string{a1, a2})
	}
}

// A request listed as a stranger's whose key is LATER pinned is held at the
// next start and delivered once the peer verifies; a held request whose key is
// no longer pinned becomes a listed stranger's request, never delivered.
func TestPinningAKeyAdoptsItsListedRequestsAndUnpinningListsThem(t *testing.T) {
	e := newEnv(t)
	bob := e.newParty("bob")
	us := KeyIDOf(e.us.chief)
	a := e.send(e.peer.chief, []string{us}, "", "from alice, not yet pinned", "")
	p1 := e.ready("", e.peerConf(bob))
	e.must(p1.PollRequests(bg))
	if got := p1.RequestsList()["count"]; got != 1 {
		t.Fatalf("setup: alice unpinned should be listed, count=%v", got)
	}
	p1.Close()

	p2 := e.plugin("", e.peerConf(bob), e.peerConf(e.peer))
	if got := p2.RequestsList()["count"]; got != 0 || !p2.isHeld(a) {
		t.Fatalf("now pinned: listed count=%v held=%v, want 0 and held", got, p2.isHeld(a))
	}
	// Unpinned again before it verified: listed, not delivered.
	p2.Close()
	p3 := e.plugin("", e.peerConf(bob))
	e.must(p3.PollInbox(bg))
	if got := p3.RequestsList()["count"]; got != 1 || p3.isHeld(a) {
		t.Fatalf("unpinned: listed count=%v held=%v, want 1 and not held", got, p3.isHeld(a))
	}
	assertNothingDelivered(t, e)
	p3.Close()

	p4 := e.plugin("", e.peerConf(bob), e.peerConf(e.peer))
	e.must(p4.SyncPeers(bg))
	e.must(p4.PollInbox(bg))
	if got := e.postsByID(); len(got) != 1 || got[a] != 1 {
		t.Fatalf("pinned and verified: posts by id %v, want %s once", got, a)
	}
}

// F2 exactly-once across a restart: the crash lands after muster's 2xx and
// before the state write; the restarted plugin re-posts (muster dedupes on
// external_id) and then never again.
func TestPinnedPeerRequestExactlyOnceAcrossRestart(t *testing.T) {
	e := newEnv(t)
	a := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "early", "")
	p1 := e.ready(CrashAfterMuster)
	if err := p1.PollRequests(bg); err != errCrash {
		t.Fatalf("the crash point was not reached: %v", err)
	}
	p1.Close()
	p2 := e.plugin("")
	for i := 0; i < 3; i++ {
		if err := p2.PollRequests(bg); err != nil {
			t.Fatal(err)
		}
		if err := p2.PollInbox(bg); err != nil {
			t.Fatal(err)
		}
	}
	p2.Close()
	p3 := e.plugin("")
	if err := p3.PollRequests(bg); err != nil {
		t.Fatal(err)
	}
	if err := p3.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.postsByID(); len(got) != 1 || got[a] != 2 {
		t.Fatalf("posts by id %v: want %s posted twice (the re-read after the crash; muster stores it once) and never again", got, a)
	}
	if got := outcomeOf(p3, a); got != outDelivered {
		t.Fatalf("recorded %q, want delivered", got)
	}
}
