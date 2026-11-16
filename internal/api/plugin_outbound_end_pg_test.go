package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// No terminal state of an outbound draft leaves the item it was filed for
// WAITING (builtin.DraftEndedTx, internal/api draftEndFor): an expired or
// binding-broken draft goes back to its officer and closes its item in the
// transaction that ended it; an unconfirmed one (it may have been posted) is
// never handed back to be redrafted — its item is escalated to the owner;
// and past the draft.refused bound the follow-up is filed HELD and the item
// still settles, naming it.
//
// The expected strings are written out here, not read from the code under
// test.

// withPushes records the pushes to user 1 (the officer's owner).
func (w *outboundWorld) withPushes() *payloadPusher {
	w.t.Helper()
	pusher, store := &payloadPusher{}, &memPush{}
	claim(w.t, store, 1, ep("outbound-owner"))
	w.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	return pusher
}

// draftSQL runs an UPDATE on draft id's row as the table owner, past the
// row's immutability triggers (time moved on, without waiting for it; a
// binding broken behind 0121's guard).
func (w *outboundWorld) draftSQL(id int64, set string) {
	w.t.Helper()
	dsn := dbtest.DSN(w.t)
	dbtest.OwnerExec(w.t, dsn, `ALTER TABLE plugin_reply_drafts DISABLE TRIGGER USER`)
	defer dbtest.OwnerExec(w.t, dsn, `ALTER TABLE plugin_reply_drafts ENABLE TRIGGER USER`)
	dbtest.OwnerExec(w.t, dsn, `UPDATE plugin_reply_drafts SET `+set+` WHERE id = `+strconv.FormatInt(id, 10))
}

// sweepByClaim is the plugin's next claim, which sweeps first: 204, nothing
// handed out.
func (w *outboundWorld) sweepByClaim() {
	w.t.Helper()
	if rec := w.claim(); rec.Code != http.StatusNoContent {
		w.t.Fatalf("the sweeping claim: %d %s; want 204, nothing handed out", rec.Code, rec.Body.String())
	}
}

// fillRefusedBound queues maxOpenRefusedItems unrelated draft.refused items.
func (w *outboundWorld) fillRefusedBound() {
	w.t.Helper()
	for i := 0; i < 3; i++ {
		if _, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "draft.refused",
			SubjectKey: "draft:prior-" + strconv.Itoa(i)}, time.Now()); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *outboundWorld) notices(item int64) string {
	w.t.Helper()
	var n string
	_ = w.f.pool.QueryRow(authz.System(w.ctx), `SELECT notices::text FROM officer_items WHERE id = $1`, item).Scan(&n)
	return n
}

// TestAnExpiredDraftSettlesItsItem (case 1): a draft for an item, never
// decided, expires; the next sweep — in its own transaction — queues a
// draft.refused item saying it expired, by muster, and closes the item
// naming it.
func TestAnExpiredDraftSettlesItsItem(t *testing.T) {
	w := newOutboundWorld(t, true)
	item, id := w.fileForItem("x:expired", "A post nobody decided on.")
	w.draftSQL(id, `expires_at = now() - interval '1 minute'`)
	w.sweepByClaim()
	if d := w.draft(id); d.Status != plugins.DraftExpired {
		t.Fatalf("the draft: %s; want expired", d.Status)
	}
	refused, p, ok := w.refusedItemOf(id)
	if !ok {
		t.Fatal("no draft.refused item for the expired draft")
	}
	if p["status"] != "expired" || p["by"] != "muster" || p["item_id"] != float64(item) ||
		p["detail"] != "not sent within 7 days of being written: it expired undecided, or approved but never collected by its plugin" {
		t.Fatalf("the draft.refused payload: %v", p)
	}
	st, outcome, summary, _ := w.itemRow(item)
	if want := "draft " + strconv.FormatInt(id, 10) + " expired, not posted. The work continues on item " + strconv.FormatInt(refused, 10) + "."; st != officers.StatusDone || outcome != "no_action" || summary != want {
		t.Fatalf("the item: %s %s %q; want done no_action %q", st, outcome, summary, want)
	}
}

