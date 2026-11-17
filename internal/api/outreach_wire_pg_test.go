package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The outreach draft on the plugin's wire: what GET /plugin/v1/drafts/checks
// says about an outbound draft must be what the claim says about it
// (plugins.OutboundFields, THE one source), because a plugin that checks an
// outreach draft checks it on the same fields it later acts on — a follow's
// because_ref missing from the check listing made every follow, like and dm
// unsendable. And the refs an officer files may arrive as JSON numbers: an X
// post id is all digits, and an agent sends it as one.

// checksEntry is draft id's entry in this plugin's GET /plugin/v1/drafts/checks,
// nil when it is not listed.
func (w *outboundWorld) checksEntry(id int64) map[string]any {
	w.t.Helper()
	rec := w.do("GET", "/plugin/v1/drafts/checks", "", bearerTok(w.tok))
	if rec.Code != http.StatusOK {
		w.t.Fatalf("GET /plugin/v1/drafts/checks: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Drafts []map[string]any `json:"drafts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		w.t.Fatalf("the checks listing: %v: %s", err, rec.Body)
	}
	for _, d := range out.Drafts {
		if d["draft_id"] == float64(id) {
			return d
		}
	}
	return nil
}

// TestAnOutreachDraftAwaitingCheckListsItsBecauseRef: a follow, a like and a
// dm awaiting the plugin's check each list the because_ref they were filed
// with; an ordinary reply, which cites none, lists no because_ref key at all.
// Every because_ref is distinct from every target ref and handle, so a listing
// that put the wrong field there could not pass.
func TestAnOutreachDraftAwaitingCheckListsItsBecauseRef(t *testing.T) {
	w := newOutboundWorld(t, true)
	w.engagement("fen", "reply", "eng-fen-1", "how does it retry?")
	w.engagement("lia", "reply", "eng-lia-liked", "nice")
	w.engagement("lia", "reply", "eng-lia-why", "does it batch?")
	w.engagement("dov", "reply", "eng-dov-1", "is there a free tier?")
	file := func(what string, args map[string]any) int64 {
		t.Helper()
		id, out := w.outreach(args)
		if id == 0 {
			t.Fatalf("file the %s: %s", what, out)
		}
		return id
	}
	follow := file("follow", map[string]any{"target": map[string]any{"kind": "follow", "handle": "fen"}, "because_ref": "eng-fen-1", "reason": "asked a good question"})
	like := file("like", map[string]any{"target": map[string]any{"kind": "like", "ref": "eng-lia-liked"}, "because_ref": "eng-lia-why", "reason": "kind words"})
	dm := file("dm", map[string]any{"target": map[string]any{"kind": "dm", "handle": "dov"}, "because_ref": "eng-dov-1", "body": "Yes, there is a free tier."})
	reply := file("reply", map[string]any{"target": map[string]any{"kind": "reply", "ref": "t1_unrecorded"}, "body": "Thanks for trying it."})

	for _, c := range []struct {
		what    string
		id      int64
		because string
	}{{"follow", follow, "eng-fen-1"}, {"like", like, "eng-lia-why"}, {"dm", dm, "eng-dov-1"}} {
		e := w.checksEntry(c.id)
		if e == nil {
			t.Fatalf("the %s (draft %d) is not listed awaiting its check", c.what, c.id)
		}
		if e["kind"] != "outbound" || e["because_ref"] != c.because {
			t.Errorf("the %s awaiting its check: want kind outbound and because_ref %q, got %v", c.what, c.because, e)
		}
	}
	e := w.checksEntry(reply)
	if e == nil {
		t.Fatalf("the reply (draft %d) is not listed awaiting its check", reply)
	}
	if _, has := e["because_ref"]; has {
		t.Errorf("a reply that cites nothing lists a because_ref: %v", e)
	}
}

// TestTheCheckListingAndTheClaimCarryTheSameOutboundFields pins the
// RELATIONSHIP between the two listings of one outbound draft, not either
// alone: for a dm (which cites a because_ref) and an ordinary reply (which
// does not), the venue, target and because_ref the check listed are exactly
// what the claim hands out; and the two wire forms differ by exactly a ledger
// of keys — the check's created_at against the claim's approved_at and
// body_sha256 — so a field added to one and not the other reddens here.
func TestTheCheckListingAndTheClaimCarryTheSameOutboundFields(t *testing.T) {
	w := newOutboundWorld(t, true)
	w.engagement("gil", "reply", "eng-gil-1", "does it run offline?")
	const dmBody, replyBody = "It does, once the model is pulled.", "Thanks: fixed in the next release."
	for _, c := range []struct {
		what string
		args map[string]any
		body string
	}{
		{"dm", map[string]any{"target": map[string]any{"kind": "dm", "handle": "gil"}, "because_ref": "eng-gil-1", "body": dmBody}, dmBody},
		{"reply", map[string]any{"target": map[string]any{"kind": "reply", "ref": "t1_unrecorded"}, "body": replyBody}, replyBody},
	} {
		id, out := w.outreach(c.args)
		if id == 0 {
			t.Fatalf("file the %s: %s", c.what, out)
		}
		checked := w.checksEntry(id)
		if checked == nil {
			t.Fatalf("the %s is not listed awaiting its check", c.what)
		}
		if rec := w.check(w.tok, id, true, ""); rec.Code != http.StatusOK {
			t.Fatalf("check the %s: %d %s", c.what, rec.Code, rec.Body)
		}
		if rec := w.approve(id, bindingForm(c.body)); rec.Code != http.StatusSeeOther {
			t.Fatalf("approve the %s: %d %s", c.what, rec.Code, rec.Body)
		}
		rec := w.claim()
		var claimed map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil || claimed["draft_id"] != float64(id) {
			t.Fatalf("claim the %s (draft %d): %d %s", c.what, id, rec.Code, rec.Body)
		}
		for _, k := range []string{"kind", "venue", "target", "because_ref", "body", "message_external_id"} {
			if !reflect.DeepEqual(checked[k], claimed[k]) {
				t.Errorf("the %s's %q: the check listed %v, the claim hands out %v", c.what, k, checked[k], claimed[k])
			}
		}
		if c.what == "dm" && claimed["because_ref"] != "eng-gil-1" {
			t.Errorf("the dm's claim: because_ref %v", claimed["because_ref"])
		}
		onlyChecked, onlyClaimed := keyDiff(checked, claimed)
		if !slices.Equal(onlyChecked, []string{"created_at"}) || !slices.Equal(onlyClaimed, []string{"approved_at", "body_sha256"}) {
			t.Errorf("the %s: the check alone carries %v (want [created_at]); the claim alone %v (want [approved_at body_sha256])",
				c.what, onlyChecked, onlyClaimed)
		}
		if rec := w.result(id, `{"ok":true,"external_ref":"sent-`+strconv.FormatInt(id, 10)+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("result: %d %s", rec.Code, rec.Body)
		}
	}
}

// keyDiff is the sorted keys only a has, and only b has.
func keyDiff(a, b map[string]any) (onlyA, onlyB []string) {
	for k := range a {
		if _, ok := b[k]; !ok {
			onlyA = append(onlyA, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			onlyB = append(onlyB, k)
		}
	}
	slices.Sort(onlyA)
	slices.Sort(onlyB)
	return onlyA, onlyB
}

// toolError is an officer tool answer's "error" sentence ("" for none).
func toolError(out string) string {
	var m struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &m)
	return m.Error
}

// TestAnOutreachRefIsAStringOrASmallWholeNumber: officer_post_draft takes
// because_ref and target.ref as a JSON string (kept exactly, at any length) or
// a JSON number of digits only up to 2^53-1. An X post id is above that, and an
// officer's runtime (Node) parses a JSON number as a float64 before muster sees
// it: 2122495152350453825 arrives as 2122495152350453800, which is nobody's
// engagement, and the officer was told "not a recorded engagement" with no
// hint why. So a number above 2^53-1 is refused BY THE REF DECODER, saying to
// send the id as a string, and nothing is filed. A fraction, a sign or an
// exponent is refused too.
func TestAnOutreachRefIsAStringOrASmallWholeNumber(t *testing.T) {
	w := newOutboundWorld(t, true)
	w.engagement("quanta", "reply", "2122495152350453825", "how does it scale?")
	w.engagement("nia", "reply", "2122495152350453901", "great thread")
	w.engagement("nia", "reply", "2122495152350453902", "and the cost?")
	w.engagement("small", "reply", "9007199254740991", "a small id")

	// The string form of a 19-digit id: filed, stored with its exact digits.
	id, out := w.outreach(map[string]any{"target": map[string]any{"kind": "follow", "handle": "quanta"},
		"because_ref": "2122495152350453825", "reason": "asked a good question"})
	if id == 0 {
		t.Fatalf("a follow whose because_ref is the string of a 19-digit id was refused: %s", out)
	}
	if d := w.draft(id); d.BecauseRef == nil || *d.BecauseRef != "2122495152350453825" {
		t.Fatalf("the stored because_ref: %v", d.BecauseRef)
	}
	if e := w.checksEntry(id); e == nil || e["because_ref"] != "2122495152350453825" {
		t.Fatalf("the check listing's because_ref: %v", e)
	}
	id, out = w.outreach(map[string]any{"target": map[string]any{"kind": "like", "ref": "2122495152350453901"},
		"because_ref": "2122495152350453902", "reason": "kind words"})
	if id == 0 {
		t.Fatalf("a like whose refs are strings was refused: %s", out)
	}
	if d := w.draft(id); d.TargetRef == nil || *d.TargetRef != "2122495152350453901" || d.BecauseRef == nil || *d.BecauseRef != "2122495152350453902" {
		t.Fatalf("the stored like: target_ref %v because_ref %v", d.TargetRef, d.BecauseRef)
	}

	// 2^53-1 as a JSON number: the largest one taken, as its digits.
	id, out = w.outreach(map[string]any{"target": map[string]any{"kind": "follow", "handle": "small"},
		"because_ref": json.Number("9007199254740991"), "reason": "r"})
	if id == 0 {
		t.Fatalf("a because_ref of 2^53-1 as a JSON number was refused: %s", out)
	}
	if d := w.draft(id); d.BecauseRef == nil || *d.BecauseRef != "9007199254740991" {
		t.Fatalf("the stored because_ref: %v", d.BecauseRef)
	}

	// Above 2^53-1: refused with the fix spelled out. Each one IS a recorded
	// engagement's ref, so no later guard (no_qualifying_engagement) could
	// refuse it in the decoder's place: the answer must be the decoder's own.
	for i, n := range []string{"9007199254740992", "2122495152350453800"} {
		b, tg := "big-b"+strconv.Itoa(i), "big-t"+strconv.Itoa(i)
		w.engagement(b, "reply", n, "a question")
		w.engagement(tg, "reply", n+"1", "a question")
		w.engagement(tg, "reply", tg+"-why", "another")
		id, out := w.outreach(map[string]any{"target": map[string]any{"kind": "follow", "handle": b},
			"because_ref": json.Number(n), "reason": "r"})
		want := "because_ref " + n + ` is a JSON number above 2^53-1, so your client may have rounded it (JavaScript does): ` +
			`send the id as a JSON string, e.g. "because_ref": "<id>"`
		if id != 0 || toolError(out) != want {
			t.Errorf("because_ref %s: want refused with\n%q\ngot draft %d: %s", n, want, id, out)
		}
		id, out = w.outreach(map[string]any{"target": map[string]any{"kind": "like", "ref": json.Number(n + "1")},
			"because_ref": tg + "-why", "reason": "r"})
		if id != 0 || !strings.Contains(toolError(out), "target.ref "+n+"1 is a JSON number above 2^53-1") ||
			!strings.Contains(toolError(out), `send the id as a JSON string, e.g. "target": {"ref": "<id>"}`) {
			t.Errorf("target.ref %s1: want refused by the ref decoder, got draft %d: %s", n, id, out)
		}
	}

	// The malformed numbers.
	for i, bad := range []struct{ because, target string }{
		{"2.122495152350453977e18", "2.122495152350453978e18"},
		{"2122495152350453977.0", "2122495152350453978.0"},
		{"-2122495152350453977", "-2122495152350453978"},
		{"1e3", "2e3"},
	} {
		b, tg := "bad-b"+strconv.Itoa(i), "bad-t"+strconv.Itoa(i)
		w.engagement(b, "reply", bad.because, "a question")
		w.engagement(tg, "reply", bad.target, "a question")
		w.engagement(tg, "reply", tg+"-why", "another")
		id, out := w.outreach(map[string]any{"target": map[string]any{"kind": "follow", "handle": b},
			"because_ref": json.Number(bad.because), "reason": "r"})
		if id != 0 || !strings.Contains(out, "digits only") {
			t.Errorf("because_ref %s: want refused by the ref decoder, got draft %d: %s", bad.because, id, out)
		}
		id, out = w.outreach(map[string]any{"target": map[string]any{"kind": "like", "ref": json.Number(bad.target)},
			"because_ref": tg + "-why", "reason": "r"})
		if id != 0 || !strings.Contains(out, "digits only") {
			t.Errorf("target.ref %s: want refused by the ref decoder, got draft %d: %s", bad.target, id, out)
		}
	}
}
