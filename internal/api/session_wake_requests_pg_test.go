package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sessionwake"
)

// A DECISION ON WORK A MIRRORED SESSION ASKED FOR WAKES THAT SESSION
// (session_wake_requests.go, migration 0178): an officer's draft for an item
// the session enqueued through its own MCP door, and a step-up card its call
// raised. The fixtures are session_wake_requests_probe_pg_test.go's (an
// outbound world whose host tower mirrors here) and the probes' (a route
// fixture); the host is played by wakeMirror, every row is muster's own.

// wakeOtherSID is a Claude Code session id the window @4 does NOT run.
const wakeOtherSID = "0ther5e5-0000-4000-8000-0000000000b2"

// itemRequester is item id's recorded requester, read as the system.
func (w *officerWakeWorld) itemRequester(id int64) (string, string) {
	w.t.Helper()
	var h, s string
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT requested_host, requested_session FROM officer_items WHERE id = $1`, id).Scan(&h, &s); err != nil {
		w.t.Fatal(err)
	}
	return h, s
}

// wakeRows counts owner 1's session wakes of kind k.
func (w *officerWakeWorld) wakeRows(k sessionwake.Kind) int {
	w.t.Helper()
	var n int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM session_wakes WHERE owner_id = 1 AND kind = $1`, string(k)).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// pastes is every text the window took.
func (w *officerWakeWorld) pastes() []string {
	var out []string
	for _, p := range w.host.taken() {
		if p.window != "@4" || p.shown != routeSID {
			w.t.Fatalf("a paste went to window %q pinned to %q; want @4 pinned to %s", p.window, p.shown, routeSID)
		}
		out = append(out, p.text)
	}
	return out
}

func (w *officerWakeWorld) approveDraft(id int64, body string) {
	w.t.Helper()
	if rec := w.approve(id, bindingForm(body)); rec.Code != http.StatusSeeOther {
		w.t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
}

func officerRef(draft, item int64) string {
	return "draft #" + itoa(draft) + ", which an officer filed for officer item #" + itoa(item) + " that this session enqueued"
}

// TestARejectedOfficerDraftWakesTheSessionThatEnqueuedItsItemOnce: the item
// records the session the mirror door bound; the owner's Reject pastes ONE
// line into it — muster's sentence naming the draft and the item, the owner's
// reason fenced — and never the draft's body; more passes paste nothing more.
func TestARejectedOfficerDraftWakesTheSessionThatEnqueuedItsItemOnce(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-reject")
	if h, s := w.itemRequester(item); h != "tower" || s != routeSID {
		t.Fatalf("the item's requester: %q/%q; want tower/%s (the bound caller)", h, s, routeSID)
	}
	const body = "A launch post the owner does not want."
	id := w.draftForItem(item, body)
	if rec := w.rejectForm(id, "Wrong week.\r\nWait for the release notes."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	w.tick()
	got := w.pastes()
	if len(got) != 1 {
		t.Fatalf("the session took %d pastes; want exactly 1: %q", len(got), got)
	}
	pre := "muster: you rejected " + officerRef(id, item) + ". It will not be sent, and the officer will not redraft it: officer item #" + itoa(item) +
		" is closed, and what happens next is this session's call. The block below is the reason you gave, relayed by muster.\n\n<<<owner-reason untrusted nonce="
	if !strings.HasPrefix(got[0], pre) {
		t.Fatalf("the line does not start with muster's sentence and the reason's fence:\n%s", got[0])
	}
	if !strings.Contains(got[0], "\n| reason:\n|   Wrong week.\n|   Wait for the release notes.\n<<<end owner-reason nonce=") {
		t.Fatalf("the owner's reason is not fenced, line by line:\n%s", got[0])
	}
	if strings.Contains(got[0], body) {
		t.Fatalf("the draft's body reached the session:\n%s", got[0])
	}
	w.tick()
	if n := len(w.pastes()); n != 1 {
		t.Fatalf("more passes pasted again: %d, want 1", n)
	}
}

// openItems counts owner 1's officer items that are not finished: what the
// officer could still claim, or what waits on someone.
func (w *officerWakeWorld) openItems() int {
	w.t.Helper()
	var n int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items WHERE owner_id = 1
		AND status IN ('open', 'claimed', 'waiting', 'escalated')`).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// TestARejectionOfAnItemASessionEnqueuedGoesOnlyToThatSession: the owner
// rejects an officer draft for an item a session enqueued. The rejection and
// its reason reach that session, whose line says the officer will not redraft;
// the officer gets no draft.refused item, no escalation, and the item is
// finished, not left waiting — and the owner's reason is in no officer item.
func TestARejectionOfAnItemASessionEnqueuedGoesOnlyToThatSession(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-reject-only-session")
	id := w.draftForItem(item, "A launch post the owner does not want.")
	const reason = "Wrong week; I will handle this one myself."
	if rec := w.rejectForm(id, reason); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	w.tick()
	if f, _, ok := w.refusedItemOf(id); ok {
		t.Fatalf("the officer got draft.refused item %d for a rejection its requesting session was told", f)
	}
	it := w.itemFull(item)
	want := requesterSummary(id)
	if it.status != "done" || it.outcome != "no_action" || it.summary != want {
		t.Fatalf("the item is %s/%s %q; want done/no_action %q", it.status, it.outcome, it.summary, want)
	}
	if n := w.openItems(); n != 0 {
		t.Fatalf("%d officer items are still open (a follow-up, an escalation, the item itself); want 0", n)
	}
	var leaked int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items WHERE owner_id = 1
		AND (payload::text LIKE '%' || $1 || '%' OR coalesce(summary, '') LIKE '%' || $1 || '%')`, "I will handle this one myself").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("the owner's reason reached %d officer item(s); it is the session's alone", leaked)
	}
	got := w.pastes()
	if len(got) != 1 {
		t.Fatalf("the session took %d pastes; want 1: %q", len(got), got)
	}
	pre := "muster: you rejected " + officerRef(id, item) + ". It will not be sent, and the officer will not redraft it: officer item #" +
		itoa(item) + " is closed, and what happens next is this session's call. The block below is the reason you gave, relayed by muster.\n\n" +
		"<<<owner-reason untrusted nonce="
	if !strings.HasPrefix(got[0], pre) || !strings.Contains(got[0], "|   "+reason+"\n") {
		t.Fatalf("the session's line:\n%s\nwant it to start:\n%s\nand carry the reason fenced", got[0], pre)
	}
}