// TestAnExpiredDraftPastTheBoundIsHeld (cases 1 + 4): with the officer's
// draft.refused items at the bound, the sweep's follow-up is filed HELD (not
// claimable) and the item is finished naming it — no longer escalated to
// its owner, since the officer WILL be told.
func TestAnExpiredDraftPastTheBoundIsHeld(t *testing.T) {
	w := newOutboundWorld(t, true)
	item, id := w.fileForItem("x:expired-full", "A post that expired at the bound.")
	w.fillRefusedBound()
	w.draftSQL(id, `expires_at = now() - interval '1 minute'`)
	w.sweepByClaim()
	held, p, ok := w.refusedItemOf(id)
	if !ok {
		t.Fatal("past the bound the expired draft's draft.refused item was dropped")
	}
	if p["status"] != "expired" || p["by"] != "muster" {
		t.Fatalf("the held payload: %v", p)
	}
	if out, ok := w.claimItem(held); ok {
		t.Fatalf("the held item was claimable at the bound: %s", out)
	}
	st, outcome, summary, _ := w.itemRow(item)
	want := "draft " + strconv.FormatInt(id, 10) + " expired, not posted. Its officer already has 3 open draft.refused items, so the work is held on item " +
		strconv.FormatInt(held, 10) + " and delivered to it when one of them is finished."
	if st != officers.StatusDone || outcome != "no_action" || summary != want || w.notices(item) != "[]" {
		t.Fatalf("the item: %s %s %q notices %s; want done no_action %q, no notices", st, outcome, summary, w.notices(item), want)
	}
}

