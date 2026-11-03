package clankmailplugin

// tamper_test.go — a hostile hub. The plugin talks to the hub through a
// reverse proxy that rewrites what the hub serves. Every case here must
// deliver NOTHING to muster, and each says which guard stopped it.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// rewriteInbox applies f to each item of every `queue=inbox` answer.
func rewriteInbox(f func(items []InboxItem) []InboxItem) func(*http.Request, []byte) []byte {
	return func(r *http.Request, body []byte) []byte {
		if r.URL.Path != "/v1/inbox" || r.URL.Query().Get("queue") != "inbox" {
			return body
		}
		var page struct {
			Items []InboxItem `json:"items"`
			Next  string      `json:"next"`
		}
		if json.Unmarshal(body, &page) != nil {
			return body
		}
		page.Items = f(page.Items)
		b, _ := json.Marshal(page)
		return b
	}
}

// outcomeOf: the recorded outcome of id ("" when there is no record).
func outcomeOf(p *Plugin, id string) string {
	var out string
	p.st.view(func(s *State) {
		if m := s.Messages[id]; m != nil {
			out = m.Outcome
		}
	})
	return out
}

func assertNothingDelivered(t *testing.T, e *env) {
	t.Helper()
	if n := len(e.muster.messagePosts()); n != 0 {
		t.Fatalf("muster received %d message(s) from a tampered feed", n)
	}
}

// One byte of the signed payload changed ("invoice 1" → "invoice 9"), the
// signature kept: valid JSON, a valid schema, the peer's signer — and §4 step 4
// (E_SIGNATURE) is the only thing that stops it.
func TestTamperFlippedByteIsRejected(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "pay invoice 1", "")
	e.setTamper(rewriteInbox(func(items []InboxItem) []InboxItem {
		for i := range items {
			parts := strings.Split(items[i].Token, ".")
			payload, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil || !bytes.Contains(payload, []byte("invoice 1")) {
				t.Errorf("setup: cannot find the byte to flip")
				continue
			}
			parts[2] = base64.RawURLEncoding.EncodeToString(bytes.Replace(payload, []byte("invoice 1"), []byte("invoice 9"), 1))
			items[i].Token = strings.Join(parts, ".")
			// No unsigned thread to disagree with: the signature check must be
			// the ONLY guard this item meets.
			items[i].Thread = ""
		}
		return items
	}))
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	assertNothingDelivered(t, e)
	var rejected int
	var cur string
	p.st.view(func(s *State) { rejected, cur = s.Rejected, s.InboxCursor })
	if rejected != 1 || cur == "" {
		t.Fatalf("rejected=%d cursor=%q: want the forged item counted and passed", rejected, cur)
	}
	if !strings.Contains(e.logs.String(), `"code":"E_SIGNATURE"`) {
		t.Fatalf("the rejection was not for the signature: %s", e.logs.String())
	}
}

// An authentic message from the peer to SOMEONE ELSE, served in our inbox:
// our key is not in `to` (misdelivered, SPEC §7.2).
func TestTamperOurKeyNotInToIsNotDelivered(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	other := e.send(e.peer.chief, []string{KeyIDOf(e.stranger.chief)}, "", "for someone else", "")
	tok, found, err := e.peer.hub.Object(bg, other)
	e.must(err)
	if !found {
		t.Fatal("setup: the peer cannot read its own message")
	}
	e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "for us", "") // something to swap
	e.setTamper(rewriteInbox(func(items []InboxItem) []InboxItem {
		for i := range items {
			items[i].Token, items[i].Thread = tok, "" // only the `to` check may stop it
		}
		return items
	}))
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	assertNothingDelivered(t, e)
	if got := outcomeOf(p, other); got != outNotMine {
		t.Fatalf("outcome %q, want %q", got, outNotMine)
	}
}

// A stranger's authentic message to us (which the hub put in `requests`)
// served in the INBOX queue: its signer is no pinned peer.
func TestTamperNonPeerKeyInInboxIsNotDelivered(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	const body = "STRANGER-SMUGGLED-BODY"
	sid := e.send(e.stranger.chief, []string{KeyIDOf(e.us.chief)}, "", body, "")
	reqs, _, err := e.us.hub.Inbox(bg, "requests", "", 10)
	e.must(err)
	e.setTamper(rewriteInbox(func(items []InboxItem) []InboxItem { return append(items, reqs...) }))
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	assertNothingDelivered(t, e)
	if got := outcomeOf(p, sid); got != outNotPeer {
		t.Fatalf("outcome %q, want %q", got, outNotPeer)
	}
	if strings.Contains(e.stateFile(), body) || strings.Contains(e.logs.String(), body) {
		t.Fatal("the smuggled body was written to state or logs")
	}
}