// TestARejectionNoWakeCanReachGoesBackToTheOfficer: a session is recorded on
// the item, but its host no longer mirrors here, so no wake could be filed for
// it. The rejection is then the officer's, as with no session at all: it is
// never dropped on the floor.
func TestARejectionNoWakeCanReachGoesBackToTheOfficer(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-reject-unreachable")
	id := w.draftForItem(item, "A post whose asker is gone.")
	w.host.set(func(m *wakeMirror) { m.remote = true })
	if rec := w.rejectForm(id, "Not now."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	w.tick()
	if n := w.wakeRows(sessionwake.KindOfficerDraftDecided); n != 0 {
		t.Fatalf("fixture: %d officer wakes filed for a host that does not mirror here; want 0", n)
	}
	if _, p, ok := w.refusedItemOf(id); !ok || p["detail"] != "Not now." {
		t.Fatalf("the rejection did not go back to the officer (follow-up %v, payload %v)", ok, p)
	}
}

// TestARejectionOfAnItemNoSessionEnqueuedStillGoesBackToTheOfficer: with no
// requesting session recorded, a rejection is the officer's, as before — a
// draft.refused item with the owner's reason, the draft's body and its plan,
// and the item finished naming it. (An invariant guard: this path did not
// change; it pins that the session rule above reaches no further.)
func TestARejectionOfAnItemNoSessionEnqueuedStillGoesBackToTheOfficer(t *testing.T) {
	w := newOfficerWakeWorld(t)
	code, res := adminCall(t, w.h, "POST", "/api/officers/defs/"+builtinOfficerID+"/items", `{"kind":"owner.plan","subject_key":"plan-reject-officer"}`)
	f, _ := res["id"].(float64)
	if code != http.StatusCreated || f == 0 {
		t.Fatalf("the admin API's enqueue: %d %v", code, res)
	}
	item := int64(f)
	const body = "A post the owner sends back to its officer."
	id := w.draftForItem(item, body)
	if rec := w.rejectForm(id, "Too long."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	w.tick()
	follow, p, ok := w.refusedItemOf(id)
	if !ok {
		t.Fatal("no draft.refused item: a rejection no session asked for must go back to the officer")
	}
	plan, _ := p["plan"].(map[string]any)
	if p["detail"] != "Too long." || p["by"] != "owner" || p["body"] != body || plan == nil || plan["item_id"] != float64(item) {
		t.Fatalf("the follow-up's payload: %v", p)
	}
	it := w.itemFull(item)
	if want := "draft " + itoa(id) + " was rejected by the owner, not posted. The work continues on item " + itoa(follow) + "."; it.status != "done" ||
		it.outcome != "no_action" || it.summary != want {
		t.Fatalf("the item is %s/%s %q; want done/no_action %q", it.status, it.outcome, it.summary, want)
	}
	if got := w.pastes(); len(got) != 0 {
		t.Fatalf("a session was woken: %q", got)
	}
}

// TestAnApprovedOfficerDraftAndItsSendEachWakeTheSessionOnce: the approval is
// one line, the plugin's "sent" one more.
func TestAnApprovedOfficerDraftAndItsSendEachWakeTheSessionOnce(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-send")
	id := w.draftForItem(item, retryPost)
	w.approveDraft(id, retryPost)
	w.tick()
	want := []string{"muster: you approved " + officerRef(id, item) + ". Its plugin sends it next; muster tells this session how that went."}
	if got := w.pastes(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("after the approval the session took %q; want %q", got, want)
	}
	w.claimed(id)
	if code, out := w.postResult(id, `{"ok":true,"external_ref":"post-1"}`); code != http.StatusOK || out["status"] != "sent" {
		t.Fatalf("result: %d %v", code, out)
	}
	w.tick()
	want = append(want, "muster: "+officerRef(id, item)+" — it was sent.")
	if got := w.pastes(); len(got) != 2 || got[1] != want[1] {
		t.Fatalf("after the send the session took %q; want %q", got, want)
	}
	w.tick()
	if n := len(w.pastes()); n != 2 {
		t.Fatalf("more passes pasted again: %d, want 2", n)
	}
}

// TestAnOfficerDraftDecidedFromAnotherSessionSaysWhere: approved through
// plugin_draft_decide from another of the owner's sessions, the line names
// that session instead of "you approved", and so does the outcome line.
func TestAnOfficerDraftDecidedFromAnotherSessionSaysWhere(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-via")
	id := w.draftForItem(item, retryPost)
	if _, refused := w.srv.decideDraftAs(w.ctx, id, plugins.DraftDecision{Approve: true, BodySHA256: sumHex([]byte(retryPost)), MediaSHA256: []string{},
		Via: plugins.DraftDecisionSource{Via: plugins.DraftViaMCP, Host: "laptop", Session: wakeOtherSID}}); refused != nil {
		t.Fatalf("approve via MCP: %+v", refused)
	}
	w.tick()
	want := "muster: " + officerRef(id, item) + ", was approved — via plugin_draft_decide from laptop/" + wakeOtherSID +
		", not by you on the Asks page. Its plugin sends it next; muster tells this session how that went."
	if got := w.pastes(); len(got) != 1 || got[0] != want {
		t.Fatalf("the session took %q; want %q", got, want)
	}
	w.claimed(id)
	if code, _ := w.postResult(id, `{"ok":true,"external_ref":"post-3"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	w.tick()
	want = "muster: " + officerRef(id, item) + ", approved via plugin_draft_decide from laptop/" + wakeOtherSID + " — it was sent."
	if got := w.pastes(); len(got) != 2 || got[1] != want {
		t.Fatalf("the session took %q; want the outcome %q", got, want)
	}
}

// TestARetrySuccessorsOutcomeReachesTheSameSession: an approved draft whose
// first attempt fails with nothing sent and a transient cause is retried as a
// successor (0175); nothing is owed while the chain is still trying, and the
// successor's send is pasted into the session that enqueued the item, naming
// the successor and its attempt.
func TestARetrySuccessorsOutcomeReachesTheSameSession(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-retry")
	root := w.draftForItem(item, retryPost)
	w.approveDraft(root, retryPost)
	w.tick()
	w.claimed(root)
	if code, out := w.postResult(root, transientFail); code != http.StatusOK || out["status"] != "failed" {
		t.Fatalf("result: %d %v", code, out)
	}
	w.tick()
	if got := w.pastes(); len(got) != 1 {
		t.Fatalf("while the chain retries the session took %d pastes; want only the approval's: %q", len(got), got)
	}
	succ := w.successors(root)
	if len(succ) != 1 {
		t.Fatalf("%d successors; want 1", len(succ))
	}
	next := succ[0].ID
	w.due(next)
	w.claimed(next)
	if code, out := w.postResult(next, `{"ok":true,"external_ref":"post-2"}`); code != http.StatusOK || out["status"] != "sent" {
		t.Fatalf("the successor's result: %d %v", code, out)
	}
	w.tick()
	want := "muster: draft #" + itoa(next) + " (attempt 2 of its retry chain), which an officer filed for officer item #" + itoa(item) +
		" that this session enqueued — it was sent."
	got := w.pastes()
	if len(got) != 2 || got[1] != want {
		t.Fatalf("after the successor's send the session took %q; want the outcome %q", got, want)
	}
	if n := w.wakeRows(sessionwake.KindOfficerDraftOutcome); n != 1 {
		t.Fatalf("%d officer outcome wakes; want 1 (the successor's only)", n)
	}
}

// TestAChainThatStopsRetryingSaysSoWithItsAttempts: a transient failure, then
// a failure that is not transient: the chain ends on attempt 2, and that is
// the one outcome line.
func TestAChainThatStopsRetryingSaysSoWithItsAttempts(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-stop")
	root := w.draftForItem(item, retryPost)
	w.approveDraft(root, retryPost)
	w.claimed(root)
	if code, _ := w.postResult(root, transientFail); code != http.StatusOK {
		t.Fatal(code)
	}
	next := w.successors(root)[0].ID
	w.due(next)
	w.claimed(next)
	if code, out := w.postResult(next, `{"ok":false,"detail":"the venue refused it"}`); code != http.StatusOK || out["status"] != "failed" {
		t.Fatalf("the successor's result: %d %v", code, out)
	}
	w.tick()
	want := "muster: draft #" + itoa(next) + " (attempt 2 of its retry chain), which an officer filed for officer item #" + itoa(item) +
		" that this session enqueued — it could not be sent after 2 attempts (its plugin reported a failure), and muster stopped retrying it. " +
		"Nothing more is sent on its own."
	got := w.pastes()
	if len(got) != 2 || got[1] != want {
		t.Fatalf("the session took %q; want the approval and then %q", got, want)
	}
	if strings.Contains(got[1], "the venue refused it") {
		t.Fatalf("the plugin's detail reached the session: %s", got[1])
	}
}

// TestAnItemNoSessionEnqueuedWakesNoSession: an item enqueued through the
// admin API (the CLI's door: a hook token, which names no session) records no
// requester — not from the request's session headers, not from a payload that
// spells one — and the owner's decision on its draft pastes nothing.
func TestAnItemNoSessionEnqueuedWakesNoSession(t *testing.T) {
	w := newOfficerWakeWorld(t)
	rec := w.do("POST", "/api/officers/defs/"+builtinOfficerID+"/items",
		`{"kind":"owner.plan","subject_key":"plan-cli","payload":{"requested_host":"tower","requested_session":"`+routeSID+`","mirror_session":"`+routeSID+`"}}`,
		func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+connTestHookToken)
			r.Header.Set(mirrorSessionHeader, routeSID)
			r.Header.Set("X-Muster-Session-Id", routeSID)
			r.Header.Set("X-Muster-Host", "tower")
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("the admin API's enqueue: %d %s", rec.Code, rec.Body.String())
	}
	var item int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT id FROM officer_items WHERE subject_key = 'plan-cli'`).Scan(&item); err != nil {
		t.Fatal(err)
	}
	if h, s := w.itemRequester(item); h != "" || s != "" {
		t.Fatalf("a claim in the request became the requester: %q/%q", h, s)
	}
	id := w.draftForItem(item, retryPost)
	if rec := w.rejectForm(id, "No."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d", rec.Code)
	}
	w.tick()
	if got := w.pastes(); len(got) != 0 {
		t.Fatalf("an item no session enqueued woke the session: %q", got)
	}
	if n := w.wakeRows(sessionwake.KindOfficerDraftDecided); n != 0 {
		t.Fatalf("%d officer wakes filed; want 0", n)
	}
	// Positive control: the same decision on an item the session DID enqueue
	// is pasted.
	mine := w.sessionEnqueues("plan-control")
	id2 := w.draftForItem(mine, retryPost)
	if rec := w.rejectForm(id2, "No."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d", rec.Code)
	}
	w.tick()
	if got := w.pastes(); len(got) != 1 {
		t.Fatalf("the control took %d pastes; want 1", len(got))
	}
}

// TestASessionClaimInTheCallIsIgnored: officer_item_enqueue names no session —
// an argument that tries is refused, and a payload that spells another session
// is the officer's to read, never the requester: the requester is the session
// the mirror door bound.
func TestASessionClaimInTheCallIsIgnored(t *testing.T) {
	w := newOfficerWakeWorld(t)
	if out := w.mirrorCall(routeSID, officerItemEnqueueTool, map[string]any{"officer": builtinOfficerID, "kind": "owner.plan",
		"subject_key": "plan-forged", "requested_session": wakeOtherSID, "mirror_host": "tower"}); !strings.HasPrefix(out, "tool error: ") {
		t.Fatalf("an argument naming a session was accepted: %s", out)
	}
	out := w.mirrorCall(routeSID, officerItemEnqueueTool, map[string]any{"officer": builtinOfficerID, "kind": "owner.plan",
		"subject_key": "plan-forged", "payload": map[string]any{"requested_host": "tower", "requested_session": wakeOtherSID, "session": wakeOtherSID}})
	if strings.HasPrefix(out, "tool error") || !strings.Contains(out, `"requested_by_this_session":true`) {
		t.Fatalf("the enqueue: %s", out)
	}
	var item int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT id FROM officer_items WHERE subject_key = 'plan-forged'`).Scan(&item); err != nil {
		t.Fatal(err)
	}
	if h, s := w.itemRequester(item); h != "tower" || s != routeSID {
		t.Fatalf("the requester is %q/%q; want the bound caller tower/%s", h, s, routeSID)
	}
	// A call naming a session the host does not list runs as no session.
	if out := w.mirrorCall(wakeOtherSID, officerItemEnqueueTool, map[string]any{"officer": builtinOfficerID, "kind": "owner.plan",
		"subject_key": "plan-unlisted"}); !strings.HasPrefix(out, "tool error: ") {
		t.Fatalf("a session the host does not list enqueued: %s", out)
	}
	// FIRST REQUESTER WINS: the same subject again changes nothing.
	w.host.set(func(m *wakeMirror) { m.sid = wakeOtherSID })
	out = w.mirrorCall(wakeOtherSID, officerItemEnqueueTool, map[string]any{"officer": builtinOfficerID, "kind": "owner.plan", "subject_key": "plan-forged"})
	if !strings.Contains(out, `"created":false`) || !strings.Contains(out, `"requested_by_this_session":false`) {
		t.Fatalf("a second session's enqueue of the live item: %s", out)
	}
	if h, s := w.itemRequester(item); h != "tower" || s != routeSID {
		t.Fatalf("a second session took the item over: %q/%q", h, s)
	}
}

// TestAnotherOwnersSessionIsNeverWokenByAnOfficerDraft: the decision's wake is
// its owner's, invisible to every other principal, and goes only to the owner's
// own window: when the window is another owner's, nothing is pasted.
func TestAnotherOwnersSessionIsNeverWokenByAnOfficerDraft(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-stranger")
	id := w.draftForItem(item, retryPost)
	// The window (same host name, same session id) now shows only to user 2.
	w.host.set(func(m *wakeMirror) { m.owner = w.other })
	if rec := w.rejectForm(id, "No."); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d", rec.Code)
	}
	w.tick()
	if got := w.pastes(); len(got) != 0 {
		t.Fatalf("another owner's window took %q", got)
	}
	var wid int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT id FROM session_wakes WHERE owner_id = 1 AND kind = 'officer_draft_decided'`).Scan(&wid); err != nil {
		t.Fatalf("the owner's wake was not filed: %v", err)
	}
	sctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: w.other, Role: authz.RoleMember, Via: authz.ViaSession})
	if _, err := sessionwake.NewPG(w.f.pool).Get(sctx, wid); !errors.Is(err, sessionwake.ErrNotFound) {
		t.Fatalf("another owner read the wake: %v", err)
	}
	// And user 2's own session reaching for owner 1's officer is refused.
	w.srv.ext.MirrorTokens = fakeMirrorTokens{w.hostTok: {ID: 92, OwnerID: w.other, Host: "tower"}}
	if out := w.mirrorCall(routeSID, officerItemEnqueueTool, map[string]any{"officer": builtinOfficerID, "kind": "owner.plan",
		"subject_key": "plan-of-another"}); !strings.HasPrefix(out, "tool error: ") {
		t.Fatalf("another owner's session enqueued for this owner's officer: %s", out)
	}
}

