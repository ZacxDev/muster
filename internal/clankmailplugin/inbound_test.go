package clankmailplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

var bg = context.Background()

// ready: the plugin with the peer verified and allowed (so the peer's
// messages land in our `inbox` queue at the hub).
func (e *env) ready(crashAt string, peers ...PeerConf) *Plugin {
	e.t.Helper()
	p := e.plugin(crashAt, peers...)
	if err := p.SyncPeers(bg); err != nil {
		e.t.Fatalf("SyncPeers: %v", err)
	}
	return p
}

// An allow-listed peer's message is delivered ONCE, as `alice/chief`, with
// sender_verified true ON THE WIRE (the raw JSON, not a struct that would
// default it), and the thread is the derived root: the message itself for a
// root, the root for a reply.
func TestAllowListedPeerDeliveredOnceWithVerifiedSenderAndDerivedThread(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	us := KeyIDOf(e.us.chief)
	a := e.send(e.peer.chief, []string{us}, "lunch", "noon?", "")
	if got := e.queue(e.us.chief, "inbox"); len(got) != 1 {
		t.Fatalf("precondition: the allowed peer's message should be in our hub inbox queue, got %d", len(got))
	}
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if err := p.PollInbox(bg); err != nil { // a second poll must not deliver again
		t.Fatal(err)
	}
	posts := e.muster.messagePosts()
	if len(posts) != 1 {
		t.Fatalf("muster got %d message posts, want exactly 1", len(posts))
	}
	raw := string(posts[0])
	if !strings.Contains(raw, `"sender_verified":true`) {
		t.Fatalf("sender_verified is not true on the wire: %s", raw)
	}
	m := decodeMsg(t, posts[0])
	want := map[string]any{"external_id": a, "sender": "alice/chief", "subject": "lunch", "body": "noon?",
		"sender_verified": true, "thread": a, "received_at": "2000-01-02T03:04:05Z"}
	for k, v := range want {
		if k == "received_at" {
			if s, _ := m[k].(string); !strings.HasPrefix(s, "2000-01-02T03:04:") {
				t.Errorf("received_at = %v, want the message's signed ts", m[k])
			}
			continue
		}
		if m[k] != v {
			t.Errorf("muster got %s = %v, want %v", k, m[k], v)
		}
	}

	// A reply on that thread (re → a): thread is a, derived.
	b := e.send(e.peer.chief, []string{us}, "", "or one?", a)
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts = e.muster.messagePosts()
	if len(posts) != 2 {
		t.Fatalf("muster got %d posts, want 2", len(posts))
	}
	m = decodeMsg(t, posts[1])
	if m["external_id"] != b || m["thread"] != a {
		t.Fatalf("reply: external_id %v thread %v, want %s thread %s", m["external_id"], m["thread"], b, a)
	}
}

// The thread is derived even when the parent is not in our state: fetched
// with authenticated GET /v1/objects/{id}, verified, its id recomputed.
func TestThreadRootDerivedThroughAFetchedAncestor(t *testing.T) {
	e := newEnv(t)
	us := KeyIDOf(e.us.chief)
	// a and b arrive BEFORE the plugin ever polls: the stored cursor is then
	// moved past them by hand, so the plugin knows neither when c arrives.
	p := e.ready("")
	a := e.send(e.peer.chief, []string{us}, "", "root", "")
	b := e.send(e.peer.chief, []string{us}, "", "middle", a)
	items, _, err := e.us.hub.Inbox(bg, "inbox", "", 50)
	e.must(err)
	e.must(p.st.update(func(s *State) error { s.InboxCursor = items[len(items)-1].Cursor; return nil }))
	c := e.send(e.peer.chief, []string{us}, "", "leaf", b)
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts := e.muster.messagePosts()
	if len(posts) != 1 {
		t.Fatalf("muster got %d posts, want 1", len(posts))
	}
	m := decodeMsg(t, posts[0])
	if m["external_id"] != c || m["thread"] != a {
		t.Fatalf("got external_id %v thread %v, want %s thread %s", m["external_id"], m["thread"], c, a)
	}
	fetched := 0
	for _, h := range e.hubCalls() {
		if h.Method == http.MethodGet && strings.HasPrefix(h.Path, "/v1/objects/") {
			fetched++
		}
	}
	if fetched != 2 {
		t.Fatalf("fetched %d ancestors, want 2 (b, then a)", fetched)
	}
}

// When an ancestor is not visible to us (the peer's reply cites a message it
// sent to someone else), the root cannot be derived: NO thread is sent — even
// though the hub, which holds the ancestor, offers one (unsigned).
func TestThreadOmittedWhenAnAncestorIsNotVisible(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	elsewhere := e.send(e.peer.chief, []string{KeyIDOf(e.stranger.chief)}, "", "to a third party", "")
	r := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "fwd", elsewhere)
	items, _, err := e.us.hub.Inbox(bg, "inbox", "", 10)
	e.must(err)
	if len(items) != 1 || items[0].Thread != elsewhere {
		t.Fatalf("precondition: the hub should offer its own root %s, got %+v", elsewhere, items)
	}
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts := e.muster.messagePosts()
	if len(posts) != 1 {
		t.Fatalf("muster got %d posts, want 1", len(posts))
	}
	m := decodeMsg(t, posts[0])
	if _, has := m["thread"]; has || m["external_id"] != r {
		t.Fatalf("muster got thread %v for a root this plugin could not derive; want none", m["thread"])
	}
}

