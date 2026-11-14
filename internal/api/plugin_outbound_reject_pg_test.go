package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
)

// An OWNER'S rejection of an officer's outbound draft (decideDraftAs →
// outboundSettled): it goes back to the officer as a draft.refused item
// carrying the owner's reason, bounded like a plugin's refusal, and closes
// the item the draft was filed for. A reply draft's rejection goes nowhere.

// fileForItem enqueues a plugin.reply item, has the instance claim it, files
// a post for it (item_id) and has the plugin check it sendable: the item's
// and the draft's ids.
func (w *outboundWorld) fileForItem(subject, body string) (int64, int64) {
	w.t.Helper()
	item, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "plugin.reply", SubjectKey: subject}, time.Now())
	if err != nil {
		w.t.Fatal(err)
	}
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(item, 10)+`"}`); isToolErr(out) {
		w.t.Fatalf("claim: %s", out)
	}
	b, _ := json.Marshal(body)
	id, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"example.forum","target":{"kind":"post"},"body":` + string(b) +
		`,"item_id":"` + strconv.FormatInt(item, 10) + `"}`)
	if id == 0 {
		w.t.Fatalf("file: %s", out)
	}
	if rec := w.check(w.tok, id, true, ""); rec.Code != http.StatusOK {
		w.t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	return item, id
}

// rejectForm is the card's Reject, with reason (none when "").
func (w *outboundWorld) rejectForm(id int64, reason string) *httptest.ResponseRecorder {
	form := url.Values{}
	if reason != "" {
		form.Set("reason", reason)
	}
	return w.do("POST", "/plugins/drafts/"+strconv.FormatInt(id, 10)+"/reject", form.Encode(), w.operatorForm)
}

// refusedItemOf is draft id's draft.refused item: its id and payload, ok
// false when there is none.
func (w *outboundWorld) refusedItemOf(id int64) (int64, map[string]any, bool) {
	w.t.Helper()
	var item int64
	var raw []byte
	err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT id, payload FROM officer_items WHERE kind = 'draft.refused' AND subject_key = $1`,
		"draft:"+strconv.FormatInt(id, 10)).Scan(&item, &raw)
	if err != nil {
		return 0, nil, false
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		w.t.Fatal(err)
	}
	return item, p, true
}

