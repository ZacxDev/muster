package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_ask_person: an officer instance asks a named HQ member a question
// about an item it holds (HQ's items/{item}/asks route), citing images it
// uploaded from its own pod (`ccd ask-image`, officer_ask_upload_test.go).
//
// muster passes the ask through as given: HQ checks every limit, and its
// refusal comes back with its status and code.
// ---------------------------------------------------------------------------

// askPNG is the smallest byte string http.DetectContentType names image/png,
// padded.
var askPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 40)...)

// askPNGOf is a PNG fixture of exactly n bytes.
func askPNGOf(n int) []byte {
	return append(append([]byte(nil), askPNG[:8]...), bytes.Repeat([]byte{4}, n-8)...)
}

// askRig is an officer rig with officer B's instance 1 holding a claimed
// item, and a member "design-lead" in HQ.
func askRig(t *testing.T) (*officerRig, agents.Agent, string) {
	t.Helper()
	r := newOfficerRig(t)
	inst := r.addInstance(r.b, 1, 901)
	item := r.hq.AddItem(offB, "triage.look", "svc:ask")
	r.hq.AddMember("design-lead")
	if out := r.s.dispatchAgentTool(context.Background(), inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	r.hq.ResetRequests()
	return r, inst, item
}

func askArgs(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAnAskSendsHQTheContractBodyAndEndsTheLease: the ask is created with
// EXACTLY the contract's body — instance, to, question, options, the image ids
// verbatim and in order (as images), context_url and deadline_seconds — with
// the officer's own token, and nothing else is sent (no upload: the images are
// already in HQ). The result is HQ's answer plus the note that the lease has
// ENDED; HQ's item is waiting with no holder. A second ask on the waiting item
// is allowed and sends only what was given.
func TestAnAskSendsHQTheContractBodyAndEndsTheLease(t *testing.T) {
	r, srv, _, inst := uploadRig(t)
	ctx := context.Background()
	item := r.hq.AddItem(offB, "triage.look", "svc:ask")
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatal(out)
	}
	var ids []any
	for _, n := range []int{100, 2_000} {
		code, up := postAskImage(t, srv.URL, "?filename=shot.png", askTokB, askPNGOf(n))
		if code != http.StatusCreated {
			t.Fatalf("upload: %d %v", code, up)
		}
		ids = append(ids, up["id"])
	}
	// Cited in the reverse of upload order, so "in order" is the instance's.
	ids[0], ids[1] = ids[1], ids[0]
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, askArgs(t, map[string]any{
		"item_id": item, "to": "design-lead", "question": "Which header reads better?",
		"options":     []string{"the left one", "the right one"},
		"image_ids":   ids,
		"context_url": "https://pr.example/42", "deadline_seconds": 14400,
	}))
	reqs := r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d HQ requests, want only the create: %+v\nresult %s", len(reqs), reqs, out)
	}
	c := reqs[0]
	if c.Method != http.MethodPost || c.Path != "/api/v1/officers/"+offB+"/items/"+item+"/asks" || c.Auth != "Bearer "+offBTok {
		t.Fatalf("create: %s %s (auth %q)", c.Method, c.Path, c.Auth)
	}
	want := map[string]any{
		"instance": inst.Name, "to": "design-lead", "question": "Which header reads better?",
		"options": []any{"the left one", "the right one"}, "images": ids,
		"context_url": "https://pr.example/42", "deadline_seconds": 14400,
	}
	if got, w := normJSON(t, c.Body), normJSON(t, want); got != w {
		t.Fatalf("create body:\n%s\nwant\n%s", got, w)
	}
	if isToolErr(out) {
		t.Fatalf("ask: %s", out)
	}
	var res struct {
		Ask  map[string]any `json:"ask"`
		Item map[string]any `json:"item"`
		Note string         `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %s", out)
	}
	if res.Ask["id"] != "ask-1" || res.Item["status"] != "waiting" {
		t.Errorf("result does not carry HQ's ask and item: %s", out)
	}
	// HQ nests the delivery in the ask; it is passed through there, untouched.
	if d, _ := res.Ask["delivery"].(map[string]any); d["notice"] != "both" || d["dm"] != "queued" {
		t.Errorf("the ask's delivery is not HQ's: %s", out)
	}
	for _, s := range []string{"ENDED", "do not heartbeat, complete or release", "re-opens", "escalates", "same item_id"} {
		if !strings.Contains(res.Note, s) {
			t.Errorf("the note does not say %q: %s", s, res.Note)
		}
	}
	if it := r.hq.Item(item); it.Status != "waiting" || it.ClaimedBy != "" {
		t.Fatalf("HQ's item after the ask: %+v, want waiting with no holder", it)
	}

	// A second ask on the now-waiting item: only what was given is sent.
	r.hq.ResetRequests()
	out = r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, `{"item_id":"`+item+`","to":"design-lead","question":"And the footer?"}`)
	if isToolErr(out) {
		t.Fatalf("a second ask on a waiting item: %s", out)
	}
	reqs = r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("second ask: %d HQ requests, want 1", len(reqs))
	}
	if got, w := normJSON(t, reqs[0].Body), normJSON(t, map[string]any{"instance": inst.Name, "to": "design-lead", "question": "And the footer?"}); got != w {
		t.Fatalf("second create body:\n%s\nwant\n%s", got, w)
	}
}

// askRefusal decodes a refused ask's tool result.
func askRefusal(t *testing.T, out string) (msg string, status float64, code string) {
	t.Helper()
	var r struct {
		Error   string `json:"error"`
		Refused struct {
			Status float64        `json:"status"`
			Code   string         `json:"code"`
			HQ     map[string]any `json:"hq"`
		} `json:"refused"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || !isToolErr(out) {
		t.Fatalf("not a tool error: %s", out)
	}
	return r.Error, r.Refused.Status, r.Refused.Code
}

