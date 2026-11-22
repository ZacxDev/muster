package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sessionwake"
)

// A REJECTION OF WORK A SESSION ASKED FOR IS NEVER DROPPED (round-1 audit of
// #998): the item is closed quietly ONLY when a fresh rejection wake was
// actually filed for the session (decideDraftAs files it first), a rejection
// after an approval is a line of its own (0178's session_wakes_one_per_draft
// keys a decision wake on its decision), and a rejection wake that settles
// "Not delivered" goes back to the officer after all (wakeNotDelivered).

// requesterSummary is the summary of an item closed because its rejection
// went to the session that enqueued it.
func requesterSummary(draft int64) string {
	return "draft " + itoa(draft) + " was rejected by the owner, not posted. The owner's decision was sent to the session that enqueued this item, " +
		"which decides what happens next: there is nothing to redraft. Should muster be unable to deliver it, it comes back to this officer " +
		"as a draft.refused item."
}

// officerWakesOf is draft id's officer_draft_decided wakes, oldest first.
func (w *officerWakeWorld) officerWakesOf(id int64) []sessionwake.Wake {
	w.t.Helper()
	var out []sessionwake.Wake
	rows, err := w.f.pool.Query(authz.System(w.ctx), `SELECT decision, state, detail FROM session_wakes
		WHERE owner_id = 1 AND kind = 'officer_draft_decided' AND draft_id = $1 ORDER BY id`, id)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var x sessionwake.Wake
		var st string
		if err := rows.Scan(&x.Decision, &st, &x.Detail); err != nil {
			w.t.Fatal(err)
		}
		x.State = sessionwake.State(st)
		out = append(out, x)
	}
	return out
}

// TestARejectionAfterAnApprovalReachesTheSessionAsItsOwnLine: the owner
// approves an officer's draft (the session is told), then withdraws it with a
// rejection before its plugin claims it. The rejection is a SECOND line, with
// the owner's reason — and only then is the item closed as the session's.
func TestARejectionAfterAnApprovalReachesTheSessionAsItsOwnLine(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-approve-then-reject")
	id := w.draftForItem(item, retryPost)
	w.approveDraft(id, retryPost)
	w.tick()
	approved := "muster: you approved " + officerRef(id, item) + ". Its plugin sends it next; muster tells this session how that went."
	if got := w.pastes(); len(got) != 1 || got[0] != approved {
		t.Fatalf("after the approval the session took %q; want %q", got, approved)
	}
	const reason = "Hold it; the launch slipped."
	if rec := w.rejectForm(id, reason); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	if d := w.draft(id); d.Status != plugins.DraftRejected {
		t.Fatalf("fixture: the unclaimed approval was not withdrawn (draft is %s)", d.Status)
	}
	w.tick()
	got := w.pastes()
	if len(got) != 2 {
		t.Fatalf("the session took %d pastes; want 2 (the approval, then the rejection): %q", len(got), got)
	}
	pre := "muster: you rejected " + officerRef(id, item) + ". It will not be sent, and the officer will not redraft it: officer item #" +
		itoa(item) + " is closed, and what happens next is this session's call. The block below is the reason you gave, relayed by muster.\n\n" +
		"<<<owner-reason untrusted nonce="
	if !strings.HasPrefix(got[1], pre) || !strings.Contains(got[1], "|   "+reason+"\n") {
		t.Fatalf("the second line:\n%s\nwant it to start:\n%s\nand carry the reason fenced", got[1], pre)
	}
	if ws := w.officerWakesOf(id); len(ws) != 2 || ws[0].Decision != sessionwake.DecisionApproved || ws[1].Decision != sessionwake.DecisionRejected {
		t.Fatalf("the draft's decision wakes: %+v; want approved then rejected", ws)
	}
	if f, _, ok := w.refusedItemOf(id); ok {
		t.Fatalf("the officer got draft.refused item %d for a rejection the session was told", f)
	}
	if it := w.itemFull(item); it.status != "done" || it.outcome != "no_action" || it.summary != requesterSummary(id) {
		t.Fatalf("the item is %s/%s %q; want done/no_action %q", it.status, it.outcome, it.summary, requesterSummary(id))
	}
}

