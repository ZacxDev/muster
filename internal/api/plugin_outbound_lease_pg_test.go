package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A slow post: the plugin's send can outlast the 2-minute lease (a post made
// through a browser). Two things keep that from leaving a false trail:
//   - a LATE ok:true on an unconfirmed outbound draft completes the item the
//     unconfirmed end escalated, and withdraws the owner's "may have been
//     posted" ask (builtin.DraftPosted);
//   - the plugin can RENEW its lease (POST /plugin/v1/drafts/{id}/renew),
//     never past 10 minutes after the claim.
// Expected strings and the 10-minute bound are written out here.

// unconfirmedForItem files a draft for an item, approves it, has the plugin
// claim it and lets the lease lapse; the sweep makes it unconfirmed and
// escalates the item, and the officer Attention sweep raises the owner's
// ask. The item's and the draft's ids.
func (w *outboundWorld) unconfirmedForItem(subject, body string) (int64, int64) {
	w.t.Helper()
	item, id := w.fileForItem(subject, body)
	if rec := w.approve(id, bindingForm(body)); rec.Code != http.StatusSeeOther {
		w.t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.claim(); rec.Code != http.StatusOK {
		w.t.Fatalf("the plugin's claim: %d %s", rec.Code, rec.Body.String())
	}
	w.draftSQL(id, `lease_until = now() - interval '1 minute'`)
	w.sweepByClaim()
	if st, _, _, _ := w.itemRow(item); st != officers.StatusEscalated {
		w.t.Fatalf("control: the unconfirmed draft's item is %s; want escalated", st)
	}
	w.srv.SweepOfficerAttention(w.ctx)
	return item, id
}

// escalationAsks is the owner's escalation asks about item: their statuses.
func (w *outboundWorld) escalationAsks(item int64) []string {
	w.t.Helper()
	rows, err := w.f.pool.Query(authz.System(w.ctx), `SELECT status FROM attention_asks WHERE owner_id = 1 AND asker_label = $1
		AND question LIKE $2 ORDER BY id`, "officer "+builtinOfficerID, "%item "+strconv.FormatInt(item, 10)+" %")
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (w *outboundWorld) result(id int64, body string) *httptest.ResponseRecorder {
	return w.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(id, 10)+"/result", body, bearerTok(w.tok))
}

// TestALateOKCompletesTheUnconfirmedDraftsItemAndWithdrawsTheEscalation: the
// post went out after all. Its late ok:true completes the escalated item
// done with the post's reference, withdraws the owner's open escalation ask,
// and the next Attention sweep neither re-raises nor re-opens anything.
func TestALateOKCompletesTheUnconfirmedDraftsItemAndWithdrawsTheEscalation(t *testing.T) {
	w := newOutboundWorld(t, true)
	item, id := w.unconfirmedForItem("x:late-ok", "A slow post that did go out.")
	if got := w.escalationAsks(item); len(got) != 1 || got[0] != "open" {
		t.Fatalf("control: the owner's escalation asks before the result: %v; want one open", got)
	}
	if rec := w.result(id, `{"ok":true,"external_ref":"post-late-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("the late result: %d %s", rec.Code, rec.Body.String())
	}
	if d := w.draft(id); d.Status != plugins.DraftSent {
		t.Fatalf("the draft: %s; want sent", d.Status)
	}
	st, outcome, summary, _ := w.itemRow(item)
	if want := "posted post-late-1 (draft " + strconv.FormatInt(id, 10) + ")"; st != officers.StatusDone || outcome != "done" || summary != want {
		t.Fatalf("the item after the late ok: %s %s %q; want done %q", st, outcome, summary, want)
	}
	if got := w.escalationAsks(item); len(got) != 1 || got[0] != "cancelled" {
		t.Fatalf("the owner's escalation asks after the late ok: %v; want the one withdrawn", got)
	}
	w.srv.SweepOfficerAttention(w.ctx)
	if got := w.escalationAsks(item); len(got) != 1 {
		t.Fatalf("the next sweep raised another escalation ask: %v", got)
	}
	if st, _, _, _ := w.itemRow(item); st != officers.StatusDone {
		t.Fatalf("the next sweep moved the item to %s", st)
	}
}

// TestALateOKLeavesAnItemANewerDraftWasFiledFor: once a newer draft was filed
// for the item (the owner answered the escalation and the officer redrafted),
// the old draft's late ok:true does not settle it.
func TestALateOKLeavesAnItemANewerDraftWasFiledFor(t *testing.T) {
	w := newOutboundWorld(t, true)
	item, id := w.unconfirmedForItem("x:late-newer", "An unconfirmed post.")
	// A newer draft for the same item (as after the owner sent it back and
	// the officer filed again); the item escalated again since.
	sysExec := func(q string) {
		t.Helper()
		if _, err := w.f.pool.Exec(authz.System(w.ctx), q); err != nil {
			t.Fatal(err)
		}
	}
	sysExec(`UPDATE officer_items SET status = 'claimed', outcome = NULL, claimed_by = '` + w.agent.Name + `', lease_expires_at = now() + interval '1 hour' WHERE id = ` +
		strconv.FormatInt(item, 10))
	b, _ := json.Marshal("A newer post.")
	if newer, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"example.forum","target":{"kind":"post"},"body":` + string(b) +
		`,"item_id":"` + strconv.FormatInt(item, 10) + `"}`); newer == 0 {
		t.Fatalf("file the newer draft: %s", out)
	}
	sysExec(`UPDATE officer_items SET status = 'escalated', outcome = 'escalated' WHERE id = ` + strconv.FormatInt(item, 10))
	if rec := w.result(id, `{"ok":true,"external_ref":"post-late-2"}`); rec.Code != http.StatusOK {
		t.Fatalf("the late result: %d %s", rec.Code, rec.Body.String())
	}
	if st, _, _, _ := w.itemRow(item); st != officers.StatusEscalated {
		t.Fatalf("the old draft's late ok moved the item to %s; want left escalated (a newer draft was filed for it)", st)
	}
}

// TestALateOKLeavesAnItemEscalatedAgainForSomethingElse: the owner sent the
// unconfirmed draft's item back (an asks entry, after the draft's claim) and
// it was escalated again since; the draft's late ok:true does not close that
// newer escalation or withdraw its asks.
func TestALateOKLeavesAnItemEscalatedAgainForSomethingElse(t *testing.T) {
	w := newOutboundWorld(t, true)
	item, id := w.unconfirmedForItem("x:late-again", "An unconfirmed post.")
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `UPDATE officer_items SET asks = asks || jsonb_build_array(jsonb_build_object(
		'ask_id', 1, 'question', 'escalation', 'asked_by', 'user 1', 'asked_at', now(), 'status', 'answered', 'answer', 'look again'))
		WHERE id = $1`, item); err != nil {
		t.Fatal(err)
	}
	if rec := w.result(id, `{"ok":true,"external_ref":"post-late-3"}`); rec.Code != http.StatusOK {
		t.Fatalf("the late result: %d %s", rec.Code, rec.Body.String())
	}
	if st, _, _, _ := w.itemRow(item); st != officers.StatusEscalated {
		t.Fatalf("the late ok moved an item escalated again since to %s; want left escalated", st)
	}
	if got := w.escalationAsks(item); len(got) != 1 || got[0] != "open" {
		t.Fatalf("the escalation asks: %v; want the open one left alone", got)
	}
}

