package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// A REDRAFT IS THE SAME ASK (builtin.inheritRequesterTx): when an officer's
// draft for an item a session enqueued ends unsent and muster files the
// officer a draft.refused follow-up to redraft from, the follow-up carries the
// item's requested_host / requested_session — so the owner's decision on the
// redraft wakes the session that asked, not nobody. Before the fix the
// follow-up named no session and every decision on the redraft was silent.

// unsentDraftForItem has the officer's instance claim item and file body for
// it, NOT yet checked: the draft's id.
func (w *officerWakeWorld) unsentDraftForItem(item int64, body string) int64 {
	w.t.Helper()
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(item, 10)+`"}`); isToolErr(out) {
		w.t.Fatalf("claim: %s", out)
	}
	b, _ := json.Marshal(body)
	id, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"example.forum","target":{"kind":"post"},"body":` + string(b) +
		`,"item_id":"` + strconv.FormatInt(item, 10) + `"}`)
	if id == 0 {
		w.t.Fatalf("file: %s", out)
	}
	return id
}

// followUpOf is draft id's draft.refused follow-up, failing when there is none.
func (w *officerWakeWorld) followUpOf(id int64) int64 {
	w.t.Helper()
	f, _, ok := w.refusedItemOf(id)
	if !ok {
		w.t.Fatalf("no draft.refused follow-up for draft %d", id)
	}
	return f
}

// TestARedraftAfterAPluginRefusalWakesTheSessionThatEnqueuedTheItem is the
// regression, end to end: a session enqueues an item, the officer's draft for
// it is refused by its plugin's check, the officer redrafts from the
// draft.refused follow-up, and the owner approves the redraft. The follow-up
// names the session, and the approval of the redraft reaches it.
func TestARedraftAfterAPluginRefusalWakesTheSessionThatEnqueuedTheItem(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-refused-redraft")
	id := w.unsentDraftForItem(item, "A first post its plugin refuses.")
	if rec := w.check(w.outboundWorld.tok, id, false, "rule: off topic"); rec.Code != http.StatusOK {
		t.Fatalf("refuse: %d %s", rec.Code, rec.Body.String())
	}
	follow := w.followUpOf(id)
	if h, s := w.itemRequester(follow); h != "tower" || s != routeSID {
		t.Fatalf("follow-up %d's requester: %q/%q; want tower/%s (item %d's)", follow, h, s, routeSID, item)
	}
	redraft := w.draftForItem(follow, retryPost)
	w.approveDraft(redraft, retryPost)
	w.tick()
	want := "muster: you approved " + officerRef(redraft, follow) + ". Its plugin sends it next; muster tells this session how that went."
	got := w.pastes()
	if len(got) == 0 || got[len(got)-1] != want {
		t.Fatalf("after the redraft's approval the session took %q; want its last line %q", got, want)
	}
}

// TestAnExpiredDraftsFollowUpKeepsTheSessionThatEnqueuedTheItem: an end
// muster makes itself (the sweep's expiry, officerPluginLink.DraftEndedTx, in
// the claim's transaction) files a follow-up naming the same session.
func TestAnExpiredDraftsFollowUpKeepsTheSessionThatEnqueuedTheItem(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-expired-redraft")
	id := w.draftForItem(item, "A post nobody decided on.")
	w.draftSQL(id, `expires_at = now() - interval '1 minute'`)
	w.sweepByClaim()
	follow := w.followUpOf(id)
	if h, s := w.itemRequester(follow); h != "tower" || s != routeSID {
		t.Fatalf("follow-up %d's requester: %q/%q; want tower/%s (item %d's)", follow, h, s, routeSID, item)
	}
}

// TestARejectionHandedBackToTheOfficerKeepsTheSession: a rejection whose wake
// could not be filed (the session's host no longer mirrors here) goes back to
// the officer (TestARejectionNoWakeCanReachGoesBackToTheOfficer) — and its
// follow-up still names the session, so a later decision on the redraft
// reaches it once its host mirrors again.
func TestARejectionHandedBackToTheOfficerKeepsTheSession(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-reject-handed-back")
	id := w.draftForItem(item, "A post whose asker is away.")
	w.host.set(func(m *wakeMirror) { m.remote = true })
	if rec := w.rejectForm(id, "Not now."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	w.tick()
	follow := w.followUpOf(id)
	if h, s := w.itemRequester(follow); h != "tower" || s != routeSID {
		t.Fatalf("follow-up %d's requester: %q/%q; want tower/%s (item %d's)", follow, h, s, routeSID, item)
	}
}

// TestAFollowUpOfAnItemNoSessionEnqueuedNamesNoSession (an invariant guard,
// not regression coverage: main never wrote a requester here either): an item
// enqueued through the admin API names no session, and neither does its
// follow-up — the inheritance invents none.
func TestAFollowUpOfAnItemNoSessionEnqueuedNamesNoSession(t *testing.T) {
	w := newOfficerWakeWorld(t)
	code, res := adminCall(t, w.h, "POST", "/api/officers/defs/"+builtinOfficerID+"/items", `{"kind":"owner.plan","subject_key":"plan-refused-nobody"}`)
	f, _ := res["id"].(float64)
	if code != http.StatusCreated || f == 0 {
		t.Fatalf("the admin API's enqueue: %d %v", code, res)
	}
	id := w.unsentDraftForItem(int64(f), "A post no session asked for.")
	if rec := w.check(w.outboundWorld.tok, id, false, "rule: off topic"); rec.Code != http.StatusOK {
		t.Fatalf("refuse: %d %s", rec.Code, rec.Body.String())
	}
	if h, s := w.itemRequester(w.followUpOf(id)); h != "" || s != "" {
		t.Fatalf("the follow-up names %q/%q; want no session", h, s)
	}
}