// TestAnAsksRefusalsAreHQsWithTheirCode: muster checks none of HQ's limits,
// so what HQ refuses comes back with HQ's status and code, in the error text
// and in refused. A deadline outside HQ's range is passed through verbatim and
// HQ's 400 invalid is the answer, as for its other input limits (image count,
// option count and length); so are an unknown recipient (422), an item this
// instance never claimed (409 lease_lost), an unknown item (404, HQ's own
// message), and forced 409 too_many_open, 422 recipient_muted, 422
// recipient_unreachable (an ask that would reach nobody) and 429
// rate_limited. None of them changes HQ's item or stores an ask.
func TestAnAsksRefusalsAreHQsWithTheirCode(t *testing.T) {
	r, inst, item := askRig(t)
	ctx := context.Background()
	ask := func(item, to string, extra map[string]any) string {
		m := map[string]any{"item_id": item, "to": to, "question": "q"}
		for k, v := range extra {
			m[k] = v
		}
		return r.s.dispatchAgentTool(ctx, inst, officers.ToolAskPerson, askArgs(t, m))
	}

	r.hq.ResetRequests()
	msg, status, code := askRefusal(t, ask(item, "design-lead", map[string]any{"deadline_seconds": 60}))
	if status != 400 || code != "invalid" || !strings.Contains(msg, "400 invalid") || !strings.Contains(msg, "deadline_seconds must be 300-259200") {
		t.Errorf("a deadline HQ refuses: %q %v %q", msg, status, code)
	}
	if reqs := r.hq.Requests(); len(reqs) != 1 || reqs[0].Body["deadline_seconds"] != float64(60) {
		t.Errorf("the deadline was not passed through verbatim: %+v", reqs)
	}
	// HQ's other input limits are 400 invalid too, with HQ's own message.
	for _, tc := range []struct {
		extra map[string]any
		msg   string
	}{
		{map[string]any{"image_ids": []string{"a", "b", "c", "d", "e"}}, "at most 4 images"},
		{map[string]any{"options": []string{"1", "2", "3", "4", "5", "6", "7"}}, "at most 6 options"},
		{map[string]any{"options": []string{strings.Repeat("x", 41)}}, "options[0] must be 1-40 characters"},
		{map[string]any{"deadline_seconds": 259201}, "deadline_seconds must be 300-259200"},
	} {
		if msg, status, code := askRefusal(t, ask(item, "design-lead", tc.extra)); status != 400 || code != "invalid" || !strings.Contains(msg, tc.msg) {
			t.Errorf("HQ's limit %q: %q %v %q", tc.msg, msg, status, code)
		}
	}

	out := ask("999", "design-lead", nil)
	if _, status, _ := askRefusal(t, out); status != 404 || !strings.Contains(out, "no such item") {
		t.Errorf("an unknown item: %s, want HQ's 404 with its own message", out)
	}
	if msg, status, code := askRefusal(t, ask(item, "nobody-here", nil)); status != 422 || code != "unknown_recipient" || !strings.Contains(msg, "422 unknown_recipient") || !strings.Contains(msg, "no HQ member nobody-here") {
		t.Errorf("an unknown recipient: %q %v %q", msg, status, code)
	}
	other := r.hq.AddItem(offB, "triage.look", "svc:other")
	if msg, status, code := askRefusal(t, ask(other, "design-lead", nil)); status != 409 || code != "lease_lost" || !strings.Contains(msg, "409 lease_lost") {
		t.Errorf("an unclaimed item: %q %v %q", msg, status, code)
	}
	for _, tc := range []struct {
		status int
		code   string
	}{{409, "too_many_open"}, {422, "recipient_muted"}, {422, "recipient_unreachable"}, {429, "rate_limited"}} {
		r.hq.RefuseAsks(tc.status, tc.code, "refused by the fake: "+tc.code)
		msg, status, code := askRefusal(t, ask(item, "design-lead", nil))
		if int(status) != tc.status || code != tc.code || !strings.Contains(msg, "refused by the fake: "+tc.code) {
			t.Errorf("HQ %d %s: %q %v %q", tc.status, tc.code, msg, status, code)
		}
	}
	r.hq.RefuseAsks(0, "", "")
	if it := r.hq.Item(item); it.Status != "claimed" || it.ClaimedBy != inst.Name {
		t.Fatalf("a refused ask changed HQ's item: %+v", it)
	}
	if n := len(r.hq.Asks()); n != 0 {
		t.Fatalf("HQ stored %d asks, want none (every one was refused)", n)
	}
}