// rejectionWakeFails is a wake store whose Create of an officer draft's
// rejection wake fails (every other call is the real store's).
type rejectionWakeFails struct{ sessionwake.Store }

func (f rejectionWakeFails) Create(ctx context.Context, n sessionwake.New) (sessionwake.Wake, bool, error) {
	if n.Kind == sessionwake.KindOfficerDraftDecided && n.Decision == sessionwake.DecisionRejected {
		return sessionwake.Wake{}, false, errors.New("injected: the wake store refused the row")
	}
	return f.Store.Create(ctx, n)
}

// TestARejectionWhoseWakeCannotBeFiledGoesBackToTheOfficer: the session is
// recorded and its host mirrors here, but the rejection's wake cannot be
// written. The rejection is then the officer's, with the owner's reason —
// never closed as "the session's" when the session was told nothing.
func TestARejectionWhoseWakeCannotBeFiledGoesBackToTheOfficer(t *testing.T) {
	w := newOfficerWakeWorld(t)
	w.srv.ext.SessionWakes = rejectionWakeFails{w.srv.ext.SessionWakes}
	item := w.sessionEnqueues("plan-reject-wake-fails")
	const body = "A post whose rejection's wake cannot be written."
	id := w.draftForItem(item, body)
	if rec := w.rejectForm(id, "Not this one."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	w.tick()
	follow, p, ok := w.refusedItemOf(id)
	if !ok || p["detail"] != "Not this one." || p["by"] != "owner" || p["body"] != body {
		t.Fatalf("the rejection did not go back to the officer (follow-up %v, payload %v)", ok, p)
	}
	if it, want := w.itemFull(item), "draft "+itoa(id)+" was rejected by the owner, not posted. The work continues on item "+itoa(follow)+"."; it.status != "done" ||
		it.outcome != "no_action" || it.summary != want {
		t.Fatalf("the item is %s/%s %q; want done/no_action %q", it.status, it.outcome, it.summary, want)
	}
	if got := w.pastes(); len(got) != 0 {
		t.Fatalf("the session took %q; the wake was never written", got)
	}
}

// TestARejectionTheSessionNeverReceivesGoesBackToTheOfficer: the rejection's
// wake is filed, but no window shows the session (it ended), and a day later
// it settles "Not delivered". The rejection then goes to the officer as the
// draft.refused item it would have had — once, however many passes follow.
func TestARejectionTheSessionNeverReceivesGoesBackToTheOfficer(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-reject-never-delivered")
	const body = "A post whose asker has gone."
	id := w.draftForItem(item, body)
	w.host.set(func(m *wakeMirror) { m.gone = true })
	if rec := w.rejectForm(id, "Wrong venue."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	w.tick()
	if ws := w.officerWakesOf(id); len(ws) != 1 || ws[0].State != sessionwake.Queued || ws[0].Detail != wakeWaitNoWindow {
		t.Fatalf("with no window the rejection's wake is %+v; want one queued, saying %q", ws, wakeWaitNoWindow)
	}
	if f, _, ok := w.refusedItemOf(id); ok {
		t.Fatalf("draft.refused item %d filed while the rejection may still reach the session", f)
	}
	if it := w.itemFull(item); it.summary != requesterSummary(id) {
		t.Fatalf("the item's summary: %q; want %q", it.summary, requesterSummary(id))
	}
	// A day later.
	w.srv.sendQueueClock = func() time.Time { return time.Now().Add(wakeGiveUp + time.Minute) }
	t.Cleanup(func() { w.srv.sendQueueClock = nil })
	w.tick()
	if ws := w.officerWakesOf(id); len(ws) != 1 || ws[0].State != sessionwake.Unconfirmed || !strings.HasPrefix(ws[0].Detail, "Not delivered") {
		t.Fatalf("a day later the rejection's wake is %+v; want unconfirmed, Not delivered", ws)
	}
	_, p, ok := w.refusedItemOf(id)
	if !ok || p["detail"] != "Wrong venue." || p["by"] != "owner" || p["body"] != body {
		t.Fatalf("the undelivered rejection did not go back to the officer (follow-up %v, payload %v)", ok, p)
	}
	w.tick()
	var n int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items WHERE owner_id = 1 AND kind = $1 AND subject_key = $2`,
		draftRefusedKind, "draft:"+itoa(id)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d draft.refused items for the draft; want exactly 1", n)
	}
	if got := w.pastes(); len(got) != 0 {
		t.Fatalf("the session took %q with no window", got)
	}
}

// TestAReplyDraftRejectedAfterItsApprovalWakesTheSessionTwice: 0177's own
// kind (a reply draft a session wrote) has the same shape — an approval, then
// its withdrawal by a rejection before the plugin claims it: two lines.
func TestAReplyDraftRejectedAfterItsApprovalWakesTheSessionTwice(t *testing.T) {
	f := newWakeFixture(t)
	_, id, rid := f.routedDraft(t)
	if _, refused := f.srv.decideDraftAs(f.ownerCtx(), id, plugins.DraftDecision{Approve: true, Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); refused != nil {
		t.Fatalf("approve: %+v", refused)
	}
	f.srv.WaitForAttentionDeliveries()
	f.tick(t)
	f.onePaste(t)
	if _, refused := f.srv.decideDraftAs(f.ownerCtx(), id, plugins.DraftDecision{Approve: false, Reason: "Not Thursday after all",
		Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); refused != nil {
		t.Fatalf("reject the approved draft: %+v", refused)
	}
	f.srv.WaitForAttentionDeliveries()
	f.tick(t)
	got := f.host.taken()
	if len(got) != 2 {
		t.Fatalf("the session took %d pastes; want 2 (the approval, then the rejection)", len(got))
	}
	pre := "muster: you rejected reply draft #" + itoa(id) + " (route " + rid + "), which this session wrote."
	if !strings.HasPrefix(got[1].text, pre) || !strings.Contains(got[1].text, "|   Not Thursday after all\n") {
		t.Fatalf("the second line:\n%s\nwant it to start %q and carry the reason fenced", got[1].text, pre)
	}
}

// TestAnExpectedSessionOnlyRefuses: expect_session (what `muster officer item
// enqueue` passes from its environment) names a session the call is NOT bound
// to: refused, nothing queued, nothing recorded. Naming the bound session, the
// call goes through and the requester is still the binding's.
func TestAnExpectedSessionOnlyRefuses(t *testing.T) {
	w := newOfficerWakeWorld(t)
	out := w.mirrorCall(routeSID, officerItemEnqueueTool, map[string]any{"officer": builtinOfficerID, "kind": "owner.plan",
		"subject_key": "plan-expect-other", "expect_session": wakeOtherSID})
	if !strings.HasPrefix(out, "tool error: ") || !strings.Contains(out, "this call is bound to Claude Code session "+routeSID+" (") ||
		!strings.Contains(out, "nothing was queued or recorded") {
		t.Fatalf("a call bound to another session than the expected one: %s", out)
	}
	var n int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items WHERE subject_key = 'plan-expect-other'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d items queued by a refused call", n)
	}
	out = w.mirrorCall(routeSID, officerItemEnqueueTool, map[string]any{"officer": builtinOfficerID, "kind": "owner.plan",
		"subject_key": "plan-expect-mine", "expect_session": routeSID})
	if strings.HasPrefix(out, "tool error") || !strings.Contains(out, `"requested_by_this_session":true`) {
		t.Fatalf("the call naming its own session: %s", out)
	}
	var item int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT id FROM officer_items WHERE subject_key = 'plan-expect-mine'`).Scan(&item); err != nil {
		t.Fatal(err)
	}
	if h, s := w.itemRequester(item); h != "tower" || s != routeSID {
		t.Fatalf("the requester is %q/%q; want the bound caller tower/%s", h, s, routeSID)
	}
}