func (w *outboundWorld) itemRow(id int64) (status, outcome, summary, claimedBy string) {
	w.t.Helper()
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT status, coalesce(outcome, ''), coalesce(summary, ''), coalesce(claimed_by, '')
		FROM officer_items WHERE id = $1`, id).Scan(&status, &outcome, &summary, &claimedBy); err != nil {
		w.t.Fatal(err)
	}
	return
}

// TestAnOwnerRejectionGoesBackToTheOfficerWithTheReason is the feature, from
// the card's Reject: the reason is stored and drawn on the card's history and
// audited; the officer gets a draft.refused item — status rejected, by the
// owner, detail exactly the reason, the draft's venue, target and item — and
// the item the draft was for is closed naming that follow-up.
func TestAnOwnerRejectionGoesBackToTheOfficerWithTheReason(t *testing.T) {
	w := newOutboundWorld(t, true)
	const reason = "Too salesy. Lead with the fix, then the link."
	item, id := w.fileForItem("x:reason", "A post that sells too hard.")
	if rec := w.rejectForm(id, reason); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	d := w.draft(id)
	if d.Status != plugins.DraftRejected || d.DecisionReason == nil || *d.DecisionReason != reason {
		t.Fatalf("the draft: %s reason %v; want rejected with the reason", d.Status, d.DecisionReason)
	}
	c, err := w.ps.DraftCard(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if card := draftCardHTML(c); !strings.Contains(card, `data-draft-reason>Your reason: Too salesy. Lead with the fix, then the link.</span>`) {
		t.Fatalf("the card does not show the reason: %s", card)
	}
	var audited string
	_ = w.f.pool.QueryRow(authz.System(w.ctx), `SELECT coalesce(detail->>'reason', '') FROM audit_log WHERE object = $1 AND action = 'plugin.draft.rejected'`,
		plugins.DraftObject(id)).Scan(&audited)
	if audited != reason {
		t.Fatalf("the audit row's reason: %q", audited)
	}
	refused, p, ok := w.refusedItemOf(id)
	if !ok {
		t.Fatal("no draft.refused item for the owner's rejection")
	}
	if p["status"] != "rejected" || p["by"] != "owner" || p["detail"] != reason || p["draft_id"] != float64(id) ||
		p["item_id"] != float64(item) || p["venue"] != "example.forum" {
		t.Fatalf("the draft.refused payload: %v", p)
	}
	st, outcome, summary, _ := w.itemRow(item)
	if st != officers.StatusDone || outcome != "no_action" || !strings.Contains(summary, "rejected by the owner") ||
		!strings.Contains(summary, "item "+strconv.FormatInt(refused, 10)) {
		t.Fatalf("the item the draft was for: %s %s %q; want closed naming item %d", st, outcome, summary, refused)
	}
}

// TestAnOwnerRejectionWithNoReasonSaysSo: no reason is the fixed sentence,
// and the MCP door's decision (decideDraftAs with an MCP source) and a
// withdrawn approval each queue the item too.
func TestAnOwnerRejectionWithNoReasonSaysSo(t *testing.T) {
	w := newOutboundWorld(t, true)
	_, id := w.fileForItem("x:none", "A post rejected with no reason.")
	if rec := w.rejectForm(id, ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	if d := w.draft(id); d.DecisionReason != nil {
		t.Fatalf("a rejection with no reason stored %q", *d.DecisionReason)
	}
	if _, p, ok := w.refusedItemOf(id); !ok || p["detail"] != OwnerRejectedNoReason || p["by"] != "owner" {
		t.Fatalf("the draft.refused item: %v %v; want detail %q", ok, p, OwnerRejectedNoReason)
	}
	// The MCP door's decision, with a reason.
	_, id2 := w.fileForItem("x:mcp", "A post rejected over MCP.")
	const body2 = "A post rejected over MCP."
	_, refused := w.srv.decideDraftAs(w.ctx, id2, plugins.DraftDecision{Reason: "not this week",
		Via:        plugins.DraftDecisionSource{Via: plugins.DraftViaMCP, Host: "tower", Session: "5e55a1d0-0000-4000-8000-000000000001"},
		BodySHA256: sumHex([]byte(body2))})
	if refused != nil {
		t.Fatalf("an MCP rejection: %s", refused.msg)
	}
	if _, p, ok := w.refusedItemOf(id2); !ok || p["detail"] != "not this week" {
		t.Fatalf("the MCP rejection's item: %v %v", ok, p)
	}
	// A withdrawn approval is the owner's rejection too.
	_, id3 := w.fileForItem("x:withdraw", "A post approved then withdrawn.")
	if rec := w.approve(id3, bindingForm("A post approved then withdrawn.")); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d", rec.Code)
	}
	if rec := w.rejectForm(id3, "changed my mind"); rec.Code != http.StatusSeeOther {
		t.Fatalf("withdraw: %d %s", rec.Code, rec.Body.String())
	}
	if _, p, ok := w.refusedItemOf(id3); !ok || p["detail"] != "changed my mind" {
		t.Fatalf("the withdrawal's item: %v %v", ok, p)
	}
}

// TestAnOwnerRejectionIsBounded: with maxOpenRefusedItems draft.refused items
// already live, another rejection queues none, and the item the draft was for
// still settles (TestAnOwnerRejectionPastTheBoundSettlesAndTellsTheOwner has
// what it says). CONTROL: below the bound one is queued.
func TestAnOwnerRejectionIsBounded(t *testing.T) {
	w := newOutboundWorld(t, true)
	for i := 0; i < maxOpenRefusedItems-1; i++ {
		if _, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "draft.refused",
			SubjectKey: "draft:prior-" + strconv.Itoa(i)}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	_, below := w.fileForItem("x:below", "The last one under the bound.")
	if rec := w.rejectForm(below, "no"); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d", rec.Code)
	}
	if _, _, ok := w.refusedItemOf(below); !ok {
		t.Fatal("control: below the bound no draft.refused item was queued")
	}
	item, at := w.fileForItem("x:at", "One past the bound.")
	if rec := w.rejectForm(at, "no again"); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d", rec.Code)
	}
	if _, _, ok := w.refusedItemOf(at); ok {
		t.Fatal("a draft.refused item was queued past the bound")
	}
	var live int
	_ = w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items WHERE kind = 'draft.refused' AND status IN ('open', 'claimed', 'waiting')`).Scan(&live)
	if live != maxOpenRefusedItems {
		t.Fatalf("%d live draft.refused items; want %d", live, maxOpenRefusedItems)
	}
	if st, outcome, _, _ := w.itemRow(item); st != officers.StatusDone || outcome != "no_action" {
		t.Fatalf("past the bound the item the draft was for is %s %s; want settled no_action, never left waiting", st, outcome)
	}
}