type renewed struct {
	DraftID    int64     `json:"draft_id"`
	LeaseUntil time.Time `json:"lease_until"`
	LeaseMax   time.Time `json:"lease_max"`
	Code       string    `json:"code"`
}

func (w *outboundWorld) renew(tok string, id int64) (int, renewed) {
	w.t.Helper()
	rec := w.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(id, 10)+"/renew", "", bearerTok(tok))
	var r renewed
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r
}

// claimedDraft files, approves and has the plugin claim a post: its id.
func (w *outboundWorld) claimedDraft(subject, body string) int64 {
	w.t.Helper()
	_, id := w.fileForItem(subject, body)
	if rec := w.approve(id, bindingForm(body)); rec.Code != http.StatusSeeOther {
		w.t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.claim(); rec.Code != http.StatusOK {
		w.t.Fatalf("the plugin's claim: %d %s", rec.Code, rec.Body.String())
	}
	return id
}

// TestARenewExtendsTheLeaseWithinItsBound: a renewal moves lease_until to two
// minutes from now, and keeps the draft from going unconfirmed under a slow
// send; near the bound it stops at exactly 10 minutes after the claim, and
// once there the next renewal is refused lease_at_max.
func TestARenewExtendsTheLeaseWithinItsBound(t *testing.T) {
	w := newOutboundWorld(t, true)
	id := w.claimedDraft("x:renew", "A post that takes a while.")
	// 90 s into the 2-minute lease: 30 s left.
	w.draftSQL(id, `claimed_at = now() - interval '90 seconds', lease_until = now() + interval '30 seconds'`)
	code, r := w.renew(w.tok, id)
	if code != http.StatusOK || r.DraftID != id || time.Until(r.LeaseUntil) < 110*time.Second || time.Until(r.LeaseUntil) > 125*time.Second {
		t.Fatalf("a renewal: %d %+v; want 200 with lease_until two minutes from now", code, r)
	}
	claimed := *w.draft(id).ClaimedAt
	if r.LeaseMax.Sub(claimed.Add(10*time.Minute)).Abs() > time.Millisecond {
		t.Fatalf("lease_max %s; want the claim (%s) plus 10 minutes", r.LeaseMax, claimed)
	}
	// The renewed lease outlives the original: the sweep leaves it approved.
	w.draftSQL(id, `claimed_at = claimed_at - interval '1 minute'`)
	w.sweepByClaim()
	if d := w.draft(id); d.Status != plugins.DraftApproved {
		t.Fatalf("under a renewed lease the draft became %s; want still approved", d.Status)
	}
	// 9 minutes in: the next renewal stops at the bound, not 2 minutes on.
	w.draftSQL(id, `claimed_at = now() - interval '9 minutes', lease_until = now() + interval '30 seconds'`)
	code, r = w.renew(w.tok, id)
	if left := time.Until(r.LeaseUntil); code != http.StatusOK || left < 50*time.Second || left > 65*time.Second {
		t.Fatalf("a renewal 9 minutes in: %d %+v (%s left); want 200 capped at the claim plus 10 minutes", code, r, left)
	}
	if code, r = w.renew(w.tok, id); code != http.StatusConflict || r.Code != "lease_at_max" {
		t.Fatalf("a renewal at the bound: %d %+v; want 409 lease_at_max", code, r)
	}
	if rec := w.result(id, `{"ok":true,"external_ref":"post-slow-1"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sent"`) {
		t.Fatalf("the result under the renewed lease: %d %s; want sent", rec.Code, rec.Body.String())
	}
}

// TestARenewIsRefusedToANonHolderALapsedLeaseAndAnUnclaimedDraft: another
// plugin's renewal is 404; a lease that already lapsed is 409 lease_lapsed
// and is not revived (the sweep still makes the draft unconfirmed); a draft
// never claimed is 409 not_claimed.
func TestARenewIsRefusedToANonHolderALapsedLeaseAndAnUnclaimedDraft(t *testing.T) {
	w := newOutboundWorld(t, true)
	id := w.claimedDraft("x:renew-refused", "A post claimed by the bound plugin.")
	if code, _ := w.renew(w.outTok, id); code != http.StatusNotFound {
		t.Fatalf("another owner's plugin's renewal: %d; want 404", code)
	}
	// A second plugin of the SAME owner: row-level security lets it see the
	// draft, so only the holder check refuses it.
	sess1 := authz.WithPrincipal(w.ctx, authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	sibling, _, err := w.ps.CreatePlugin(sess1, plugins.NewPlugin{Name: "example-mint", Label: "Sibling", DataScope: "sibling",
		Capabilities: []plugins.Capability{plugins.CapMessages}})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := w.renew(sibling, id); code != http.StatusNotFound {
		t.Fatalf("another plugin's renewal: %d; want 404", code)
	}
	w.draftSQL(id, `lease_until = now() - interval '1 second'`)
	if code, r := w.renew(w.tok, id); code != http.StatusConflict || r.Code != "lease_lapsed" {
		t.Fatalf("a renewal of a lapsed lease: %d %+v; want 409 lease_lapsed", code, r)
	}
	w.sweepByClaim()
	if d := w.draft(id); d.Status != plugins.DraftUnconfirmed {
		t.Fatalf("after a refused renewal of a lapsed lease the draft is %s; want unconfirmed", d.Status)
	}
	// Swept unconfirmed: still lease_lapsed, so the plugin still reports.
	if code, r := w.renew(w.tok, id); code != http.StatusConflict || r.Code != "lease_lapsed" {
		t.Fatalf("a renewal of a swept unconfirmed draft: %d %+v; want 409 lease_lapsed", code, r)
	}
	_, pending := w.fileForItem("x:renew-pending", "A post nobody claimed.")
	if code, r := w.renew(w.tok, pending); code != http.StatusConflict || r.Code != "not_claimed" {
		t.Fatalf("a renewal of an unclaimed draft: %d %+v; want 409 not_claimed", code, r)
	}
}

// TestALateFailureOfAnUnconfirmedDraftSettlesItsItemAndWithdrawsTheEscalation
// is draft 363's shape (live, 2026-11-14): the plugin claimed the approved
// post, the post did NOT go out ("something covers the submit button"), and
// its ok:false arrived 18 s after the 2-minute lease lapsed — after the sweep
// had made the draft unconfirmed and escalated its item to the owner asking
// whether it was posted. The late failure: the draft is failed; the item is
// settled no_action naming the draft.refused follow-up, which carries the
// plugin's detail; the owner's "may have been posted" ask is withdrawn; and
// the owner's push says it went back to the officer.
func TestALateFailureOfAnUnconfirmedDraftSettlesItsItemAndWithdrawsTheEscalation(t *testing.T) {
	w := newOutboundWorld(t, true)
	pushes := w.withPushes()
	item, id := w.unconfirmedForItem("x:draft-363", "A post whose submit button was covered.")
	if got := w.escalationAsks(item); len(got) != 1 || got[0] != "open" {
		t.Fatalf("control: the owner's escalation asks before the result: %v; want one open", got)
	}
	const detail = "not sent (refused): something covers the submit button; nothing sent"
	if rec := w.result(id, `{"ok":false,"detail":"`+detail+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("the late failure: %d %s", rec.Code, rec.Body.String())
	}
	if d := w.draft(id); d.Status != plugins.DraftFailed {
		t.Fatalf("the draft: %s; want failed", d.Status)
	}
	refused, p, ok := w.refusedItemOf(id)
	if !ok {
		t.Fatal("no draft.refused item for the late failure")
	}
	if p["status"] != "failed" || p["by"] != "plugin" || p["detail"] != detail || p["item_id"] != float64(item) {
		t.Fatalf("the draft.refused payload: %v", p)
	}
	st, outcome, summary, _ := w.itemRow(item)
	if want := "draft " + strconv.FormatInt(id, 10) + " was refused by the plugin, not posted. The work continues on item " +
		strconv.FormatInt(refused, 10) + "."; st != officers.StatusDone || outcome != "no_action" || summary != want {
		t.Fatalf("the item after the late failure: %s %s %q; want done no_action %q", st, outcome, summary, want)
	}
	if got := w.escalationAsks(item); len(got) != 1 || got[0] != "cancelled" {
		t.Fatalf("the owner's escalation asks after the late failure: %v; want the one withdrawn", got)
	}
	w.srv.WaitForPushes()
	var bodies []string
	for _, x := range pushes.to(ep("outbound-owner")) {
		if x.Tag == "plugin-draft-"+strconv.FormatInt(id, 10) && x.Title == "A reply was not sent" {
			bodies = append(bodies, x.Body)
		}
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "It went back to the officer that wrote it.") {
		t.Fatalf("the owner's refusal pushes: %q; want one saying it went back to the officer", bodies)
	}
	w.srv.SweepOfficerAttention(w.ctx)
	if st, _, _, _ := w.itemRow(item); st != officers.StatusDone {
		t.Fatalf("the next Attention sweep moved the item to %s", st)
	}
}

// TestALateFailureLeavesAnEscalationThatIsNotItsDraftsOwn: the late-failure
// caller honours escalatedByDraft's "not ours" answers. (a) The OWNER
// escalated the waiting item by hand while the plugin held the draft's claim:
// neither the lapse's sweep nor the ok:false that follows touches that
// escalation (its summary is the owner's). (b) The unconfirmed draft's item
// was sent back by the owner (an asks entry) and is escalated again: a late
// ok:false leaves it, and its ask, alone. In both the officer still gets its
// draft.refused item.
func TestALateFailureLeavesAnEscalationThatIsNotItsDraftsOwn(t *testing.T) {
	w := newOutboundWorld(t, true)
	const body = "A post the owner escalated mid-send."
	item, id := w.fileForItem("x:hand", body)
	if rec := w.approve(id, bindingForm(body)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.claim(); rec.Code != http.StatusOK {
		t.Fatalf("the plugin's claim: %d %s", rec.Code, rec.Body.String())
	}
	const mine = "the owner holds this one: legal is reviewing the venue"
	if _, err := w.store.ResolveItem(w.ctx, 1, builtinOfficerID, item, "escalated", mine, time.Now()); err != nil {
		t.Fatalf("the owner's escalation by hand: %v", err)
	}
	w.draftSQL(id, `lease_until = now() - interval '1 minute'`)
	w.sweepByClaim()
	if st, _, summary, _ := w.itemRow(item); st != officers.StatusEscalated || summary != mine {
		t.Fatalf("(a) after the lapse: %s %q; want the owner's own escalation left as it is", st, summary)
	}
	if rec := w.result(id, `{"ok":false,"detail":"not sent"}`); rec.Code != http.StatusOK {
		t.Fatalf("(a) the late failure: %d %s", rec.Code, rec.Body.String())
	}
	if st, _, summary, _ := w.itemRow(item); st != officers.StatusEscalated || summary != mine {
		t.Fatalf("(a) after the late failure: %s %q; want the owner's own escalation left as it is", st, summary)
	}
	if _, _, ok := w.refusedItemOf(id); !ok {
		t.Fatal("(a) no draft.refused item for the failure")
	}

	item2, id2 := w.unconfirmedForItem("x:sent-back", "An unconfirmed post sent back since.")
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `UPDATE officer_items SET asks = asks || jsonb_build_array(jsonb_build_object(
		'ask_id', 1, 'question', 'escalation', 'asked_by', 'user 1', 'asked_at', now(), 'status', 'answered', 'answer', 'look again'))
		WHERE id = $1`, item2); err != nil {
		t.Fatal(err)
	}
	if rec := w.result(id2, `{"ok":false,"detail":"not sent"}`); rec.Code != http.StatusOK {
		t.Fatalf("(b) the late failure: %d %s", rec.Code, rec.Body.String())
	}
	if st, _, _, _ := w.itemRow(item2); st != officers.StatusEscalated {
		t.Fatalf("(b) the late failure moved an item escalated again since to %s; want left escalated", st)
	}
	if got := w.escalationAsks(item2); len(got) != 1 || got[0] != "open" {
		t.Fatalf("(b) the escalation asks: %v; want the open one left alone", got)
	}
}