// TestAnUnconfirmedDraftIsEscalatedAndNeverRedrafted (case 2): the plugin
// collects the approved draft and its lease lapses with no result. The draft
// may be live, so:
//   - 🔴 NEVER RE-POSTED: no draft.refused item is queued (the officer is not
//     asked to redraft it), the draft is not offered to a claim again, and
//     the instance cannot claim the item back;
//   - the item is escalated to the owner, and the officer Attention sweep
//     raises the owner's ask saying it may have been posted.
func TestAnUnconfirmedDraftIsEscalatedAndNeverRedrafted(t *testing.T) {
	w := newOutboundWorld(t, true)
	const body = "A post whose plugin went quiet."
	item, id := w.fileForItem("x:unconfirmed", body)
	if rec := w.approve(id, bindingForm(body)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.claim(); rec.Code != http.StatusOK {
		t.Fatalf("the plugin's claim: %d %s", rec.Code, rec.Body.String())
	}
	w.draftSQL(id, `lease_until = now() - interval '1 minute'`)
	w.sweepByClaim()
	if d := w.draft(id); d.Status != plugins.DraftUnconfirmed {
		t.Fatalf("the draft: %s; want unconfirmed", d.Status)
	}
	if _, p, ok := w.refusedItemOf(id); ok {
		t.Fatalf("an unconfirmed draft was handed back to be redrafted: %v", p)
	}
	w.sweepByClaim() // not offered again
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(item, 10)+`"}`); !isToolErr(out) {
		t.Fatalf("the instance claimed the unconfirmed draft's item back: %s", out)
	}
	st, outcome, summary, _ := w.itemRow(item)
	want := "Draft " + strconv.FormatInt(id, 10) + " may have been posted: its plugin collected it and reported no result before its lease lapsed, " +
		"so it is unconfirmed. muster will not offer it again and the officer is not asked to redraft it, which could post it twice. " +
		"Check example.forum for it."
	if st != officers.StatusEscalated || outcome != "escalated" || summary != want || !strings.Contains(w.notices(item), `"recipient": 1`) {
		t.Fatalf("the item: %s %s %q notices %s; want escalated to user 1, %q", st, outcome, summary, w.notices(item), want)
	}
	w.srv.SweepOfficerAttention(w.ctx)
	var asked int
	_ = w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM attention_asks WHERE owner_id = 1 AND asker_label = $1
		AND question LIKE $2`, "officer "+builtinOfficerID, "%item "+strconv.FormatInt(item, 10)+"%may have been posted%").Scan(&asked)
	if asked != 1 {
		t.Fatalf("%d Attention asks to the owner about the unconfirmed draft; want 1", asked)
	}
}

// TestABindingBrokenDraftSettlesItsItem (case 3): the claim finds the
// approved body changed and withdraws the draft; in the claim's transaction
// its officer gets a draft.refused item and the item is closed.
func TestABindingBrokenDraftSettlesItsItem(t *testing.T) {
	w := newOutboundWorld(t, true)
	const body = "A post changed after approval."
	item, id := w.fileForItem("x:binding", body)
	if rec := w.approve(id, bindingForm(body)); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	w.draftSQL(id, `body = body || ' and more'`)
	w.sweepByClaim()
	if d := w.draft(id); d.Status != plugins.DraftRejected || d.Detail != "the body or media changed after approval" {
		t.Fatalf("the draft: %s %q; want withdrawn", d.Status, d.Detail)
	}
	refused, p, ok := w.refusedItemOf(id)
	if !ok {
		t.Fatal("no draft.refused item for the binding-broken draft")
	}
	if p["status"] != "rejected" || p["by"] != "muster" || p["detail"] != "the body or media changed after approval" {
		t.Fatalf("the draft.refused payload: %v", p)
	}
	st, outcome, summary, _ := w.itemRow(item)
	want := "draft " + strconv.FormatInt(id, 10) + " was withdrawn at its claim, not posted: the body or media changed after approval. " +
		"The work continues on item " + strconv.FormatInt(refused, 10) + "."
	if st != officers.StatusDone || outcome != "no_action" || summary != want {
		t.Fatalf("the item: %s %s %q; want %q", st, outcome, summary, want)
	}
}

// officerTagPushes is user 1's pushes on draft id's "-officer" tag (the
// retired "did not reach the officer" push's).
func officerTagPushes(srv *Server, p *payloadPusher, id int64) []push.Payload {
	srv.WaitForPushes()
	var out []push.Payload
	for _, x := range p.to(ep("outbound-owner")) {
		if x.Tag == "plugin-draft-"+strconv.FormatInt(id, 10)+"-officer" {
			out = append(out, x)
		}
	}
	return out
}

// TestAnOwnerRejectionPastTheBoundIsHeldNotLost (case 4): with the officer's
// draft.refused items at the bound, the owner's rejection is HELD for the
// officer — the item settles naming it — and no "did not reach the officer"
// push goes out: it does reach it, later. Below the bound, as above.
func TestAnOwnerRejectionPastTheBoundIsHeldNotLost(t *testing.T) {
	w := newOutboundWorld(t, true)
	pushes := w.withPushes()
	_, below := w.fileForItem("x:below", "Rejected under the bound.")
	if rec := w.rejectForm(below, "no"); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d", rec.Code)
	}
	w.fillRefusedBound()
	item, at := w.fileForItem("x:at", "Rejected at the bound.")
	if rec := w.rejectForm(at, "no again"); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d", rec.Code)
	}
	held, p, ok := w.refusedItemOf(at)
	if !ok || p["detail"] != "no again" {
		t.Fatalf("past the bound the rejection: %v %v; want a held draft.refused item with the reason", ok, p)
	}
	st, outcome, summary, _ := w.itemRow(item)
	want := "draft " + strconv.FormatInt(at, 10) + " was rejected by the owner, not posted. Its officer already has 3 open draft.refused items, " +
		"so the work is held on item " + strconv.FormatInt(held, 10) + " and delivered to it when one of them is finished."
	if st != officers.StatusDone || outcome != "no_action" || summary != want {
		t.Fatalf("the item: %s %s %q; want %q", st, outcome, summary, want)
	}
	for _, id := range []int64{below, at} {
		if ps := officerTagPushes(w.srv, pushes, id); len(ps) != 0 {
			t.Fatalf("draft %d: %d not-told pushes; want none", id, len(ps))
		}
	}
}

// TestAPluginRefusalPastTheBoundIsHeld (case 4, the plugin's door): the
// refusal is held for the officer, the item settles naming it, and the
// refusal push says it goes back to the officer once it is under the bound.
func TestAPluginRefusalPastTheBoundIsHeld(t *testing.T) {
	w := newOutboundWorld(t, true)
	pushes := w.withPushes()
	w.fillRefusedBound()
	item, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "plugin.reply", SubjectKey: "x:refused-full"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(item, 10)+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	id, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"example.forum","target":{"kind":"post"},"body":"Refused at the bound.","item_id":"` +
		strconv.FormatInt(item, 10) + `"}`)
	if id == 0 {
		t.Fatalf("file: %s", out)
	}
	if rec := w.check(w.tok, id, false, "rule: off topic"); rec.Code != http.StatusOK {
		t.Fatalf("refuse: %d %s", rec.Code, rec.Body.String())
	}
	held, p, ok := w.refusedItemOf(id)
	if !ok || p["by"] != "plugin" {
		t.Fatalf("past the bound the plugin's refusal: %v %v; want a held draft.refused item", ok, p)
	}
	st, outcome, summary, _ := w.itemRow(item)
	want := "draft " + strconv.FormatInt(id, 10) + " was refused by the plugin, not posted. Its officer already has 3 open draft.refused items, " +
		"so the work is held on item " + strconv.FormatInt(held, 10) + " and delivered to it when one of them is finished."
	if st != officers.StatusDone || outcome != "no_action" || summary != want {
		t.Fatalf("the item: %s %s %q; want %q", st, outcome, summary, want)
	}
	w.srv.WaitForPushes()
	var bodies []string
	for _, p := range pushes.to(ep("outbound-owner")) {
		if p.Tag == "plugin-draft-"+strconv.FormatInt(id, 10) && p.Title == "A reply was not sent" {
			bodies = append(bodies, p.Body)
		}
	}
	const tail = "refused it. It goes back to the officer that wrote it once that officer has fewer than 3 open draft.refused items. It said: "
	if len(bodies) != 1 || !strings.Contains(bodies[0], tail) || strings.Contains(bodies[0], "NOT told") {
		t.Fatalf("the refusal pushes: %q; want one saying it is held for the officer", bodies)
	}
}