// TestAReplyDraftsRejectionGoesNowhere: the chief-style reply draft (a
// message delivered to an agent, answered with FileDraft) rejected WITH a
// reason stores the reason and queues nothing for any officer.
func TestAReplyDraftsRejectionGoesNowhere(t *testing.T) {
	w := newOutboundWorld(t, true)
	sess1 := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	_, p, err := w.ps.CreatePlugin(sess1, plugins.NewPlugin{Name: "example-late", Label: "Inbox", DataScope: "inbox",
		Capabilities: []plugins.Capability{plugins.CapMessages}})
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := w.ps.Submit(w.ctx, p.ID, plugins.MessageIn{ExternalID: "reply-1", Sender: "s@example.test", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := w.ps.BeginDelivery(w.ctx, m.ID, w.agent.ID); err != nil || !ok {
		t.Fatalf("begin delivery: %v %v", ok, err)
	}
	if ok, err := w.ps.RecordDelivery(w.ctx, m.ID, w.agent.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("record delivery: %v %v", ok, err)
	}
	d, _, err := w.ps.FileDraft(w.ctx, w.agent.ID, 1, w.agent.Name, m.ID, "thanks, will do")
	if err != nil {
		t.Fatal(err)
	}
	var before int
	_ = w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items`).Scan(&before)
	if rec := w.rejectForm(d.ID, "wrong tone"); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	got := w.draft(d.ID)
	if got.Status != plugins.DraftRejected || got.DecisionReason == nil || *got.DecisionReason != "wrong tone" {
		t.Fatalf("the reply draft: %s %v", got.Status, got.DecisionReason)
	}
	var after int
	_ = w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items`).Scan(&after)
	if after != before {
		t.Fatalf("officer items %d → %d after a reply draft's rejection; want unchanged", before, after)
	}
}

// TestARejectionReasonIsScreened: a control character, or one over the
// bound, is refused (400), and nothing moves: the draft stays pending and no
// item is queued. An approval carrying a reason is refused too. CONTROL: the
// same reason without the control character goes through.
func TestARejectionReasonIsScreened(t *testing.T) {
	w := newOutboundWorld(t, true)
	_, id := w.fileForItem("x:screen", "A post whose rejection is screened.")
	for name, bad := range map[string]string{
		"a control character": "no\x1bthanks",
		"an invisible one":    "no​thanks",
		"over the bound":      strings.Repeat("a", plugins.MaxDecisionReasonRunes+1),
	} {
		if rec := w.rejectForm(id, bad); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reason") {
			t.Errorf("%s: %d; want 400 naming the reason", name, rec.Code)
		}
	}
	if d := w.draft(id); d.Status != plugins.DraftPending {
		t.Fatalf("after refused rejections the draft is %s", d.Status)
	}
	if _, _, ok := w.refusedItemOf(id); ok {
		t.Fatal("a refused rejection queued an item")
	}
	if _, err := w.ps.DecideDraftFrom(w.ctx, id, plugins.DraftDecision{Approve: true, Reason: "fine",
		BodySHA256: sumHex([]byte("A post whose rejection is screened.")), MediaSHA256: []string{}}); err == nil {
		t.Fatal("an approval with a reason was allowed")
	}
	if rec := w.rejectForm(id, "no\nthanks, "+strings.Repeat("a", plugins.MaxDecisionReasonRunes-20)); rec.Code != http.StatusSeeOther {
		t.Fatalf("control, a plain reason at the bound: %d %s", rec.Code, rec.Body.String())
	}
}
