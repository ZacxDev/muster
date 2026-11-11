package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// CHECKS REPLIES ON AN EXISTING PLUGIN (POST /plugins/{id}/draft-checks,
// plugins.PGStore.SetDraftChecks): the owner turns draft_checks on or off for
// a live plugin from /plugins, keeping the SAME token and the same row — its
// routing rules, chief session and queued messages — rather than minting a new
// plugin. Only the owner may; only beside messages; and turning it off lapses
// the drafts that were awaiting a check, so none is stranded.

// toggleChecks posts the /plugins card's Checks replies form as the browser's
// current user (sessionUserOverride), with on "1" or "0".
func (f *refusalFixture) toggleChecks(t *testing.T, id int64, on string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, http.MethodPost, "/plugins/"+strconv.FormatInt(id, 10)+"/draft-checks",
		strings.NewReader(url.Values{"on": {on}}.Encode()), map[string]string{
			"Cookie": sessionCookieFor(f.srv), "Origin": "http://example.com", "Content-Type": "application/x-www-form-urlencoded",
			"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate"})
}

// pluginRow is plugin id's capabilities and chief session, read straight from
// the row (system scope), and whether it is live.
func (f *refusalFixture) pluginRow(t *testing.T, id int64) (caps []string, chiefSession *int64, live bool) {
	t.Helper()
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT capabilities, chief_session_id, revoked_at IS NULL
		FROM plugins WHERE id = $1`, id).Scan(&caps, &chiefSession, &live); err != nil {
		t.Fatal(err)
	}
	return caps, chiefSession, live
}

// TestTheOwnerTurnsChecksRepliesOnAnExistingPlugin: the plugin's own token is
// refused on GET /plugin/v1/drafts/checks before (403), the owner turns Checks
// replies on from /plugins, and the SAME token is then answered 200. The
// plugin is the same row: its routing rule, chief session and queued message
// are untouched, and the change is audited with the old and new sets. Another
// owner cannot turn it off (404, nothing changes); turning it on again is a
// no-op with no second audit row.
func TestTheOwnerTurnsChecksRepliesOnAnExistingPlugin(t *testing.T) {
	f := newRefusalFixture(t)
	pid := f.plugin.ID
	_, delivered := f.pluginMessage(t, "example-inbox", "ct-rule-src", "someone@example.test", true)
	rule, err := f.plugins.CreateRuleProposal(f.asOwner, plugins.RuleNew{PluginID: pid, PluginName: f.plugin.Name,
		Match: plugins.RuleMatch{Sender: "ct@example.test"}, Action: plugins.RuleSettle, SettleReason: "ct rule",
		ChiefAgent: f.chief.ID, SourceMessage: delivered.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.plugins.ApproveRule(f.asOwner, rule.ID); err != nil {
		t.Fatal(err)
	}
	queued, _, err := f.plugins.Submit(f.asOwner, pid, plugins.MessageIn{ExternalID: "ct-queued", Sender: "q@example.test", Body: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	_, sessBefore, _ := f.pluginRow(t, pid)

	if code, _ := f.plug(t, f.plugTok, http.MethodGet, "/plugin/v1/drafts/checks", ""); code != http.StatusForbidden {
		t.Fatalf("control: before the grant the plugin's checks: %d; want 403", code)
	}
	if w := f.toggleChecks(t, pid, "1"); w.Code != http.StatusSeeOther {
		t.Fatalf("the owner turning Checks replies on: %d %s", w.Code, w.Body)
	}
	if code, out := f.plug(t, f.plugTok, http.MethodGet, "/plugin/v1/drafts/checks", ""); code != http.StatusOK || out["drafts"] == nil {
		t.Fatalf("after the grant the SAME token's checks: %d %v; want 200", code, out)
	}
	caps, sessAfter, live := f.pluginRow(t, pid)
	if !live || !slices.Equal(caps, []string{"messages", "draft_checks"}) {
		t.Fatalf("the plugin after the grant: caps %v live %v; want [messages draft_checks], live", caps, live)
	}
	if sessBefore == nil || sessAfter == nil || *sessBefore != *sessAfter {
		t.Fatalf("the chief session moved: %v → %v", sessBefore, sessAfter)
	}
	rules, err := f.plugins.Rules(f.asOwner)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, r := range rules {
		kept = kept || (r.ID == rule.ID && r.PluginID == pid)
	}
	if !kept {
		t.Fatalf("the routing rule is gone or moved after the grant: %+v", rules)
	}
	if m, err := f.plugins.Message(f.asOwner, queued.ID); err != nil || m.Status != plugins.StatusQueued || m.PluginID != pid {
		t.Fatalf("the queued message after the grant: %+v %v", m, err)
	}
	audits := func() (n int, last string) {
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*), coalesce(max(detail::text), '')
			FROM audit_log WHERE action = 'plugin.capability.changed' AND object = $1`, plugins.Object(pid)).Scan(&n, &last); err != nil {
			t.Fatal(err)
		}
		return
	}
	if n, d := audits(); n != 1 || !strings.Contains(d, `"old": ["messages"]`) || !strings.Contains(d, `"new": ["messages", "draft_checks"]`) {
		t.Fatalf("the audit: %d row(s), %s", n, d)
	}

	// Another owner: their session names the owner's plugin, which they cannot see.
	stranger := f.stranger
	f.srv.sessionUserOverride = func(*http.Request) int64 { return stranger }
	if w := f.toggleChecks(t, pid, "0"); w.Code != http.StatusNotFound {
		t.Fatalf("another owner turning it off: %d; want 404", w.Code)
	}
	owner := f.owner
	f.srv.sessionUserOverride = func(*http.Request) int64 { return owner }
	if caps, _, _ := f.pluginRow(t, pid); !slices.Contains(caps, "draft_checks") {
		t.Fatalf("another owner's request changed the plugin: %v", caps)
	}
	if w := f.toggleChecks(t, pid, "1"); w.Code != http.StatusSeeOther {
		t.Fatalf("turning it on again: %d", w.Code)
	}
	if n, _ := audits(); n != 1 {
		t.Fatalf("turning it on again wrote %d audit rows; want still 1", n)
	}
}