// TestADraftFiledBeforeItemsWaitedSettlesItsItem (the transitional case):
// an item left CLAIMED by a draft filed before 0.218.0 settles when the
// draft ends, once its lease has lapsed; one an instance holds under a live
// lease is left alone.
func TestADraftFiledBeforeItemsWaitedSettlesItsItem(t *testing.T) {
	w := newOutboundWorld(t, true)
	asBefore := func(item int64, lease string) {
		t.Helper()
		dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE officer_items SET status = 'claimed', claimed_by = 'an-old-instance', lease_expires_at = now() + interval '`+
			lease+`' WHERE id = `+strconv.FormatInt(item, 10))
	}
	lapsed, id := w.fileForItem("x:before-lapsed", "Filed before items waited.")
	asBefore(lapsed, "-1 hour")
	held, id2 := w.fileForItem("x:before-held", "Filed before items waited, still held.")
	asBefore(held, "1 hour")
	w.draftSQL(id, `expires_at = now() - interval '1 minute'`)
	w.draftSQL(id2, `expires_at = now() - interval '1 minute'`)
	w.sweepByClaim()
	if st, outcome, summary, holder := w.itemRow(lapsed); st != officers.StatusDone || outcome != "no_action" || holder != "" ||
		!strings.HasPrefix(summary, "draft "+strconv.FormatInt(id, 10)+" expired, not posted.") {
		t.Fatalf("the lapsed item: %s %s %q held by %q; want done no_action", st, outcome, summary, holder)
	}
	if st, _, _, holder := w.itemRow(held); st != officers.StatusClaimed || holder != "an-old-instance" {
		t.Fatalf("the held item: %s held by %q; want left claimed", st, holder)
	}
	if _, _, ok := w.refusedItemOf(id2); !ok {
		t.Fatal("the held item's expired draft queued no draft.refused item")
	}
}