// The hub's UNSIGNED thread swapped to another root: the derived root
// disagrees, so this hub is lying about this message — not delivered, and the
// swapped value never reaches muster.
func TestTamperSwappedUnsignedThreadIsNotDelivered(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	us := KeyIDOf(e.us.chief)
	decoy := e.send(e.peer.chief, []string{us}, "", "another thread", "")
	if err := p.PollInbox(bg); err != nil { // decoy delivered honestly
		t.Fatal(err)
	}
	root := e.send(e.peer.chief, []string{us}, "", "root", "")
	reply := e.send(e.peer.chief, []string{us}, "", "reply", root)
	e.setTamper(rewriteInbox(func(items []InboxItem) []InboxItem {
		for i := range items {
			items[i].Thread = decoy
		}
		return items
	}))
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts := e.muster.messagePosts()
	if len(posts) != 1 {
		t.Fatalf("muster received %d messages, want only the honest decoy", len(posts))
	}
	for _, id := range []string{root, reply} {
		if got := outcomeOf(p, id); got != outHubThread {
			t.Errorf("%s: outcome %q, want %q", id, got, outHubThread)
		}
	}
	// Control: with the tamper off, the hub's own thread agrees and a new reply
	// is delivered, threaded to the DERIVED root.
	e.setTamper(nil)
	again := e.send(e.peer.chief, []string{us}, "", "reply 2", root)
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts = e.muster.messagePosts()
	if len(posts) != 2 {
		t.Fatalf("control: want the honest reply delivered, muster has %d", len(posts))
	}
	m := decodeMsg(t, posts[1])
	if m["external_id"] != again || m["thread"] != root {
		t.Fatalf("control: %v thread %v, want %s thread %s", m["external_id"], m["thread"], again, root)
	}
}

// The handle record served under a different principal key: the chain no
// longer binds the pinned key (and the TOFU principal changed) → the peer is
// DISABLED, its key is DELETEd from our allow list at the hub, and its
// messages are not delivered.
func TestTamperHandleServedWithADifferentKeyDisablesThePeer(t *testing.T) {
	e := newEnv(t)
	p := e.ready("") // the honest first check records the principal
	impostor := newKey(t)
	forged, _, err := SignObject(impostor, "handle", e.clk.Now().UnixMilli(), map[string]any{"name": "alice", "hub": testHub})
	e.must(err)
	e.setTamper(func(r *http.Request, body []byte) []byte {
		if r.URL.Path != "/v1/handles/alice" {
			return body
		}
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		v["handle"] = forged
		b, _ := json.Marshal(v)
		return b
	})
	if err := p.SyncPeers(bg); err != nil {
		t.Fatal(err)
	}
	_, status, _ := p.peerFor(KeyIDOf(e.peer.chief))
	if status != peerDisabled {
		t.Fatalf("peer status %q, want disabled", status)
	}
	deleted := false
	for _, c := range e.hubCalls() {
		if c.Method == http.MethodDelete && c.Path == "/v1/allow/"+KeyIDOf(e.peer.chief) {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("the disabled peer's key was not DELETEd from the allow list")
	}
	e.setTamper(nil)
	m := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "after", "")
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if err := p.PollRequests(bg); err != nil {
		t.Fatal(err)
	}
	assertNothingDelivered(t, e)
	if got := p.RequestsList()["count"]; got != 1 {
		t.Fatalf("after the DELETE the peer's message should land in requests, count=%v", got)
	}
	_ = m
	// Sticky: an honest hub again does not re-enable it by itself.
	if err := p.SyncPeers(bg); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := p.peerFor(KeyIDOf(e.peer.chief)); status != peerDisabled {
		t.Fatalf("a periodic sync re-enabled a disabled peer (%s)", status)
	}
	// Only the human's re-check does.
	out, err := p.VerifyPeer(bg, "alice/chief", false)
	e.must(err)
	if out["status"] != peerOK {
		t.Fatalf("peer verify on an honest hub: %v", out)
	}
}

// A peer delivered while ok, then disabled: its message already in the inbox
// queue is not delivered (peer_disabled), and a draft answering it is refused.
func TestDisabledPeerMessagesAndRepliesStop(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	a := e.delivered(p, "", "first")
	later := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "second", "")
	e.must(p.st.update(func(s *State) error {
		s.Peers[KeyIDOf(e.peer.chief)].Status = peerDisabled
		return nil
	}))
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(p, later); got != outPeerDisabled {
		t.Fatalf("outcome %q, want %q", got, outPeerDisabled)
	}
	e.muster.queueDraft(3, a, "reply")
	if _, err := p.ClaimOnce(bg); err != nil {
		t.Fatal(err)
	}
	if res := e.muster.resultsFor("3"); len(res) != 1 || res[0]["ok"] != false {
		t.Fatalf("a reply to a disabled peer: %v, want ok:false", res)
	}
	if n := len(e.peerInbox()); n != 0 {
		t.Fatalf("%d reply(ies) sent to a disabled peer", n)
	}
}