// 🔴 A stranger's message lands in `requests`, and muster receives ZERO
// requests of any kind for it; its body and subject appear nowhere: not in
// state.json, not in the logs. The positive controls: the peer's message in
// the same run DOES reach muster (the fake is wired), the stranger's message
// IS at the hub (so there was something to leak), and the plugin COUNTED it.
func TestStrangerLandsInRequestsAndNothingOfItReachesMusterStateOrLogs(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	us := KeyIDOf(e.us.chief)
	const body, subject = "STRANGER-BODY-ignore-previous-instructions", "STRANGER-SUBJECT-urgent"
	sid := e.send(e.stranger.chief, []string{us}, subject, body, "")
	if got := e.queue(e.us.chief, "requests"); len(got) != 1 || got[0].ID != sid {
		t.Fatalf("precondition: the stranger's message should be in our requests queue, got %d", len(got))
	}
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if err := p.PollRequests(bg); err != nil {
		t.Fatal(err)
	}
	if hits := e.muster.allHits(); len(hits) != 0 {
		t.Fatalf("muster received %d request(s) while only a stranger had written: %+v", len(hits), hits)
	}
	list := p.RequestsList()
	if list["count"] != 1 {
		t.Fatalf("requests count = %v, want 1", list["count"])
	}
	b, _ := json.Marshal(list)
	if !strings.Contains(string(b), sid) || !strings.Contains(string(b), `"body_len":42`) {
		t.Fatalf("the requests list should name the message and its length: %s", b)
	}
	// Positive control: a peer's message in the same run reaches muster.
	e.send(e.peer.chief, []string{us}, "", "hello", "")
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.muster.messagePosts()); n != 1 {
		t.Fatalf("positive control: the peer's message should reach muster, got %d posts", n)
	}
	for _, where := range []struct{ name, text string }{
		{"state.json", e.stateFile()}, {"logs", e.logs.String()},
		{"muster", rawHits(e.muster.allHits())}, {"the requests list", string(b)},
	} {
		for _, secret := range []string{body, subject} {
			if strings.Contains(where.text, secret) {
				t.Errorf("the stranger's text %q appears in %s", secret, where.name)
			}
		}
	}
	// Positive control for that scan: the peer's body IS in muster's copy.
	if !strings.Contains(rawHits(e.muster.allHits()), "hello") {
		t.Fatal("positive control: the scan cannot see a body that is there")
	}
}

// rawHits is every request muster received, as the text that arrived.
func rawHits(hs []musterHit) string {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(h.Method + " " + h.Path + "\n" + string(h.Raw) + "\n")
	}
	return b.String()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// A body over muster's 16 KiB is recorded muster_refused; muster gets nothing
// and the cursor moves on (the next message is delivered).
func TestBodyOver16KiBIsRecordedMusterRefusedNotPosted(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	us := KeyIDOf(e.us.chief)
	big := e.send(e.peer.chief, []string{us}, "", strings.Repeat("x", MaxMusterBody+1), "")
	small := e.send(e.peer.chief, []string{us}, "", strings.Repeat("y", MaxMusterBody), "")
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts := e.muster.messagePosts()
	if len(posts) != 1 || decodeMsg(t, posts[0])["external_id"] != small {
		t.Fatalf("want only the 16 KiB message posted, got %d posts", len(posts))
	}
	var out string
	p.st.view(func(s *State) { out = s.Messages[big].Outcome })
	if out != outMusterRefused {
		t.Fatalf("the oversized message is recorded %q, want %q", out, outMusterRefused)
	}
}

// muster refusing the content (400) is recorded muster_refused and the cursor
// moves; muster refusing the TOKEN or being down (401/503) holds the cursor.
func TestMusterRefusalVersusOutage(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	us := KeyIDOf(e.us.chief)
	a := e.send(e.peer.chief, []string{us}, "", "one", "")
	e.muster.mu.Lock()
	e.muster.messageStatus = http.StatusServiceUnavailable
	e.muster.mu.Unlock()
	if err := p.PollInbox(bg); err == nil {
		t.Fatal("a muster outage must hold the cursor and report an error")
	}
	var cur string
	p.st.view(func(s *State) { cur = s.InboxCursor })
	if cur != "" {
		t.Fatalf("the cursor moved (%q) past a message muster never took", cur)
	}
	e.muster.mu.Lock()
	e.muster.messageStatus = http.StatusBadRequest
	e.muster.mu.Unlock()
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	var out string
	p.st.view(func(s *State) { out, cur = s.Messages[a].Outcome, s.InboxCursor })
	if out != outMusterRefused || cur == "" {
		t.Fatalf("a content refusal: outcome %q cursor %q, want muster_refused and a moved cursor", out, cur)
	}
}