// TestAPluginWithoutMessagesCannotGainChecksReplies: a viewer-only plugin is
// refused (400, the mint's rule) and keeps exactly its capabilities.
func TestAPluginWithoutMessagesCannotGainChecksReplies(t *testing.T) {
	f := newRefusalFixture(t)
	_, v, err := f.plugins.CreatePlugin(f.asOwner, plugins.NewPlugin{Name: "ct-viewer", Label: "Example viewer", DataScope: "none",
		Capabilities: []plugins.Capability{plugins.CapViewer}, Origin: "https://*.viewer.example.net", EntryURL: "https://w.viewer.example.net/enter"})
	if err != nil {
		t.Fatal(err)
	}
	w := f.toggleChecks(t, v.ID, "1")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "draft_checks needs messages") {
		t.Fatalf("a viewer-only plugin gaining Checks replies: %d; want 400 saying it needs messages", w.Code)
	}
	if caps, _, _ := f.pluginRow(t, v.ID); !slices.Equal(caps, []string{"viewer"}) {
		t.Fatalf("the viewer plugin's capabilities: %v; want [viewer]", caps)
	}
}

// TestTurningChecksRepliesOffLapsesTheDraftsAwaitingIt: a draft_checks
// plugin's draft awaits its check; the owner turns Checks replies off. The
// draft lapses at once — pending, approvable, an attention item — and the
// owner is pushed once, told the plugin no longer checks replies; the
// plugin's token is refused on the checks again (403); its draft of another
// plugin is untouched.
func TestTurningChecksRepliesOffLapsesTheDraftsAwaitingIt(t *testing.T) {
	f := newRefusalFixture(t)
	ctok, cp := f.checker(t)
	const body = "Awaiting a check that is turned off."
	_, m := f.pluginMessage(t, "example-checker", "ct-off", "someone@example.test", true)
	id, _ := f.answer(t, f.route(t, m), body)
	if d, _ := f.plugins.Draft(f.asOwner, id); !d.AwaitingCheck() {
		t.Fatalf("control: the draft is %s/%s; want awaiting", d.Status, d.CheckState)
	}
	if w := f.toggleChecks(t, cp.ID, "0"); w.Code != http.StatusSeeOther {
		t.Fatalf("turning Checks replies off: %d %s", w.Code, w.Body)
	}
	d, _ := f.plugins.Draft(f.asOwner, id)
	if d.Status != plugins.DraftPending || d.CheckState != plugins.CheckLapsed {
		t.Fatalf("after Checks replies was turned off the draft is %s/%s; want pending, lapsed", d.Status, d.CheckState)
	}
	f.srv.WaitForPushes()
	asked := 0
	for _, p := range f.pusher.to(ep("rf-owner")) {
		if p.Tag == "plugin-draft-"+strconv.FormatInt(id, 10) && p.Title == DraftPushTitle && strings.Contains(p.Body, "no longer checks replies") {
			asked++
		}
	}
	if asked != 1 {
		t.Fatalf("the owner was asked %d time(s); want 1, saying the plugin no longer checks replies", asked)
	}
	if code, _ := f.plug(t, ctok, http.MethodGet, "/plugin/v1/drafts/checks", ""); code != http.StatusForbidden {
		t.Fatalf("the plugin's checks after Checks replies was turned off: %d; want 403", code)
	}
	if text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body)); isErr || !strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("approving the lapsed draft: %s", text)
	}
}
