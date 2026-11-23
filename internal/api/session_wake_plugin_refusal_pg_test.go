package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/sessionwake"
)

// A PLUGIN'S REFUSAL OF WORK A SESSION ASKED FOR GOES TO THAT SESSION
// (operator decision 2026-11-23; wakeOfficerDraftRefused, migration 0183): the
// officer's draft for an item a session enqueued is refused by its plugin's
// pre-approval check. The session is told why, the officer files no redraft,
// and a line that is never delivered hands the refusal back to the officer
// once. Before this change the refusal reached no session and the officer
// always redrafted.

const pluginRefusalReason = "posts_per_day: 3 of 3 posts already sent today"

// pluginRefusalSummary is the summary of an item closed because its plugin's
// refusal went to the session that enqueued it.
func pluginRefusalSummary(draft int64) string {
	return "draft " + itoa(draft) + " was refused by the plugin, not posted. The plugin's refusal was sent to the session that enqueued this item, " +
		"which decides what happens next: there is nothing to redraft. Should muster be unable to deliver it, it comes back to this officer " +
		"as a draft.refused item."
}

// refuseAtCheck has the officer draft body for item and its plugin's check
// refuse it with pluginRefusalReason: the draft's id.
func (w *officerWakeWorld) refuseAtCheck(item int64, body string) int64 {
	w.t.Helper()
	id := w.unsentDraftForItem(item, body)
	if rec := w.check(w.outboundWorld.tok, id, false, pluginRefusalReason); rec.Code != http.StatusOK {
		w.t.Fatalf("refuse: %d %s", rec.Code, rec.Body.String())
	}
	return id
}

// TestAPluginRefusalOfAnItemASessionEnqueuedGoesOnlyToThatSession: the session
// takes ONE line saying the plugin refused it, with the plugin's reason fenced
// as the plugin's words; the officer gets no draft.refused item and nothing is
// left open; the item is finished saying where the refusal went.
func TestAPluginRefusalOfAnItemASessionEnqueuedGoesOnlyToThatSession(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-plugin-refused")
	const body = "A media-less post its plugin refuses."
	id := w.refuseAtCheck(item, body)
	w.tick()
	if f, _, ok := w.refusedItemOf(id); ok {
		t.Fatalf("the officer got draft.refused item %d for a refusal its requesting session was told", f)
	}
	it := w.itemFull(item)
	if it.status != "done" || it.outcome != "no_action" || it.summary != pluginRefusalSummary(id) {
		t.Fatalf("the item is %s/%s %q; want done/no_action %q", it.status, it.outcome, it.summary, pluginRefusalSummary(id))
	}
	if n := w.openItems(); n != 0 {
		t.Fatalf("%d officer items are still open; want 0", n)
	}
	got := w.pastes()
	if len(got) != 1 {
		t.Fatalf("the session took %d pastes; want exactly 1: %q", len(got), got)
	}
	pre := "muster: the plugin refused " + officerRef(id, item) + ", at its check, before the owner saw it. It will not be sent, and the officer will not redraft it: " +
		"officer item #" + itoa(item) + " is closed, and what happens next is this session's call. The block below is the plugin's reason " +
		"(the plugin's words, not the owner's), relayed by muster.\n\n<<<plugin-reason untrusted nonce="
	if !strings.HasPrefix(got[0], pre) || !strings.Contains(got[0], "\n| plugin reason:\n|   "+pluginRefusalReason+"\n<<<end plugin-reason nonce=") {
		t.Fatalf("the session's line:\n%s\nwant it to start:\n%s\nand carry the plugin's reason fenced", got[0], pre)
	}
	if strings.Contains(got[0], body) || strings.Contains(got[0], "you rejected") {
		t.Fatalf("the line carries the draft's body or calls it the owner's rejection:\n%s", got[0])
	}
	w.tick()
	if n := len(w.pastes()); n != 1 {
		t.Fatalf("more passes pasted again: %d, want 1", n)
	}
}

