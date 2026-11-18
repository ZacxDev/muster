package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
)

// The draft_checks toggle and the refusal aftermath, audit round 2: a filing
// racing a turn-off, a revoked plugin, cross-origin posts, an expired draft,
// and the refusal push and notes following the STORE's decision.

// TestTurningChecksRepliesOffRacesNoFilingIntoAnAwaitingDraft (round 2 Y-a):
// the chief files a draft for a draft_checks plugin's message AT THE SAME
// TIME as the owner turns Checks replies off. Whichever commits first, no
// draft may be left awaiting a check from a plugin that no longer checks:
// either the filing saw the capability gone (no check asked), or the turn-off
// saw the filing (lapsed). Each round starts both together behind a barrier.
func TestTurningChecksRepliesOffRacesNoFilingIntoAnAwaitingDraft(t *testing.T) {
	f := newRefusalFixture(t)
	_, cp := f.checker(t)
	const rounds = 400
	stranded := 0
	// One message (a plugin's inbound rate is capped): each round files a new
	// draft for it, and the last one is rejected at the end of its round.
	_, m := f.pluginMessage(t, "example-checker", "r2-race", "someone@example.test", true)
	for i := 0; i < rounds; i++ {
		if _, _, _, err := f.plugins.SetDraftChecks(f.asOwner, cp.ID, true); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var d plugins.Draft
		var ferr, terr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			d, _, ferr = f.plugins.FileDraft(f.asOwner, f.chief.ID, f.owner, f.chief.Name, m.ID, "Race reply "+strconv.Itoa(i)+".")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, _, terr = f.plugins.SetDraftChecks(f.asOwner, cp.ID, false)
		}()
		close(start)
		wg.Wait()
		if ferr != nil || terr != nil {
			t.Fatalf("round %d: file %v, turn off %v", i, ferr, terr)
		}
		got, err := f.plugins.Draft(f.asOwner, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.AwaitingCheck() {
			stranded++
		}
		// Out of the per-agent pending cap's way for the next round.
		if _, err := f.plugins.DecideDraftFrom(f.asOwner, d.ID, plugins.DraftDecision{}); err != nil {
			t.Fatal(err)
		}
	}
	if stranded != 0 {
		t.Fatalf("%d of %d racing filings left a draft awaiting a check from a plugin that no longer checks", stranded, rounds)
	}
}

// TestARevokedPluginsChecksRepliesCannotBeChanged (round 2 G-a): turning
// Checks replies on for a REVOKED plugin is 404, as revoking it again would
// find nothing live, and its capabilities stay as they were.
func TestARevokedPluginsChecksRepliesCannotBeChanged(t *testing.T) {
	f := newRefusalFixture(t)
	if ok, err := f.plugins.Revoke(f.asOwner, f.plugin.ID); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if w := f.toggleChecks(t, f.plugin.ID, "1"); w.Code != http.StatusNotFound {
		t.Fatalf("turning Checks replies on for a revoked plugin: %d; want 404", w.Code)
	}
	if caps, _, live := f.pluginRow(t, f.plugin.ID); live || len(caps) != 1 || caps[0] != "messages" {
		t.Fatalf("the revoked plugin: caps %v live %v; want [messages], revoked", caps, live)
	}
}

