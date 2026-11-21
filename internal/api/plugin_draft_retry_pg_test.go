package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// RETRY CHAINS over the real routes, stores and officer store
// (plugin_draft_retry.go, internal/plugins draft_retry.go): an approved
// outbound draft whose send failed with nothing sent and a transient cause is
// tried again on its own, as a successor row; nothing else is.

const retryPost = "A post that goes out once."

// retryEP is user 1's push device in these tests.
var retryEP = ep("retry-owner")

// withRetryPushes gives w's server a push service and user 1 a device.
func (w *outboundWorld) withRetryPushes() *payloadPusher {
	w.t.Helper()
	store, pusher := &memPush{}, &payloadPusher{}
	claim(w.t, store, 1, retryEP)
	ext := w.srv.ext
	ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	w.srv.UseExtensions(ext)
	return pusher
}

// pushesTitled is every push user 1's device got titled title.
func (w *outboundWorld) pushesTitled(p *payloadPusher, title string) []push.Payload {
	w.srv.WaitForPushes()
	var out []push.Payload
	for _, n := range p.to(retryEP) {
		if n.Title == title {
			out = append(out, n)
		}
	}
	return out
}

// approvedForItem files retryPost for a fresh item, approves it on the card:
// the item's and the draft's ids.
func (w *outboundWorld) approvedForItem(subject string) (int64, int64) {
	w.t.Helper()
	item, id := w.fileForItem(subject, retryPost)
	if rec := w.approve(id, bindingForm(retryPost)); rec.Code != http.StatusSeeOther {
		w.t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	return item, id
}

// claimed is the plugin's claim, which must hand out draft want (0: none).
func (w *outboundWorld) claimed(want int64) {
	w.t.Helper()
	rec := w.claim()
	if want == 0 {
		if rec.Code != http.StatusNoContent {
			w.t.Fatalf("claim: %d %s; want nothing to claim", rec.Code, rec.Body.String())
		}
		return
	}
	var c struct {
		DraftID int64 `json:"draft_id"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &c) != nil || c.DraftID != want {
		w.t.Fatalf("claim: %d %s; want draft %d", rec.Code, rec.Body.String(), want)
	}
}

// postResult posts body as draft id's result: the status code and the answer.
func (w *outboundWorld) postResult(id int64, body string) (int, map[string]any) {
	w.t.Helper()
	rec := w.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(id, 10)+"/result", body, bearerTok(w.tok))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// transientFail is a result asserting nothing was sent, for a cause that clears.
const transientFail = `{"ok":false,"nothing_sent":true,"transient":true,"detail":"error before any submit, nothing sent: No current window"}`

// successors is root's chain beyond the root, by attempt.
func (w *outboundWorld) successors(root int64) []plugins.Draft {
	w.t.Helper()
	rows, err := w.f.pool.Query(authz.System(w.ctx), `SELECT id FROM plugin_reply_drafts WHERE retry_of = $1 ORDER BY attempt`, root)
	if err != nil {
		w.t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			w.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []plugins.Draft
	for _, id := range ids {
		out = append(out, w.draft(id))
	}
	return out
}

// due makes draft id's backoff over (its not_before a second ago).
func (w *outboundWorld) due(id int64) {
	w.t.Helper()
	dbtest.OwnerExec(w.t, dbtest.DSN(w.t), `UPDATE plugin_reply_drafts SET not_before = now() - interval '1 second' WHERE id = `+strconv.FormatInt(id, 10))
}

// refusedItems counts the officer's draft.refused items.
func (w *outboundWorld) refusedItems() int {
	w.t.Helper()
	var n int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items WHERE kind = 'draft.refused'`).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// TestATransientNothingSentFailureRetriesOnItsOwn is the feature: the owner
// approves once; the send fails with nothing_sent + transient; muster files
// exactly ONE successor — the root's approved bytes, attempt 2, its backoff
// RetryBackoff(1) — which the claim skips until its not_before and then hands
// out; the item keeps waiting (no draft.refused follow-up) and is settled
// once, done, when the successor is posted. The failed row stays failed and
// claimed: no claim is cleared.
func TestATransientNothingSentFailureRetriesOnItsOwn(t *testing.T) {
	w := newOutboundWorld(t, true)
	item, root := w.approvedForItem("retry:1")
	w.claimed(root)
	before := time.Now()
	code, out := w.postResult(root, transientFail)
	if code != http.StatusOK || out["status"] != "failed" {
		t.Fatalf("result: %d %v", code, out)
	}
	succ := w.successors(root)
	if len(succ) != 1 {
		t.Fatalf("after one transient failure: %d successors, want exactly 1", len(succ))
	}
	next := succ[0]
	// The answer is exactly {draft_id, status}: the next attempt is a new
	// draft the claim hands out, never named in the result.
	if len(out) != 2 || out["draft_id"] != float64(root) {
		t.Fatalf("the result's answer: %v; want exactly {draft_id, status}", out)
	}
	r := w.draft(root)
	if r.Status != plugins.DraftFailed || r.ClaimedAt == nil || !r.NothingSent {
		t.Fatalf("the failed root: %s claimed %v nothing_sent %v; want failed, still claimed, nothing_sent", r.Status, r.ClaimedAt, r.NothingSent)
	}
	var auditTransient string
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT detail->>'transient' FROM audit_log WHERE action = 'plugin.draft.failed' AND object = $1`,
		plugins.DraftObject(root)).Scan(&auditTransient); err != nil || auditTransient != "true" {
		t.Fatalf("the failure's audit row: transient %q (%v); want true", auditTransient, err)
	}
	if next.Status != plugins.DraftApproved || next.ClaimedAt != nil || next.Attempt != 2 || next.RetryOf == nil || *next.RetryOf != root ||
		!next.BindingHolds() || *next.ApprovedBodySHA256 != sumHex([]byte(retryPost)) || next.Body != retryPost ||
		next.OfficerItemID == nil || *next.OfficerItemID != item || !next.ExpiresAt.Equal(r.ExpiresAt) {
		t.Fatalf("the successor: %+v", next)
	}
	if wait := next.NotBefore.Sub(before); wait < 50*time.Second || wait > 70*time.Second {
		t.Fatalf("the successor's not_before is %v after the result; want RetryBackoff(1) = 1m", wait)
	}
	if !strings.Contains(next.RetryLastDetail, "No current window") {
		t.Fatalf("the successor's last detail: %q", next.RetryLastDetail)
	}
	// 🔴 NOT BEFORE ITS BACKOFF.
	w.claimed(0)
	if st, _, _, _ := w.itemRow(item); st != officers.StatusWaiting {
		t.Fatalf("the item while its chain retries: %s; want waiting", st)
	}
	if n := w.refusedItems(); n != 0 {
		t.Fatalf("%d draft.refused items while the chain retries; want 0", n)
	}
	w.due(next.ID)
	w.claimed(next.ID)
	if code, out := w.postResult(next.ID, `{"ok":true,"external_ref":"post-9"}`); code != http.StatusOK || out["status"] != "sent" {
		t.Fatalf("the successor's result: %d %v", code, out)
	}
	if st, oc, summary, _ := w.itemRow(item); st != officers.StatusDone || oc != "done" || !strings.Contains(summary, "post-9") {
		t.Fatalf("the item after the successor posted: %s/%s %q; want done naming post-9", st, oc, summary)
	}
	if n := w.refusedItems(); n != 0 {
		t.Fatalf("%d draft.refused items after the chain posted; want 0", n)
	}
	if len(w.successors(root)) != 1 {
		t.Fatal("a successor was filed after the post")
	}
}

// TestNoRetryWithoutBothFlags: a failure that does not assert nothing_sent
// AND transient — an old plugin's (neither field), a permanent cause
// (nothing_sent alone) — files no successor and goes back to the officer
// exactly as before (a draft.refused item, the item settled); the result
// carries no retry. transient without nothing_sent, and either flag with
// ok true, are refused (400) and change nothing.
func TestNoRetryWithoutBothFlags(t *testing.T) {
	w := newOutboundWorld(t, true)
	for i, body := range []string{
		`{"ok":false,"detail":"the account bar never drew"}`,
		`{"ok":false,"nothing_sent":true,"detail":"venue policy: body rejected"}`,
		`{"ok":false,"nothing_sent":true,"transient":false,"detail":"glance review missing"}`,
	} {
		item, id := w.approvedForItem("noretry:" + strconv.Itoa(i))
		w.claimed(id)
		code, out := w.postResult(id, body)
		if code != http.StatusOK || out["status"] != "failed" {
			t.Fatalf("%s: %d %v", body, code, out)
		}
		if s := w.successors(id); len(s) != 0 {
			t.Errorf("%s: %d successors, want none", body, len(s))
		}
		if _, _, ok := w.refusedItemOf(id); !ok {
			t.Errorf("%s: no draft.refused item for the officer (the old path)", body)
		}
		if st, oc, _, _ := w.itemRow(item); st != officers.StatusDone || oc != "no_action" {
			t.Errorf("%s: the item is %s/%s, want done/no_action (the old path)", body, st, oc)
		}
	}
	_, id := w.approvedForItem("noretry:bad")
	w.claimed(id)
	for _, body := range []string{
		`{"ok":false,"transient":true,"detail":"x"}`,
		`{"ok":true,"nothing_sent":true}`,
		`{"ok":true,"transient":true,"nothing_sent":true}`,
	} {
		if code, out := w.postResult(id, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v; want 400", body, code, out)
		}
	}
	if d := w.draft(id); d.Status != plugins.DraftApproved || d.FinishedAt != nil {
		t.Fatalf("after the refused results the draft is %s; want still approved, unsettled", d.Status)
	}
}

// TestNoRetryPastTheChainsExpiry: a transient, nothing-sent failure whose
// backoff would end past the chain's expiry files no successor: the chain
// ends with that failure (the old path for the officer) and the owner is
// pushed once that it ended unsent.
func TestNoRetryPastTheChainsExpiry(t *testing.T) {
	w := newOutboundWorld(t, true)
	p := w.withRetryPushes()
	item, id := w.approvedForItem("expiry:1")
	w.claimed(id)
	dbtest.OwnerExec(t, dbtest.DSN(t), `SET session_replication_role = replica; UPDATE plugin_reply_drafts SET expires_at = now() + interval '30 seconds' WHERE id = `+strconv.FormatInt(id, 10))
	code, out := w.postResult(id, transientFail)
	if code != http.StatusOK || out["status"] != "failed" {
		t.Fatalf("result: %d %v; want failed", code, out)
	}
	if s := w.successors(id); len(s) != 0 {
		t.Fatalf("%d successors past the expiry, want none", len(s))
	}
	if st, oc, _, _ := w.itemRow(item); st != officers.StatusDone || oc != "no_action" {
		t.Fatalf("the item: %s/%s; want settled done/no_action", st, oc)
	}
	if got := w.pushesTitled(p, DraftRetryEndedPushTitle); len(got) != 1 || !strings.Contains(got[0].Body, "No current window") {
		t.Fatalf("chain-ended pushes: %+v; want exactly one carrying the last detail", got)
	}
}

// TestARetryChainSettlesItsItemOnceAndPushesTwice: attempts 1-3 fail with
// nothing sent (transient), each filing one successor with the next backoff
// (1m, 5m, 15m); the owner is pushed ONCE, at attempt 3's failure, carrying
// its detail — never at attempts 1 and 2 — and ONCE when the chain ends
// unsent at its expiry (the sweep). The item is settled once, by the chain's
// end, with ONE draft.refused follow-up (the last attempt's); the chain is
// one card on the Asks page throughout.
func TestARetryChainSettlesItsItemOnceAndPushesTwice(t *testing.T) {
	w := newOutboundWorld(t, true)
	p := w.withRetryPushes()
	item, root := w.approvedForItem("chain:1")
	cur := root
	for _, step := range []struct {
		attempt int
		backoff time.Duration
		detail  string
	}{{1, time.Minute, "No current window"}, {2, 5 * time.Minute, "account switch failed"}, {3, 15 * time.Minute, "X wants a security check"}} {
		if step.attempt > 1 {
			w.due(cur)
		}
		w.claimed(cur)
		before := time.Now()
		body, _ := json.Marshal(map[string]any{"ok": false, "nothing_sent": true, "transient": true, "detail": step.detail})
		if code, out := w.postResult(cur, string(body)); code != http.StatusOK || out["status"] != "failed" {
			t.Fatalf("attempt %d's result: %d %v", step.attempt, code, out)
		}
		succ := w.successors(root)
		if len(succ) != step.attempt {
			t.Fatalf("after attempt %d failed: %d successors, want %d (one per failure)", step.attempt, len(succ), step.attempt)
		}
		next := succ[len(succ)-1]
		if next.Attempt != step.attempt+1 {
			t.Fatalf("the successor of attempt %d is attempt %d", step.attempt, next.Attempt)
		}
		if wait := next.NotBefore.Sub(before); wait < step.backoff-10*time.Second || wait > step.backoff+10*time.Second {
			t.Fatalf("attempt %d's backoff: %v; want %v", step.attempt, wait, step.backoff)
		}
		pushes := w.pushesTitled(p, DraftRetryingPushTitle)
		switch {
		case step.attempt < plugins.RetryOwnerPushAttempt && len(pushes) != 0:
			t.Fatalf("attempt %d pushed %+v; want no push before attempt %d", step.attempt, pushes, plugins.RetryOwnerPushAttempt)
		case step.attempt == plugins.RetryOwnerPushAttempt && (len(pushes) != 1 || !strings.Contains(pushes[0].Body, "security check") ||
			pushes[0].Data["url"] != draftLink(next.ID)):
			t.Fatalf("attempt 3 pushed %+v; want exactly one carrying its detail, to the live attempt's card", pushes)
		}
		if st, _, _, _ := w.itemRow(item); st != officers.StatusWaiting {
			t.Fatalf("the item after attempt %d: %s; want waiting", step.attempt, st)
		}
		// THE CHAIN IS ONE CARD: its latest attempt.
		cards, err := w.ps.DraftCards(w.ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		var shown []int64
		for _, c := range cards {
			if c.Draft.ChainRoot() == root {
				shown = append(shown, c.Draft.ID)
			}
		}
		if len(shown) != 1 || shown[0] != next.ID {
			t.Fatalf("the Asks list shows %v of the chain; want only its latest attempt %d", shown, next.ID)
		}
		cur = next.ID
	}
	if n := w.refusedItems(); n != 0 {
		t.Fatalf("%d draft.refused items while the chain retries; want 0", n)
	}
	// The chain ends: its live attempt expires unsent (the sweep).
	dbtest.OwnerExec(t, dbtest.DSN(t), `SET session_replication_role = replica; UPDATE plugin_reply_drafts SET expires_at = now() - interval '1 second' WHERE id = `+strconv.FormatInt(cur, 10))
	w.claimed(0)
	if d := w.draft(cur); d.Status != plugins.DraftExpired {
		t.Fatalf("the last attempt after its expiry: %s", d.Status)
	}
	if st, oc, _, _ := w.itemRow(item); st != officers.StatusDone || oc != "no_action" {
		t.Fatalf("the item after the chain ended: %s/%s; want done/no_action", st, oc)
	}
	if n := w.refusedItems(); n != 1 {
		t.Fatalf("%d draft.refused items after the chain ended; want exactly 1", n)
	}
	if _, _, ok := w.refusedItemOf(cur); !ok {
		t.Fatal("the follow-up is not the last attempt's")
	}
	if got := w.pushesTitled(p, DraftRetryingPushTitle); len(got) != 1 {
		t.Fatalf("%d 'still retrying' pushes over the chain; want exactly 1", len(got))
	}
	ended := w.pushesTitled(p, DraftRetryEndedPushTitle)
	if len(ended) != 1 || ended[0].Tag != draftTag(root) {
		t.Fatalf("chain-ended pushes: %+v; want exactly one on the chain's tag", ended)
	}
	// Another sweep tells nobody again.
	w.claimed(0)
	if got := w.pushesTitled(p, DraftRetryEndedPushTitle); len(got) != 1 {
		t.Fatalf("a second sweep pushed again: %d", len(got))
	}
}

// TestAManualRetryIsAllowedOnlyAfterNothingWasSent: the card's Retry (and
// the store under plugin_draft_retry) files a successor claimable NOW only
// when the chain's latest attempt failed with nothing_sent and has not
// expired; it is refused for an unknown outcome, a sent, a rejected, a
// pending and an expired draft, and while an attempt of the chain is still
// approved (one live attempt).
func TestAManualRetryIsAllowedOnlyAfterNothingWasSent(t *testing.T) {
	w := newOutboundWorld(t, true)
	via := plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}
	refused := func(id int64, code string) {
		t.Helper()
		_, err := w.ps.RetryDraft(w.ctx, id, via)
		var rr plugins.RetryRefusal
		if !asRetryRefusal(err, &rr) || rr.Code != code {
			t.Fatalf("retry of draft %d: %v; want refused %s", id, err, code)
		}
	}
	// Pending (never approved).
	_, pending := w.fileForItem("manual:pending", retryPost)
	refused(pending, plugins.RetryNotFailed)
	// An unknown outcome: failed without nothing_sent.
	_, unknown := w.approvedForItem("manual:unknown")
	w.claimed(unknown)
	w.postResult(unknown, `{"ok":false,"detail":"outcome unknown"}`)
	refused(unknown, plugins.RetryUnknown)
	// Sent.
	_, sent := w.approvedForItem("manual:sent")
	w.claimed(sent)
	w.postResult(sent, `{"ok":true,"external_ref":"p-1"}`)
	refused(sent, plugins.RetrySent)
	// Rejected.
	_, rejected := w.fileForItem("manual:rejected", retryPost)
	w.rejectForm(rejected, "")
	refused(rejected, plugins.RetryRejected)
	// A permanent nothing-sent failure: allowed, by the card's Retry.
	_, perm := w.approvedForItem("manual:perm")
	w.claimed(perm)
	w.postResult(perm, `{"ok":false,"nothing_sent":true,"detail":"wrong account: logged in as someone else"}`)
	if len(w.successors(perm)) != 0 {
		t.Fatal("a non-transient failure filed a successor on its own")
	}
	rec := w.do("POST", "/plugins/drafts/"+strconv.FormatInt(perm, 10)+"/retry", url.Values{}.Encode(), w.operatorForm)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the card's Retry: %d %s", rec.Code, rec.Body.String())
	}
	succ := w.successors(perm)
	if len(succ) != 1 || succ[0].Attempt != 2 || succ[0].NotBefore == nil || time.Until(*succ[0].NotBefore) > time.Second {
		t.Fatalf("after the manual retry: %+v; want one successor claimable now", succ)
	}
	// ONE LIVE ATTEMPT: a second retry, by either attempt's id, is refused.
	refused(perm, plugins.RetryLive)
	refused(succ[0].ID, plugins.RetryLive)
	w.claimed(succ[0].ID)
	refused(perm, plugins.RetryLive)
	// Expired: the chain's latest attempt failed with nothing sent, past
	// its expiry.
	w.postResult(succ[0].ID, `{"ok":false,"nothing_sent":true,"detail":"still the wrong account"}`)
	dbtest.OwnerExec(t, dbtest.DSN(t), `SET session_replication_role = replica; UPDATE plugin_reply_drafts SET expires_at = now() - interval '1 second' WHERE id IN (`+
		strconv.FormatInt(perm, 10)+`,`+strconv.FormatInt(succ[0].ID, 10)+`)`)
	refused(perm, plugins.RetryExpired)
}

// asRetryRefusal is errors.As for a RetryRefusal.
func asRetryRefusal(err error, rr *plugins.RetryRefusal) bool { return errors.As(err, rr) }
