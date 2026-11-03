package clankmailplugin

// tamper_test.go — a hostile hub. The plugin talks to the hub through a
// reverse proxy that rewrites what the hub serves. Every forged case here must
// deliver NOTHING to muster, and each says which guard stopped it. The one
// exception is an authentic message the hub merely mis-threads (its unsigned
// `thread`): that is delivered on the derived root, and the lie is counted.

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

// The hub's UNSIGNED thread swapped to another root. The message is authentic,
// so it IS delivered — threaded on the root DERIVED from its signed `re`
// chain, never on the hub's value — and the disagreement is counted, so `show`
// and /health report how many messages this hub mis-threaded.
func TestTamperSwappedUnsignedThreadIsDeliveredOnTheDerivedRootAndCounted(t *testing.T) {
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
	if len(posts) != 3 {
		t.Fatalf("muster received %d messages, want 3: the decoy and both mis-threaded messages", len(posts))
	}
	for i, id := range []string{root, reply} {
		m := decodeMsg(t, posts[1+i])
		if m["external_id"] != id {
			t.Fatalf("post %d is %v, want %s", 1+i, m["external_id"], id)
		}
		if m["thread"] == decoy {
			t.Fatalf("%s: muster was sent the hub's unsigned thread %s", id, decoy)
		}
		if m["thread"] != root {
			t.Fatalf("%s: thread %v, want the derived root %s", id, m["thread"], root)
		}
		if got := outcomeOf(p, id); got != outDelivered {
			t.Errorf("%s: outcome %q, want %q", id, got, outDelivered)
		}
	}
	if got := hubThreadMismatches(t, p); got != 2 {
		t.Fatalf("hub_thread_mismatches %d, want 2 (root and reply)", got)
	}
	// Control: with the tamper off, the hub's own thread agrees: the new reply
	// is delivered on the derived root and NOT counted.
	e.setTamper(nil)
	again := e.send(e.peer.chief, []string{us}, "", "reply 2", root)
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts = e.muster.messagePosts()
	if len(posts) != 4 {
		t.Fatalf("control: want the honest reply delivered, muster has %d", len(posts))
	}
	m := decodeMsg(t, posts[3])
	if m["external_id"] != again || m["thread"] != root {
		t.Fatalf("control: %v thread %v, want %s thread %s", m["external_id"], m["thread"], again, root)
	}
	if got := hubThreadMismatches(t, p); got != 2 {
		t.Fatalf("control: hub_thread_mismatches %d, want still 2 (an honest hub is not counted)", got)
	}
}

// hubThreadMismatches is the count `show` and /health report, read through
// Summary (what an operator sees), not the state field.
func hubThreadMismatches(t *testing.T, p *Plugin) int {
	t.Helper()
	b, err := json.Marshal(p.Summary()["hub_thread_mismatches"])
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if json.Unmarshal(b, &n) != nil {
		t.Fatalf("Summary has no numeric hub_thread_mismatches (got %s)", b)
	}
	return n
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
	if got := e.queue(e.us.chief, "requests"); len(got) != 1 || got[0].ID != m {
		t.Fatalf("after the DELETE the peer's message should land in requests at the hub, got %d", len(got))
	}
	// A pinned key's requests item is held while the peer is disabled, not
	// listed as a stranger's.
	if got := p.RequestsList()["count"]; got != 0 {
		t.Fatalf("the disabled peer's message is listed as a stranger's request, count=%v", got)
	}
	if !p.isHeld(m) {
		t.Fatal("the disabled peer's requests item is not held")
	}
	// Sticky: an honest hub again does not re-enable it by itself.
	if err := p.SyncPeers(bg); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := p.peerFor(KeyIDOf(e.peer.chief)); status != peerDisabled {
		t.Fatalf("a periodic sync re-enabled a disabled peer (%s)", status)
	}
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	assertNothingDelivered(t, e)
	// Only the human's re-check does — and then the held message is delivered, once.
	out, err := p.VerifyPeer(bg, "alice/chief", false)
	e.must(err)
	if out["status"] != peerOK {
		t.Fatalf("peer verify on an honest hub: %v", out)
	}
	for i := 0; i < 2; i++ {
		if err := p.PollRequests(bg); err != nil {
			t.Fatal(err)
		}
		if err := p.PollInbox(bg); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.postsByID(); len(got) != 1 || got[m] != 1 {
		t.Fatalf("after peer verify: muster posts by id %v, want %s once", got, m)
	}
}

// A peer delivered while ok, then disabled: its message already in the inbox
// queue is not delivered (held, never recorded: held.go), and a draft
// answering it is refused.
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
	if got := outcomeOf(p, later); got != "" || !p.isHeld(later) {
		t.Fatalf("the disabled peer's inbox message: recorded %q held %v, want not recorded and held", got, p.isHeld(later))
	}
	if n := len(e.muster.messagePosts()); n != 1 {
		t.Fatalf("%d posts, want only the first message (delivered while ok)", n)
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
