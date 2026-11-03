package clankmailplugin

import (
	"strings"
	"testing"
	"time"
)

// delivered: the peer wrote message a to us and the plugin delivered it.
func (e *env) delivered(p *Plugin, subject, body string) string {
	e.t.Helper()
	a := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, subject, body, "")
	if err := p.PollInbox(bg); err != nil {
		e.t.Fatal(err)
	}
	return a
}

// peerInbox: what our replies did at the hub — the messages from us in the
// peer's `inbox` queue (the reply route, SPEC §7.2), verified.
func (e *env) peerInbox() []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, o := range e.queue(e.peer.chief, "inbox") {
		if o.From == KeyIDOf(e.us.chief) {
			o.Fields["id"] = o.ID
			out = append(out, o.Fields)
		}
	}
	return out
}

// An approved reply is signed by our key, verifies at the hub (it was
// accepted and the peer's copy passes §4), has re = the message, to = [the
// peer], lands in the PEER's inbox queue (the reply route), and its result is
// ok:true with the hub id. And the peer's answer to it threads back to the
// original root.
func TestApprovedReplyVerifiesAtHubAndLandsInPeerInbox(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	a := e.delivered(p, "lunch", "noon?")
	e.muster.queueDraft(17, a, "Noon works.")
	if handled, err := p.ClaimOnce(bg); err != nil || !handled {
		t.Fatalf("ClaimOnce: handled=%v err=%v", handled, err)
	}
	got := e.peerInbox()
	if len(got) != 1 {
		t.Fatalf("the peer's inbox queue holds %d replies, want 1", len(got))
	}
	r := got[0]
	to := strList(r["to"])
	if r["re"] != a || len(to) != 1 || to[0] != KeyIDOf(e.peer.chief) || r["body"] != "Noon works." || r["subject"] != "lunch" {
		t.Fatalf("reply fields wrong: %v", r)
	}
	if len(e.queue(e.peer.chief, "requests")) != 0 {
		t.Fatal("the reply landed in the peer's requests queue, not its inbox")
	}
	res := e.muster.resultsFor("17")
	if len(res) != 1 || res[0]["ok"] != true || res[0]["external_ref"] != r["id"] {
		t.Fatalf("result = %v, want one ok:true with external_ref %v", res, r["id"])
	}
	// The peer answers our reply: the thread is still a.
	c := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "see you", r["id"].(string))
	if err := p.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts := e.muster.messagePosts()
	m := decodeMsg(t, posts[len(posts)-1])
	if m["external_id"] != c || m["thread"] != a {
		t.Fatalf("the answer to our reply: external_id %v thread %v, want %s thread %s", m["external_id"], m["thread"], c, a)
	}
}