// TestAnEndedDraftLeavesAnItemWaitingOnAnotherDraft: an item whose OLDER
// draft ends while a newer draft for it is still pending is left waiting on
// the newer one, which settles it in turn.
func TestAnEndedDraftLeavesAnItemWaitingOnAnotherDraft(t *testing.T) {
	w := newOutboundWorld(t, true)
	item, older := w.fileForItem("x:two", "The older draft.")
	// The instance holds it again (as after a lapsed pre-0.218 claim) and
	// files a second draft for it.
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE officer_items SET status = 'claimed', claimed_by = '`+w.agent.Name+
		`', lease_expires_at = now() + interval '1 hour' WHERE id = `+strconv.FormatInt(item, 10))
	b, _ := json.Marshal("The newer draft.")
	newer, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"example.forum","target":{"kind":"post"},"body":` + string(b) +
		`,"item_id":"` + strconv.FormatInt(item, 10) + `"}`)
	if newer == 0 {
		t.Fatalf("file the newer draft: %s", out)
	}
	w.draftSQL(older, `expires_at = now() - interval '1 minute'`)
	w.sweepByClaim()
	if st, _, _, _ := w.itemRow(item); st != officers.StatusWaiting {
		t.Fatalf("the older draft's end moved the item to %s; want left waiting on the newer draft", st)
	}
	if rec := w.rejectForm(newer, "no"); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject the newer draft: %d", rec.Code)
	}
	if st, outcome, _, _ := w.itemRow(item); st != officers.StatusDone || outcome != "no_action" {
		t.Fatalf("after the newer draft's rejection the item is %s %s; want done no_action", st, outcome)
	}
}

// TestTurningChecksOffSettlesAnAwaitingDraftsItem (a fifth end, round 1's
// Y1): the owner turns the plugin's Checks replies off while an outbound
// draft for an item awaits its check; the draft is refused and, in the same
// transaction, its officer gets a draft.refused item and the item is closed.
func TestTurningChecksOffSettlesAnAwaitingDraftsItem(t *testing.T) {
	w := newOutboundWorld(t, true)
	item, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "plugin.reply", SubjectKey: "x:checks-off"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(item, 10)+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	id, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"example.forum","target":{"kind":"post"},"body":"Awaiting its check.","item_id":"` +
		strconv.FormatInt(item, 10) + `"}`)
	if id == 0 {
		t.Fatalf("file: %s", out)
	}
	if st, _, _, _ := w.itemRow(item); st != officers.StatusWaiting {
		t.Fatalf("control: the item before the toggle is %s; want waiting", st)
	}
	sess1 := authz.WithPrincipal(w.ctx, authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if _, _, _, err := w.ps.SetDraftChecks(sess1, w.plugin.ID, false); err != nil {
		t.Fatal(err)
	}
	if d := w.draft(id); d.Status != plugins.DraftRejected {
		t.Fatalf("the draft: %s; want refused", d.Status)
	}
	refused, p, ok := w.refusedItemOf(id)
	if !ok {
		t.Fatal("no draft.refused item for the draft refused by the turn-off")
	}
	if p["by"] != "muster" || p["detail"] != "the plugin's Checks replies was turned off before it checked this draft" {
		t.Fatalf("the draft.refused payload: %v", p)
	}
	st, outcome, summary, _ := w.itemRow(item)
	want := "draft " + strconv.FormatInt(id, 10) + " was refused unchecked, not posted: the plugin's Checks replies was turned off before it checked this draft. " +
		"The work continues on item " + strconv.FormatInt(refused, 10) + "."
	if st != officers.StatusDone || outcome != "no_action" || summary != want {
		t.Fatalf("the item: %s %s %q; want %q", st, outcome, summary, want)
	}
}