// TestAPluginRefusalTheSessionNeverReceivesGoesBackToTheOfficerOnce: the
// refusal's wake is filed, but no window shows the session, and a day later it
// settles "Not delivered". The refusal then goes to the officer as the
// draft.refused item it would have had (by the plugin, its reason, the body) —
// once, however many passes follow — and the owner reads that it went there.
func TestAPluginRefusalTheSessionNeverReceivesGoesBackToTheOfficerOnce(t *testing.T) {
	w := newOfficerWakeWorld(t)
	item := w.sessionEnqueues("plan-plugin-refused-never-delivered")
	const body = "A post whose asker has gone."
	w.host.set(func(m *wakeMirror) { m.gone = true })
	id := w.refuseAtCheck(item, body)
	w.tick()
	if ws := w.officerWakesOf(id); len(ws) != 1 || ws[0].Decision != "plugin_refused" || ws[0].State != sessionwake.Queued {
		t.Fatalf("with no window the refusal's wake is %+v; want one plugin_refused, queued", ws)
	}
	if f, _, ok := w.refusedItemOf(id); ok {
		t.Fatalf("draft.refused item %d filed while the refusal may still reach the session", f)
	}
	w.srv.sendQueueClock = func() time.Time { return time.Now().Add(wakeGiveUp + time.Minute) }
	t.Cleanup(func() { w.srv.sendQueueClock = nil })
	w.tick()
	follow, p, ok := w.refusedItemOf(id)
	if ok {
		if h, s := w.itemRequester(follow); h != "" || s != "" {
			t.Fatalf("the handed-back follow-up %d names %q/%q; want no session (it was not reached)", follow, h, s)
		}
	}
	if !ok || p["by"] != "plugin" || p["detail"] != pluginRefusalReason || p["body"] != body || p["status"] != "rejected" {
		t.Fatalf("the undelivered refusal did not go back to the officer as a plugin refusal (follow-up %v, payload %v)", ok, p)
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
	text, act := wakeDeliveryText(w.officerWakeOf(id), "the session")
	if want := "Not delivered: no window showed the session within a day (" + wakeWaitNoWindow + "). The refusal went back to the officer instead."; text != want || act != (wakeAction{}) {
		t.Fatalf("the owner reads %q offering %+v; want %q offering nothing", text, act, want)
	}
	if got := w.pastes(); len(got) != 0 {
		t.Fatalf("the session took %q with no window", got)
	}
}

// TestAPluginRefusalOfAnItemNoSessionEnqueuedStillGoesToTheOfficer (an
// invariant guard, not regression coverage: this path did not change): with no
// requesting session the plugin's refusal is the officer's draft.refused item,
// with its reason, and no session is woken.
func TestAPluginRefusalOfAnItemNoSessionEnqueuedStillGoesToTheOfficer(t *testing.T) {
	w := newOfficerWakeWorld(t)
	code, res := adminCall(t, w.h, "POST", "/api/officers/defs/"+builtinOfficerID+"/items", `{"kind":"owner.plan","subject_key":"plan-plugin-refused-officer"}`)
	f, _ := res["id"].(float64)
	if code != http.StatusCreated || f == 0 {
		t.Fatalf("the admin API's enqueue: %d %v", code, res)
	}
	id := w.refuseAtCheck(int64(f), "A post no session asked for.")
	w.tick()
	if _, p, ok := w.refusedItemOf(id); !ok || p["by"] != "plugin" || p["detail"] != pluginRefusalReason {
		t.Fatalf("no plugin-refusal draft.refused item for an item no session asked for (follow-up %v, payload %v)", ok, p)
	}
	if n := w.wakeRows(sessionwake.KindOfficerDraftDecided); n != 0 {
		t.Fatalf("%d officer wakes filed for an item no session asked for; want 0", n)
	}
	if got := w.pastes(); len(got) != 0 {
		t.Fatalf("a session was woken: %q", got)
	}
}

// TestAPluginRefusedLineIsThePluginsWordsNotTheOwners: the line never says the
// owner rejected it, fences the reason under the plugin's marker, and asks no
// sign-in for it (it carries nothing of the owner's).
func TestAPluginRefusedLineIsThePluginsWordsNotTheOwners(t *testing.T) {
	w := sessionwake.Wake{Kind: sessionwake.KindOfficerDraftDecided, Decision: "plugin_refused", DraftID: 13, OfficerItemID: 29,
		OperatorText: "plugin-words-31", Host: "host-37", Session: "session-41"}
	got := renderWake(w)
	if !strings.HasPrefix(got, "muster: the plugin refused draft #13, which an officer filed for officer item #29 that this session enqueued, at its check") ||
		!strings.Contains(got, "<<<plugin-reason untrusted nonce=") || !strings.Contains(got, "|   plugin-words-31\n") ||
		strings.Contains(got, "owner-reason") || strings.Contains(got, "you rejected") || strings.Contains(got, "host-37") || strings.Contains(got, "session-41") {
		t.Fatalf("the plugin_refused line:\n%s", got)
	}
	if wakeCarriesOwnerText(w) || wakeNeedsSignIn(w, time.Now()) {
		t.Fatal("a plugin's reason was treated as the owner's words (it would wait for a sign-in no plugin call has)")
	}
	w.OperatorText = ""
	if got := renderWake(w); !strings.HasPrefix(got, "muster: the plugin refused draft #13") || strings.Contains(got, "nonce=") {
		t.Fatalf("a refusal with no reason:\n%s", got)
	}
}