// --- step-up cards ------------------------------------------------------------------

// stepUpWorld is a probe fixture whose host tower is covered by the step-up
// gate, and an ask on the owner's other host the gated call answers.
type stepUpWorld struct {
	f   *routeFixture
	m   *wakeMirror
	att *attention.PGStore
	x   attention.Ask
}

func newStepUpWorld(t *testing.T) *stepUpWorld {
	t.Helper()
	f, m, att, _ := probeFixture(t)
	declareHostOfficerOn(t, f.srv, f.owner, "tower")
	x, err := att.Create(probeOwner(f), attention.New{Asker: attention.Asker{MirrorHost: "laptop", MirrorSession: wakeOtherSID}, Question: "Go?"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_wakes WHERE owner_id = $1`, f.owner)
	})
	return &stepUpWorld{f: f, m: m, att: att, x: x}
}

// card has the window's session (or caller c) call attention_answer on x
// through the gate: the card's id.
func (s *stepUpWorld) card(t *testing.T, c toolCaller) int64 {
	t.Helper()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: s.f.owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
	out, gated := s.f.srv.hostStepUpGate(ctx, "tower", c, attentionAnswerTool, `{"id":`+itoa(s.x.ID)+`,"answer":"yes"}`)
	if !gated {
		t.Fatal("fixture: the step-up gate does not cover tower")
	}
	return stepUpCardOf(t, out)
}

func windowCaller() toolCaller {
	return toolCaller{asker: attention.Asker{MirrorHost: "tower", MirrorSession: routeSID, Label: "work:1"}, mirrorWindow: "@4", mirrorTmuxSession: "work"}
}

// TestAStepUpCardRecordsTheSessionThatRaisedIt: the card carries the bound
// caller's host and session; a remote caller's card carries none.
func TestAStepUpCardRecordsTheSessionThatRaisedIt(t *testing.T) {
	s := newStepUpWorld(t)
	card := s.card(t, windowCaller())
	got, err := s.att.Get(probeOwner(s.f), card)
	if err != nil || got.StepUp == nil || got.StepUp.RaisedByHost != "tower" || got.StepUp.RaisedBySession != routeSID {
		t.Fatalf("the card: %+v %v; want raised by tower/%s", got.StepUp, err, routeSID)
	}
	remote := toolCaller{asker: attention.Asker{MirrorHost: "elsewhere", MirrorSession: routeSID, Label: "x"}, remote: true}
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: s.f.owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
	out, gated := s.f.srv.hostStepUpGate(ctx, "elsewhere", remote, attentionAnswerTool, `{"id":`+itoa(s.x.ID)+`,"answer":"yes"}`)
	if !gated {
		t.Fatal("a remote caller's call was not gated")
	}
	rc, _ := s.att.Get(probeOwner(s.f), stepUpCardOf(t, out))
	if rc.StepUp == nil || rc.StepUp.RaisedByHost != "" || rc.StepUp.RaisedBySession != "" {
		t.Fatalf("a remote caller's card names a session: %+v", rc.StepUp)
	}
}

// TestAConfirmedStepUpCardIsPastedOnceIntoItsSession and its Refuse twin: the
// phone's answer is pasted once, as muster's line naming the card and the tool
// — never the call's arguments.
func TestAConfirmedStepUpCardIsPastedOnceIntoItsSession(t *testing.T) {
	for _, tc := range []struct {
		answer, want string
	}{
		{hostDecisionConfirm, "muster: you confirmed step-up card #%d, which this session's attention_answer call raised, on your phone: muster ran that call once, exactly as asked. Do not call it again; read what it did with the list tools."},
		{hostDecisionRefuse, "muster: you refused step-up card #%d, which this session's attention_answer call raised, on your phone: the call did not run, and nothing was done."},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			s := newStepUpWorld(t)
			card := s.card(t, windowCaller())
			if _, err := s.f.srv.answerAskFrom(asDevice(s.f.owner), card, tc.answer, attention.AnswerSource{}); err != nil {
				t.Fatalf("the phone's %s: %v", tc.answer, err)
			}
			probeTick(t, s.f)
			probeTick(t, s.f)
			want := strings.Replace(tc.want, "%d", itoa(card), 1)
			if got := s.m.taken(); len(got) != 1 || got[0].text != want || got[0].shown != routeSID {
				t.Fatalf("the session took %+v; want exactly %q", got, want)
			}
		})
	}
}

// TestAStepUpCardWhoseCallTheToolRefusedSaysSo: confirmed, but the tool then
// refused the call (here: the ask was answered meanwhile) — the line says it
// ran and was refused, and carries none of the tool's output.
func TestAStepUpCardWhoseCallTheToolRefusedSaysSo(t *testing.T) {
	s := newStepUpWorld(t)
	card := s.card(t, windowCaller())
	if _, err := s.f.srv.answerAsk(probeOwner(s.f), s.x.ID, "decided-elsewhere-words"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.f.srv.answerAskFrom(asDevice(s.f.owner), card, hostDecisionConfirm, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	probeTick(t, s.f)
	want := "muster: you confirmed step-up card #" + itoa(card) + ", which this session's attention_answer call raised, on your phone: " +
		"muster ran that call once, and the tool refused it. Read why with the list tools before you ask again."
	if got := s.m.taken(); len(got) != 1 || got[0].text != want {
		t.Fatalf("the session took %+v; want exactly %q", got, want)
	}
}

// TestADismissedStepUpCardIsPastedOnce: the owner's Dismiss of the card is the
// session's answer too.
func TestADismissedStepUpCardIsPastedOnce(t *testing.T) {
	s := newStepUpWorld(t)
	card := s.card(t, windowCaller())
	a, err := s.f.srv.cancelAsk(probeOwner(s.f), card)
	if err != nil {
		t.Fatal(err)
	}
	s.f.srv.wakeDismissed(probeOwner(s.f), a)
	probeTick(t, s.f)
	want := "muster: you dismissed step-up card #" + itoa(card) + ", which this session's attention_answer call raised, without confirming it: " +
		"the call did not run, and nothing was done."
	if got := s.m.taken(); len(got) != 1 || got[0].text != want {
		t.Fatalf("the session took %+v; want exactly %q", got, want)
	}
}

// TestAStepUpCardSettledByItsDraftsDecisionIsPastedOnce: a card about a draft
// that the owner decided on another door (settleDraftStepUps answers it with
// stepUpDraftAnswer) tells the session that raised it; a card the phone
// already answered is not told twice.
func TestAStepUpCardSettledByItsDraftsDecisionIsPastedOnce(t *testing.T) {
	s := newStepUpWorld(t)
	ctx := probeOwner(s.f)
	d := plugins.Draft{ID: 990001, Status: plugins.DraftApproved}
	file := func(raised string) attention.Ask {
		a, err := s.att.Create(ctx, attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel}, Options: []string{hostDecisionConfirm, hostDecisionRefuse},
			StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Post it?", Why: "why", Details: "details", DraftID: d.ID,
				RaisedByHost: "tower", RaisedBySession: raised}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	settled, confirmed := file(routeSID), file(routeSID)
	if _, err := s.att.Answer(ctx, settled.ID, stepUpDraftAnswer(d, plugins.DraftViaBrowser)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.att.Answer(ctx, confirmed.ID, hostDecisionConfirm); err != nil {
		t.Fatal(err)
	}
	s.f.srv.wakeStepUpsSettledBy(ctx, d, plugins.DraftViaBrowser)
	s.f.srv.wakeStepUpsSettledBy(ctx, d, plugins.DraftViaBrowser)
	probeTick(t, s.f)
	want := "muster: step-up card #" + itoa(settled.ID) + ", which this session's plugin_draft_decide call raised, was settled when you approved the draft it is about on another door; " +
		"the call itself was not run. Read the draft with the list tools."
	if got := s.m.taken(); len(got) != 1 || got[0].text != want {
		t.Fatalf("the session took %+v; want exactly %q", got, want)
	}
}

// TestAStepUpCardRaisedByAnotherSessionNeverWakesThisOne: a card raised by a
// session the window does not run, and another owner's card, paste nothing
// here.
func TestAStepUpCardRaisedByAnotherSessionNeverWakesThisOne(t *testing.T) {
	s := newStepUpWorld(t)
	other := windowCaller()
	other.asker.MirrorSession = wakeOtherSID
	s.m.set(func(m *wakeMirror) { m.sid = wakeOtherSID })
	card := s.card(t, other)
	s.m.set(func(m *wakeMirror) { m.sid = routeSID })
	if _, err := s.f.srv.answerAskFrom(asDevice(s.f.owner), card, hostDecisionRefuse, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	probeTick(t, s.f)
	if got := s.m.taken(); len(got) != 0 {
		t.Fatalf("another session's card woke this one: %+v", got)
	}
	ws, err := sessionwake.NewPG(s.f.pool).ForAsks(probeOwner(s.f), []int64{card})
	if err != nil || ws[card].Session != wakeOtherSID {
		t.Fatalf("the card's wake: %+v %v; want it bound to %s", ws[card], err, wakeOtherSID)
	}
	stranger := s.f.newMember(t)
	sctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: stranger, Role: authz.RoleMember, Via: authz.ViaSession})
	if _, err := sessionwake.NewPG(s.f.pool).Get(sctx, ws[card].ID); !errors.Is(err, sessionwake.ErrNotFound) {
		t.Fatalf("another owner read the card's wake: %v", err)
	}
}

// TestRenderWakeRequestKindsReadOnlyTheirParts: every new kind's line, over
// pairwise-distinct fields: what the line names, and nothing it must not
// carry (the owner's text appears only in a rejection, the host and session
// never).
func TestRenderWakeRequestKindsReadOnlyTheirParts(t *testing.T) {
	base := sessionwake.Wake{ID: 7, AskID: 11, DraftID: 13, PageID: 17, ResponseID: 19, OfficerItemID: 29, Count: 3, Component: "plugin_draft_decide",
		OperatorText: "owner-words-31", Host: "host-37", Session: "session-41"}
	cases := []struct {
		kind     sessionwake.Kind
		decision string
		want     string
	}{
		{sessionwake.KindOfficerDraftDecided, sessionwake.DecisionApproved,
			"muster: you approved draft #13, which an officer filed for officer item #29 that this session enqueued. Its plugin sends it next; muster tells this session how that went."},
		{sessionwake.KindOfficerDraftOutcome, sessionwake.DecisionSent,
			"muster: draft #13 (attempt 3 of its retry chain), which an officer filed for officer item #29 that this session enqueued — it was sent."},
		{sessionwake.KindOfficerDraftOutcome, sessionwake.DecisionUnconfirmed,
			"muster: draft #13 (attempt 3 of its retry chain), which an officer filed for officer item #29 that this session enqueued — it may or may not have been sent (its plugin did not confirm it). Nothing more is sent on its own."},
		{sessionwake.KindStepUpDecided, sessionwake.DecisionSettledRejected,
			"muster: step-up card #11, which this session's plugin_draft_decide call raised, was settled when you rejected the draft it is about on another door; the call itself was not run. Read the draft with the list tools."},
	}
	for _, c := range cases {
		w := base
		w.Kind, w.Decision = c.kind, c.decision
		if got := renderWake(w); got != c.want {
			t.Errorf("%s/%s:\n%q\nwant\n%q", c.kind, c.decision, got, c.want)
		}
	}
	w := base
	w.Kind, w.Decision, w.Count = sessionwake.KindOfficerDraftOutcome, sessionwake.DecisionFailed, 1
	if got := renderWake(w); got != "muster: draft #13, which an officer filed for officer item #29 that this session enqueued — it could not be sent (its plugin reported a failure). Nothing more is sent on its own." {
		t.Errorf("a first attempt's failure: %q", got)
	}
	w = base
	w.Kind, w.Decision, w.ViaHost, w.ViaSession = sessionwake.KindOfficerDraftDecided, sessionwake.DecisionRejected, "via-43", "via-session-47"
	got := renderWake(w)
	if !strings.HasPrefix(got, "muster: draft #13, which an officer filed for officer item #29 that this session enqueued, was rejected — via plugin_draft_decide from via-43/via-session-47, not by you on the Asks page. It will not be sent, and the officer will not redraft it: officer item #29 is closed, and what happens next is this session's call. The block below is the reason given, relayed by muster.") ||
		!strings.Contains(got, "|   owner-words-31\n") {
		t.Errorf("a rejection made from another session:\n%s", got)
	}
	for _, k := range []sessionwake.Kind{sessionwake.KindOfficerDraftOutcome, sessionwake.KindStepUpDecided} {
		w := base
		w.Kind, w.Decision = k, sessionwake.DecisionSent
		if k == sessionwake.KindStepUpDecided {
			w.Decision = sessionwake.DecisionConfirmed
		}
		if got := renderWake(w); strings.Contains(got, "owner-words-31") || strings.Contains(got, "host-37") || strings.Contains(got, "session-41") || got == "" {
			t.Errorf("%s carries text that is not its own (or none):\n%s", k, got)
		}
	}
}

// TestAnExpectSessionMismatchFilesNoStepUpCard (round-2 audit of #998): on a
// step-up machine, an officer_item_enqueue whose expect_session is not the
// bound caller is refused at once — no card the owner could only see fail —
// so its CLI falls back as on an uncovered machine. Naming the bound session,
// the call is a card as before.
func TestAnExpectSessionMismatchFilesNoStepUpCard(t *testing.T) {
	s := newStepUpWorld(t)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: s.f.owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
	cards := func() int {
		var n int
		if err := s.f.pool.QueryRow(authz.System(ctx), `SELECT count(*) FROM attention_asks WHERE owner_id = $1 AND step_up IS NOT NULL`, s.f.owner).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	args := func(expect string) string {
		return `{"officer":"marketing:a","kind":"owner.plan","subject_key":"plan-expect","expect_session":"` + expect + `"}`
	}
	out, gated := s.f.srv.hostStepUpGate(ctx, "tower", windowCaller(), officerItemEnqueueTool, args(wakeOtherSID))
	want := toolErr("this call is bound to Claude Code session %s (the one muster-mirror lists in the calling pane), not to %s, "+
		"the one expect_session names: nothing was queued or recorded", routeSID, strconv.Quote(wakeOtherSID))
	if !gated || out != want {
		t.Fatalf("a mismatched expect_session on a step-up machine: gated %v, %s; want refused %s", gated, out, want)
	}
	if n := cards(); n != 0 {
		t.Fatalf("%d step-up cards filed for a call that could only be refused", n)
	}
	out, gated = s.f.srv.hostStepUpGate(ctx, "tower", windowCaller(), officerItemEnqueueTool, args(routeSID))
	if !gated {
		t.Fatal("fixture: the step-up gate does not cover tower")
	}
	stepUpCardOf(t, out)
	if n := cards(); n != 1 {
		t.Fatalf("%d step-up cards for the matching call; want 1", n)
	}
}