// TestChecksRepliesAndRevokeRefuseACrossOriginPost (round 2 G-c): both
// /plugins card forms are same-origin only (requireOperatorWrite): a post from
// another origin, with the owner's own session cookie, is refused 403 and
// changes nothing. CONTROL: the same post from the page's own origin works.
func TestChecksRepliesAndRevokeRefuseACrossOriginPost(t *testing.T) {
	f := newRefusalFixture(t)
	id := strconv.FormatInt(f.plugin.ID, 10)
	post := func(path, origin string, form url.Values) int {
		return f.call(t, http.MethodPost, path, strings.NewReader(form.Encode()), map[string]string{
			"Cookie": sessionCookieFor(f.srv), "Origin": origin, "Content-Type": "application/x-www-form-urlencoded"}).Code
	}
	if code := post("/plugins/"+id+"/draft-checks", "http://evil.example", url.Values{"on": {"1"}}); code != http.StatusForbidden {
		t.Fatalf("a cross-origin Checks replies post: %d; want 403", code)
	}
	if code := post("/plugins/"+id+"/revoke", "http://evil.example", nil); code != http.StatusForbidden {
		t.Fatalf("a cross-origin revoke: %d; want 403", code)
	}
	if caps, _, live := f.pluginRow(t, f.plugin.ID); !live || len(caps) != 1 {
		t.Fatalf("after the cross-origin posts: caps %v live %v; want unchanged", caps, live)
	}
	if code := post("/plugins/"+id+"/draft-checks", "http://example.com", url.Values{"on": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("control: a same-origin Checks replies post: %d; want 303", code)
	}
}

// TestTurningChecksRepliesOffLeavesAnExpiredDraftAlone (round 2 G-d): a draft
// awaiting a check whose 7 days are up (no sweep has marked it expired yet) is
// NOT lapsed by turning Checks replies off — it is past being decided — and
// the owner is not asked about it.
func TestTurningChecksRepliesOffLeavesAnExpiredDraftAlone(t *testing.T) {
	f := newRefusalFixture(t)
	_, cp := f.checker(t)
	_, m := f.pluginMessage(t, "example-checker", "r2-expired", "someone@example.test", true)
	var id int64
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body,
		created_at, expires_at, check_state, check_due_at) VALUES (`+strconv.FormatInt(f.owner, 10)+`, `+strconv.FormatInt(m.ID, 10)+`, `+
		strconv.FormatInt(f.chief.ID, 10)+`, 'chief', 'An expired reply.', now() - interval '8 days', now() - interval '1 day',
		'awaiting', now() + interval '1 hour')`)
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT id FROM plugin_reply_drafts WHERE message_id = $1`, m.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if w := f.toggleChecks(t, cp.ID, "0"); w.Code != http.StatusSeeOther {
		t.Fatalf("turning Checks replies off: %d", w.Code)
	}
	var st string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT coalesce(check_state, '') FROM plugin_reply_drafts WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st == string(plugins.CheckLapsed) {
		t.Fatal("turning Checks replies off lapsed an expired draft")
	}
	f.srv.WaitForPushes()
	for _, p := range f.pusher.to(ep("rf-owner")) {
		if p.Tag == "plugin-draft-"+strconv.FormatInt(id, 10) {
			t.Fatalf("the owner was pushed about an expired draft: %+v", p)
		}
	}
}

// failingReplyOut is the real store whose ReplyOutElsewhere — the API's own
// second read of "another reply went", which the API must no longer make —
// fails.
type failingReplyOut struct{ plugins.Store }

func (failingReplyOut) ReplyOutElsewhere(context.Context, plugins.Draft) (bool, error) {
	return false, errors.New("injected read failure")
}

// TestTheRefusalAftermathFollowsTheStoresDecision (round 2 G-b): what follows a
// refused draft is what the store decided in the transaction that recorded it
// (Draft.Refusal), never a second read: with any later read of "another reply
// went" failing, a routed refusal still pushes "going back to the session"
// and is handed back, and a chief-filed refusal still notes the chief.
func TestTheRefusalAftermathFollowsTheStoresDecision(t *testing.T) {
	f := newRefusalFixture(t)
	f.srv.ext.Plugins = failingReplyOut{Store: f.plugins}
	const body = "A routed reply refused with a failing read."
	id, rid := f.routedDraft(t, "r2-store-decides", body)
	f.approveClaimFail(t, f.plugTok, id, body, "rule okapi-6 refused it")
	if ps := f.refusalPushes(id); len(ps) != 1 || !strings.Contains(ps[0].Body, "going back to the session") {
		t.Fatalf("the routed refusal's push: %+v; want it going back to the session", ps)
	}
	f.sweep(t)
	r, _ := f.plugins.Route(f.asOwner, rid)
	if routes := f.routesOf(t, r.MessageID); len(routes) != 2 {
		t.Fatalf("the routed refusal was not handed back: %d routes", len(routes))
	}
	const chiefBody = "A chief reply refused with a failing read."
	cid := f.draft(t, "r2-store-decides-chief", "Ada <ada@example.invalid>", true, chiefBody)
	f.approveClaimFail(t, f.plugTok, cid, chiefBody, "rule quagga-8 refused it")
	if notes := f.chiefNotes(t, "rule quagga-8"); len(notes) != 1 {
		t.Fatalf("the chief's note for its refused draft: %v; want one", notes)
	}
}
