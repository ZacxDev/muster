package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// THE DUPLICATE-LIVE-DRAFT GUARD (internal/plugins outbound.go, FileOutbound):
// officer_post_draft refuses a draft identical — same officer, venue, target
// kind and ref, and body — to one of the officer's drafts that can still go
// out (pending, or approved: unclaimed, claimed, or a retry chain's queued
// successor), naming it. An officer resuming one long conversation once
// judged "an earlier draft is still awaiting approval" from its memory when
// muster's rows said that draft had been refused; muster now decides it.
// Expected strings are written out here, never built from the code's.

const dupBody = "A board where your agents ask before they act."

// postArgs is officer_post_draft's arguments for body on venue with target.
func postArgs(venue, target, body string) string {
	b, _ := json.Marshal(body)
	return `{"plugin":"` + outboundPluginName + `","venue":"` + venue + `","target":` + target + `,"body":` + string(b) + `}`
}

const postTarget = `{"kind":"post"}`

// outboundCount is how many outbound drafts carry body.
func (w *outboundWorld) outboundCount(body string) int {
	w.t.Helper()
	var n int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM plugin_reply_drafts WHERE message_id IS NULL AND body = $1`, body).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// wantDuplicate asserts out is the duplicate refusal naming draft first in
// status phrase.
func wantDuplicate(t *testing.T, what, out string, first int64, phrase string) {
	t.Helper()
	msg := toolError(out)
	prefix := "duplicate_live_draft: draft " + strconv.FormatInt(first, 10) + " has the same venue, target and body and is still live ("
	if !strings.HasPrefix(msg, prefix) || !strings.Contains(msg, phrase) {
		t.Fatalf("%s: %s; want refused %q… naming %q", what, out, prefix, phrase)
	}
}

// TestADuplicateOfALiveOutboundDraftIsRefusedNamingIt is the regression: a
// second identical officer_post_draft while the first is pending, approved
// and unclaimed, and claimed by its plugin is refused each time, naming the
// first draft and its state, and files nothing.
func TestADuplicateOfALiveOutboundDraftIsRefusedNamingIt(t *testing.T) {
	w := newOutboundWorld(t, true)
	args := postArgs("example.forum", postTarget, dupBody)
	first, out := w.postDraft(args)
	if first == 0 {
		t.Fatalf("the first filing: %s", out)
	}
	id, out := w.postDraft(args)
	if id != 0 {
		t.Fatalf("a duplicate of a pending draft was filed as draft %d", id)
	}
	wantDuplicate(t, "while it is pending", out, first, "status pending: awaiting its plugin's check or your owner's decision")
	if rec := w.check(w.tok, first, true, ""); rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.approve(first, bindingForm(dupBody)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	_, out = w.postDraft(args)
	wantDuplicate(t, "while it is approved", out, first, "status approved: approved, waiting for its plugin to post it")
	w.claimed(first)
	_, out = w.postDraft(args)
	wantDuplicate(t, "while its plugin is posting it", out, first, "status approved: approved and being posted by its plugin now")
	if n := w.outboundCount(dupBody); n != 1 {
		t.Fatalf("%d drafts carry the body; want 1, the first", n)
	}
}

// TestARetrySuccessorIsLiveForTheDuplicateGuard: a send that failed with
// nothing sent and a transient cause files an approved successor; the same
// body re-filed is refused naming THE SUCCESSOR, not the failed root.
func TestARetrySuccessorIsLiveForTheDuplicateGuard(t *testing.T) {
	w := newOutboundWorld(t, true)
	_, root := w.approvedForItem("dup:retry")
	w.claimed(root)
	if code, got := w.postResult(root, transientFail); code != http.StatusOK || got["status"] != "failed" {
		t.Fatalf("result: %d %v", code, got)
	}
	succ := w.successors(root)
	if len(succ) != 1 {
		t.Fatalf("%d successors; want 1", len(succ))
	}
	_, out := w.postDraft(postArgs("example.forum", postTarget, retryPost))
	wantDuplicate(t, "while its retry is queued", out, succ[0].ID, "status approved: approved, waiting for its plugin to post it")
}

// TestAnEndedDraftDoesNotBlockARefile is an INVARIANT GUARD, not regression
// coverage (filing after an ended draft worked before the guard too): it pins
// the guard's live set from the other side. A draft refused by its plugin's
// check or rejected by its owner (both through their real routes), or failed,
// expired, superseded, unconfirmed or SENT (set on the row) leaves the same
// body free to file again. Sent is deliberately not live: an identical post
// that already went out is the relay's twin guard's concern.
func TestAnEndedDraftDoesNotBlockARefile(t *testing.T) {
	w := newOutboundWorld(t, true)
	file := func(body string) int64 {
		t.Helper()
		id, out := w.postDraft(postArgs("example.forum", postTarget, body))
		if id == 0 {
			t.Fatalf("file %q: %s", body, out)
		}
		return id
	}
	// approvedClaimed checks draft id sendable, approves it and has its
	// plugin claim it.
	approvedClaimed := func(id int64) {
		t.Helper()
		if rec := w.check(w.tok, id, true, ""); rec.Code != http.StatusOK {
			t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
		}
		if rec := w.approve(id, bindingForm(w.draft(id).Body)); rec.Code != http.StatusSeeOther {
			t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
		}
		w.claimed(id)
	}
	ended := []struct {
		name string
		end  func(id int64)
	}{
		{"refused by its check", func(id int64) {
			if rec := w.check(w.tok, id, false, "venue says no"); rec.Code != http.StatusOK {
				t.Fatalf("refusing check: %d %s", rec.Code, rec.Body.String())
			}
		}},
		{"rejected by its owner", func(id int64) {
			if rec := w.check(w.tok, id, true, ""); rec.Code != http.StatusOK {
				t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
			}
			if rec := w.rejectForm(id, "not this one"); rec.Code != http.StatusSeeOther {
				t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
			}
		}},
		{"failed", func(id int64) {
			approvedClaimed(id)
			if code, got := w.postResult(id, `{"ok":false,"detail":"the venue refused it"}`); code != http.StatusOK || got["status"] != "failed" {
				t.Fatalf("failed result: %d %v", code, got)
			}
		}},
		{"expired", func(id int64) {
			w.draftSQL(id, `expires_at = now() - interval '1 minute', check_due_at = now() - interval '1 minute'`)
			w.sweepByClaim()
		}},
		{"superseded", func(id int64) { w.draftSQL(id, `status = 'superseded', decided_at = now()`) }},
		{"unconfirmed", func(id int64) {
			approvedClaimed(id)
			w.draftSQL(id, `lease_until = now() - interval '1 minute'`)
			w.sweepByClaim()
		}},
		{"sent", func(id int64) {
			approvedClaimed(id)
			if code, got := w.postResult(id, `{"ok":true,"external_ref":"post-1"}`); code != http.StatusOK || got["status"] != "sent" {
				t.Fatalf("sent result: %d %v", code, got)
			}
		}},
	}
	for _, e := range ended {
		body := dupBody + " Ended: " + e.name + "."
		first := file(body)
		e.end(first)
		want := e.name
		if strings.HasPrefix(want, "re") {
			want = "rejected" // refused by its check, rejected by its owner
		}
		if st := string(w.draft(first).Status); st != want {
			t.Fatalf("%s: control: the first draft is %s; want %s", e.name, st, want)
		}
		if again, out := w.postDraft(postArgs("example.forum", postTarget, body)); again == 0 || again == first {
			t.Fatalf("%s: the same body after it ended: %s; want filed", e.name, out)
		}
	}
}

// TestADifferentBodyVenueOrTargetIsNotBlocked: while a draft is pending, a
// draft differing from it in ONLY its body, ONLY its venue, ONLY its target
// ref, or ONLY its target kind (a dm and a reply sharing a ref) files; and
// the control — identical in all — is refused, so the guard is live in this
// very world. An identical live draft of ANOTHER officer, or of the same
// officer id under another owner, does not block either. (The owner half is
// also enforced by row-level security: with the query's owner_id predicate
// removed the other owner's row is still invisible, so that case cannot tell
// the predicate from the policy.)
func TestADifferentBodyVenueOrTargetIsNotBlocked(t *testing.T) {
	w := newOutboundWorld(t, true)
	reply := func(ref string) string { return `{"kind":"reply","ref":"` + ref + `"}` }
	first, out := w.postDraft(postArgs("example.forum", reply("t1_abc"), dupBody))
	if first == 0 {
		t.Fatalf("the first filing: %s", out)
	}
	for _, c := range []struct{ what, args string }{
		{"another body", postArgs("example.forum", reply("t1_abc"), dupBody+" Twice.")},
		{"another venue", postArgs("example.board", reply("t1_abc"), dupBody)},
		{"another target ref", postArgs("example.forum", reply("t1_def"), dupBody)},
		{"another target kind", postArgs("example.forum", postTarget, dupBody)},
	} {
		if id, out := w.postDraft(c.args); id == 0 {
			t.Fatalf("%s: %s; want filed", c.what, out)
		}
	}
	_, out = w.postDraft(postArgs("example.forum", reply("t1_abc"), dupBody))
	wantDuplicate(t, "control: identical in all", out, first, "status pending")

	// ONLY the target kind: a dm stores its person's handle as its ref, so a
	// reply to a thread spelled like that handle has the same venue, ref and
	// body as the dm, and differs from it in its kind alone.
	w.engagement("ann", "reply", "r-ann", "how does it work?")
	const dmBody = "Thanks for asking: it retries with backoff."
	dm, out := w.outreach(map[string]any{"target": map[string]any{"kind": "dm", "handle": "ann"}, "because_ref": "r-ann", "body": dmBody})
	if dm == 0 {
		t.Fatalf("the dm: %s", out)
	}
	if ref := w.draft(dm).TargetRef; ref == nil || *ref != "ann" {
		t.Fatalf("control: the dm's stored ref is %v; want ann", ref)
	}
	if id, out := w.postDraft(postArgs(outVenue, reply("ann"), dmBody)); id == 0 {
		t.Fatalf("another target kind, same ref: %s; want filed", out)
	}

	// ONLY the officer, then ONLY the owner: a live draft moved to another
	// officer of the same owner, or to the same officer id of another owner,
	// leaves the same draft free for this one.
	const movedBody = "A draft that belongs to someone else now."
	moved, out := w.postDraft(postArgs("example.forum", postTarget, movedBody))
	if moved == 0 {
		t.Fatalf("file: %s", out)
	}
	w.draftSQL(moved, `officer = 'marketing:elsewhere'`)
	again, out := w.postDraft(postArgs("example.forum", postTarget, movedBody))
	if again == 0 {
		t.Fatalf("another officer's identical live draft: %s; want filed", out)
	}
	w.draftSQL(again, `owner_id = `+strconv.FormatInt(w.other, 10)+`, plugin_id = (SELECT id FROM plugins WHERE name = 'example-other')`)
	if id, out := w.postDraft(postArgs("example.forum", postTarget, movedBody)); id == 0 {
		t.Fatalf("another owner's identical live draft: %s; want filed", out)
	}
}

// TestConcurrentIdenticalFilingsFileOne: several identical officer_post_draft
// calls at once file exactly one draft; every other is the duplicate refusal
// naming it (the filings serialize on the officer's advisory lock, inside the
// transaction that reads and inserts).
func TestConcurrentIdenticalFilingsFileOne(t *testing.T) {
	w := newOutboundWorld(t, true)
	const n = 6
	args := postArgs("example.forum", postTarget, dupBody+" At once.")
	outs := make([]string, n)
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], outs[i] = w.postDraft(args)
		}(i)
	}
	wg.Wait()
	var filed []int64
	for _, id := range ids {
		if id != 0 {
			filed = append(filed, id)
		}
	}
	if len(filed) != 1 {
		t.Fatalf("%d of %d identical concurrent filings were filed (%v); want exactly 1", len(filed), n, outs)
	}
	for i, id := range ids {
		if id == 0 {
			wantDuplicate(t, "concurrent filing "+strconv.Itoa(i), outs[i], filed[0], "status pending")
		}
	}
	if c := w.outboundCount(dupBody + " At once."); c != 1 {
		t.Fatalf("%d drafts carry the body; want 1", c)
	}
}