// A draft for a message the plugin never delivered is refused (ok:false) and
// nothing is sent.
func TestReplyToUnknownMessageRefused(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	e.muster.queueDraft("9", "m_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "hi")
	if _, err := p.ClaimOnce(bg); err != nil {
		t.Fatal(err)
	}
	res := e.muster.resultsFor("9")
	if len(res) != 1 || res[0]["ok"] != false {
		t.Fatalf("result = %v, want ok:false", res)
	}
	if n := len(e.peerInbox()); n != 0 {
		t.Fatalf("%d message(s) sent for a refused draft", n)
	}
}

// ---- crash injection: exactly one message at each of the three points ----

// Crash after muster's 2xx, before the state write: the restarted plugin
// reads the item again and re-POSTs it; muster dedupes on external_id, so
// muster holds ONE message (and the second post is that duplicate).
func TestCrashAfterMuster2xxBeforeStateWriteDeliversExactlyOnce(t *testing.T) {
	e := newEnv(t)
	p1 := e.ready(CrashAfterMuster)
	a := e.send(e.peer.chief, []string{KeyIDOf(e.us.chief)}, "", "once", "")
	if err := p1.PollInbox(bg); err != errCrash {
		t.Fatalf("the crash point was not reached: %v", err)
	}
	var cur string
	p1.st.view(func(s *State) { cur = s.InboxCursor })
	if cur != "" {
		t.Fatal("the cursor was written before the crash point: the point is not where the test thinks")
	}
	p1.Close()
	p2 := e.plugin("")
	if err := p2.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	if err := p2.PollInbox(bg); err != nil {
		t.Fatal(err)
	}
	posts := e.muster.messagePosts()
	ids := map[string]int{}
	for _, raw := range posts {
		ids[decodeMsg(t, raw)["external_id"].(string)]++
	}
	if len(ids) != 1 || ids[a] != 2 || len(posts) != 2 {
		t.Fatalf("posts %d by external_id %v: want the one message posted twice (the re-read) and so stored once", len(posts), ids)
	}
}

// Crash after the ledger write, before the submit: nothing is at the hub;
// the restarted plugin resends the ledger's bytes; ONE reply, reported once.
func TestCrashAfterLedgerWriteBeforeSubmitSendsExactlyOnce(t *testing.T) {
	e := newEnv(t)
	p1 := e.ready(CrashAfterLedger)
	a := e.delivered(p1, "", "q")
	e.muster.queueDraft(5, a, "answer")
	if _, err := p1.ClaimOnce(bg); err != errCrash {
		t.Fatalf("the crash point was not reached: %v", err)
	}
	if n := len(e.peerInbox()); n != 0 {
		t.Fatalf("%d message(s) at the hub before the submit: the crash point is not before it", n)
	}
	var tok string
	p1.st.view(func(s *State) { tok = s.Ledger["draft:5"].Token })
	if tok == "" {
		t.Fatal("the ledger holds no signed bytes at the crash point")
	}
	p1.Close()
	p2 := e.plugin("")
	if err := p2.ResumePending(bg); err != nil {
		t.Fatal(err)
	}
	if err := p2.ResumePending(bg); err != nil {
		t.Fatal(err)
	}
	got := e.peerInbox()
	if len(got) != 1 || got[0]["body"] != "answer" {
		t.Fatalf("the peer holds %d replies, want exactly 1", len(got))
	}
	res := e.muster.resultsFor("5")
	if len(res) != 1 || res[0]["ok"] != true || res[0]["external_ref"] != got[0]["id"] {
		t.Fatalf("results %v, want one ok:true naming %v", res, got[0]["id"])
	}
}

// Crash after the submit, before the result: the hub holds the reply; the
// restarted plugin resends the SAME bytes, the hub answers 200 with the same
// id, and the result is ok:true. ONE reply.
func TestCrashAfterSubmitBeforeResultSendsExactlyOnce(t *testing.T) {
	e := newEnv(t)
	p1 := e.ready(CrashAfterSubmit)
	a := e.delivered(p1, "", "q")
	e.muster.queueDraft(6, a, "answer")
	if _, err := p1.ClaimOnce(bg); err != errCrash {
		t.Fatalf("the crash point was not reached: %v", err)
	}
	if n := len(e.peerInbox()); n != 1 {
		t.Fatalf("the hub holds %d replies at the crash point, want 1 (the point is after the submit)", n)
	}
	if n := len(e.muster.resultsFor("6")); n != 0 {
		t.Fatal("a result was posted before the crash point")
	}
	p1.Close()
	e.clk.Advance(2 * time.Minute) // a restart later, inside the 300 s window or not: the id is stored
	p2 := e.plugin("")
	if err := p2.ResumePending(bg); err != nil {
		t.Fatal(err)
	}
	got := e.peerInbox()
	if len(got) != 1 {
		t.Fatalf("the peer holds %d replies, want exactly 1", len(got))
	}
	res := e.muster.resultsFor("6")
	if len(res) != 1 || res[0]["ok"] != true || res[0]["external_ref"] != got[0]["id"] {
		t.Fatalf("results %v, want one ok:true naming %v", res, got[0]["id"])
	}
}

// A ledgered reply never submitted, resent after the 300 s window: E_STALE
// on the SAME bytes means the hub never held them: ok:false, nothing at the
// peer.
func TestStaleResendIsReportedNotSent(t *testing.T) {
	e := newEnv(t)
	p1 := e.ready(CrashAfterLedger)
	a := e.delivered(p1, "", "q")
	e.muster.queueDraft(7, a, "too late")
	if _, err := p1.ClaimOnce(bg); err != errCrash {
		t.Fatalf("the crash point was not reached: %v", err)
	}
	p1.Close()
	e.clk.Advance(6 * time.Minute)
	p2 := e.plugin("")
	if err := p2.ResumePending(bg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.peerInbox()); n != 0 {
		t.Fatalf("%d stale message(s) reached the peer", n)
	}
	res := e.muster.resultsFor("7")
	if len(res) != 1 || res[0]["ok"] != false || !strings.Contains(res[0]["detail"].(string), "E_STALE") {
		t.Fatalf("results %v, want one ok:false naming E_STALE", res)
	}
}

// The same text to the same message under a NEW draft (the first one's
// result never reached muster) is not sent twice: the ledger's bytes are
// reused, and the new draft is reported sent with the same id.
func TestSameTextNewDraftReusesTheLedgeredReply(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	a := e.delivered(p, "", "q")
	e.muster.queueDraft(1, a, "same")
	if _, err := p.ClaimOnce(bg); err != nil {
		t.Fatal(err)
	}
	e.muster.queueDraft(2, a, "same")
	if _, err := p.ClaimOnce(bg); err != nil {
		t.Fatal(err)
	}
	got := e.peerInbox()
	if len(got) != 1 {
		t.Fatalf("the peer holds %d replies, want 1", len(got))
	}
	r1, r2 := e.muster.resultsFor("1"), e.muster.resultsFor("2")
	if len(r1) != 1 || len(r2) != 1 || r2[0]["ok"] != true || r2[0]["external_ref"] != got[0]["id"] {
		t.Fatalf("results %v / %v", r1, r2)
	}
}
