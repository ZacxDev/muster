package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// A REPLY THE PLUGIN REFUSED, through the real server against a real Postgres
// (plugin_drafts.go draftRefused, session_routes.go redraftRoute,
// plugins.refuseRouteDraftTx): a routed session's reply, approved by the owner
// over plugin_draft_decide, claimed by the plugin and reported ok=false — and,
// for a plugin minted with draft_checks, refused by its check before the owner
// may approve it. What must follow: the owner is pushed on the draft's tag with
// the plugin's reason, the reply goes back to the session that wrote it (a new
// route whose paste carries the reason) at most plugins.MaxRouteRedrafts
// times and then to the chief, and plugin_messages_list stops calling the
// message relayed.
//
// Every refusal detail below is a distinct invented rule name, none of which
// any muster constant spells, so an assertion that finds one found THAT
// refusal's text carried through, not a fixed sentence.

// refusalFixture is draftToolFixture with the owner's push recorded and the
// plugin's chief session set (where muster's notes to the chief land).
type refusalFixture struct {
	*draftToolFixture
	pusher *payloadPusher
	sess   agents.ChatSession
}

func newRefusalFixture(t *testing.T) *refusalFixture {
	t.Helper()
	f := &refusalFixture{draftToolFixture: newDraftToolFixture(t), pusher: &payloadPusher{}}
	store := &memPush{}
	claim(t, store, f.owner, ep("rf-owner"))
	f.srv.ext.Push = push.NewService(store, f.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	var err error
	if f.sess, err = f.srv.ext.Agents.CreateSession(f.asOwner, f.chief.ID, f.chief.Name); err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(f.asOwner, f.plugin.ID, f.chief.ID, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

// checker mints a plugin with messages AND draft_checks, sets its chief
// session, and returns its token.
func (f *refusalFixture) checker(t *testing.T) (string, plugins.Plugin) {
	t.Helper()
	tok, p, err := f.plugins.CreatePlugin(f.asOwner, plugins.NewPlugin{Name: "example-checker", Label: "Example checker",
		DataScope: "example-checker-data", Capabilities: []plugins.Capability{plugins.CapMessages, plugins.CapDraftChecks}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(f.asOwner, p.ID, f.chief.ID, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	return tok, p
}

// answer is route rid answered by its session (route_reply as routeSID): the
// draft it became, and that draft's state as route_reply reported it.
func (f *refusalFixture) answer(t *testing.T, rid, body string) (int64, string) {
	t.Helper()
	text, isErr := f.routeReplyCall(t, f.tok, routeSID, map[string]any{"route_id": rid, "body": body})
	var out struct {
		DraftID     int64  `json:"draft_id"`
		DraftStatus string `json:"draft_status"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil || out.DraftID == 0 {
		t.Fatalf("route_reply on %s: %s", rid, text)
	}
	return out.DraftID, out.DraftStatus
}

// plug is one request on the plugin door with token tok.
func (f *refusalFixture) plug(t *testing.T, tok, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := f.call(t, method, path, strings.NewReader(body), map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// approveClaimFail approves draft id over plugin_draft_decide, has plugin tok
// claim it, and reports the send failed with detail.
func (f *refusalFixture) approveClaimFail(t *testing.T, tok string, id int64, body, detail string) {
	t.Helper()
	if text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body)); isErr || !strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("approve draft %d: %s", id, text)
	}
	code, c := f.plug(t, tok, http.MethodPost, "/plugin/v1/drafts/claim", `{"wait_seconds":0}`)
	if code != http.StatusOK || int64(c["draft_id"].(float64)) != id {
		t.Fatalf("claim: %d %v; want draft %d", code, c, id)
	}
	res, _ := json.Marshal(map[string]any{"ok": false, "detail": detail})
	if code, out := f.plug(t, tok, http.MethodPost, "/plugin/v1/drafts/"+strconv.FormatInt(id, 10)+"/result", string(res)); code != http.StatusOK || out["status"] != "failed" {
		t.Fatalf("result ok=false: %d %v", code, out)
	}
}

// refusalPushes is every `plugin` push to the owner on draft id's tag whose
// title is the refusal's.
func (f *refusalFixture) refusalPushes(id int64) []push.Payload {
	f.srv.WaitForPushes()
	var out []push.Payload
	for _, p := range f.pusher.to(ep("rf-owner")) {
		if p.Type == "plugin" && p.Tag == "plugin-draft-"+strconv.FormatInt(id, 10) && p.Title == DraftRefusedPushTitle {
			out = append(out, p)
		}
	}
	return out
}

// routesOf is message mid's routes, oldest first.
func (f *refusalFixture) routesOf(t *testing.T, mid int64) []plugins.Route {
	t.Helper()
	rows, err := f.pool.Query(f.asOwner, `SELECT route_id FROM session_routes WHERE plugin_message_id = $1 ORDER BY id`, mid)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []plugins.Route
	for _, id := range ids {
		r, err := f.plugins.Route(f.asOwner, id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// listedState is plugin_messages_list's route state for message mid.
func (f *refusalFixture) listedState(t *testing.T, mid int64) string {
	t.Helper()
	text, isErr := f.mcp(t, f.tok, routeSID, pluginMessagesListTool, map[string]any{})
	var out struct {
		Messages []struct {
			ID    int64 `json:"message_id"`
			Route struct {
				State string `json:"state"`
			} `json:"route"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil {
		t.Fatalf("plugin_messages_list: %s", text)
	}
	for _, m := range out.Messages {
		if m.ID == mid {
			return m.Route.State
		}
	}
	t.Fatalf("plugin_messages_list does not list message %d: %s", mid, text)
	return ""
}

// chiefNotes is the chief session's rows that contain want.
func (f *refusalFixture) chiefNotes(t *testing.T, want string) []string {
	t.Helper()
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		if strings.Contains(m.Content, want) {
			out = append(out, m.Content)
		}
	}
	return out
}

// sweep is one route sweep (where an owed hand-back is delivered).
func (f *refusalFixture) sweep(t *testing.T) {
	t.Helper()
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// lastPaste is the newest send-queue row's body for the routed window.
func (f *refusalFixture) lastPaste(t *testing.T) (int, string) {
	t.Helper()
	items := f.queued(t)
	if len(items) == 0 {
		return 0, ""
	}
	return len(items), items[len(items)-1].Body
}

// TestARefusedSendGoesToTheOwnerAndBackToTheRoutedSession is the production
// path: route → route_reply (a PENDING fallback draft) → plugin_draft_decide
// approve (whose note must not claim it is sent) → the plugin claims it → POST
// result ok=false with a refusal. Then: one owner push on the draft's tag
// carrying the refusal; the route is send_failed, so plugin_messages_list says
// send_failed, not relayed; the sweep hands the message back to the SAME
// session as a new route whose paste carries the refusal and the refused text;
// the chief is told. A repeated result changes nothing (no second push, no
// second route).
func TestARefusedSendGoesToTheOwnerAndBackToTheRoutedSession(t *testing.T) {
	f := newRefusalFixture(t)
	const body, detail = "First line of the answer.", "not sent (rc 3): rule zebra-seven: line 4 starts with '* '"
	id, rid := f.routedDraft(t, "rf-send", body)
	d, err := f.plugins.Draft(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	mid := d.MessageID
	if s := f.listedState(t, mid); s != listedRouteRelayed {
		t.Fatalf("control: a routed message awaiting approval lists as %q; want %q", s, listedRouteRelayed)
	}

	text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body))
	if isErr || strings.Contains(text, "sends exactly this text") || !strings.Contains(text, "NOT sent yet") {
		t.Fatalf("plugin_draft_decide's approval must say it is queued, not sent: %s", text)
	}
	code, c := f.plug(t, f.plugTok, http.MethodPost, "/plugin/v1/drafts/claim", `{"wait_seconds":0}`)
	if code != http.StatusOK || int64(c["draft_id"].(float64)) != id {
		t.Fatalf("claim: %d %v", code, c)
	}
	before, _ := f.lastPaste(t)
	res, _ := json.Marshal(map[string]any{"ok": false, "detail": detail})
	path := "/plugin/v1/drafts/" + strconv.FormatInt(id, 10) + "/result"
	if code, out := f.plug(t, f.plugTok, http.MethodPost, path, string(res)); code != http.StatusOK || out["status"] != "failed" {
		t.Fatalf("result: %d %v", code, out)
	}

	ps := f.refusalPushes(id)
	if len(ps) != 1 || !strings.Contains(ps[0].Body, "rule zebra-seven") || !strings.Contains(ps[0].Body, "session that wrote it") {
		t.Fatalf("the owner's push for the refused draft: %+v; want one on its tag carrying the refusal", ps)
	}
	r, err := f.plugins.Route(f.asOwner, rid)
	if err != nil || r.State != plugins.RouteSendFailed || !r.Redraft {
		t.Fatalf("the route after the refusal: %s redraft=%v (%v); want send_failed, redraft", r.State, r.Redraft, err)
	}
	if s := f.listedState(t, mid); s != listedRouteSendFailed {
		t.Fatalf("plugin_messages_list after the refusal: %q; want %q (not relayed)", s, listedRouteSendFailed)
	}

	f.sweep(t)
	routes := f.routesOf(t, mid)
	if len(routes) != 2 || routes[1].State != plugins.RouteRouted || routes[1].TargetKey != routeSID {
		t.Fatalf("after the sweep the message's routes are %+v; want a second, routed to the same session", routes)
	}
	n, paste := f.lastPaste(t)
	if n != before+1 || !strings.Contains(paste, "route_id "+routes[1].RouteID) || !strings.Contains(paste, "rule zebra-seven") ||
		!strings.Contains(paste, "your earlier reply was: "+body) || !strings.Contains(paste, "WAS NOT SENT") {
		t.Fatalf("the hand-back paste (%d rows, was %d):\n%s", n, before, paste)
	}
	if notes := f.chiefNotes(t, "rule zebra-seven"); len(notes) != 1 || !strings.Contains(notes[0], routes[1].RouteID) {
		t.Fatalf("the chief's note: %v; want one naming the new route", notes)
	}
	if s := f.listedState(t, mid); s != listedRoutePending {
		t.Fatalf("plugin_messages_list once handed back: %q; want %q", s, listedRoutePending)
	}

	// The same result again: recorded once, so nothing follows twice.
	if code, _ := f.plug(t, f.plugTok, http.MethodPost, path, string(res)); code != http.StatusOK {
		t.Fatalf("the same result again: %d", code)
	}
	f.sweep(t)
	if ps := f.refusalPushes(id); len(ps) != 1 {
		t.Fatalf("a repeated result pushed again: %d pushes", len(ps))
	}
	if routes := f.routesOf(t, mid); len(routes) != 2 {
		t.Fatalf("a repeated result rerouted again: %d routes", len(routes))
	}
}

// TestARefusedReplyGoesBackToItsSessionAtMostMaxRouteRedraftsTimes: each
// refusal of the message's reply hands it back to the session, until the one
// after plugins.MaxRouteRedrafts of them, which goes to the CHIEF (a hand-back
// in its session saying the message is back with it) and makes no route — so
// a reply that keeps being refused cannot loop. Each round's refusal is its
// own text, and the chief's hand-back carries the LAST one.
func TestARefusedReplyGoesBackToItsSessionAtMostMaxRouteRedraftsTimes(t *testing.T) {
	f := newRefusalFixture(t)
	_, m := f.pluginMessage(t, "example-inbox", "rf-bound", "someone@example.test", true)
	rid := f.route(t, m)
	rounds := plugins.MaxRouteRedrafts + 1
	for i := 0; i < rounds; i++ {
		body := "Answer number " + strconv.Itoa(i+1) + " of the bound."
		detail := "rule wombat-" + strconv.Itoa(31+i) + " refused it"
		id, st := f.answer(t, rid, body)
		if st != "pending" {
			t.Fatalf("round %d: the answer became a %s draft; want pending", i, st)
		}
		f.approveClaimFail(t, f.plugTok, id, body, detail)
		f.sweep(t)
		routes := f.routesOf(t, m.ID)
		last := i == rounds-1
		switch {
		case !last && len(routes) != i+2:
			t.Fatalf("round %d: %d routes; want %d (handed back to the session)", i, len(routes), i+2)
		case last && len(routes) != i+1:
			t.Fatalf("round %d (past the bound): %d routes; want %d — no further hand-back to the session", i, len(routes), i+1)
		}
		if failed := routes[i]; failed.State != plugins.RouteSendFailed || failed.Redraft == last {
			t.Fatalf("round %d: the refused route is %s redraft=%v; want send_failed, redraft=%v", i, failed.State, failed.Redraft, !last)
		}
		backs := f.chiefNotes(t, "back with you")
		if !last {
			if len(backs) != 0 {
				t.Fatalf("round %d: the chief was handed the message back while the session still may redraft: %v", i, backs)
			}
			rid = routes[i+1].RouteID
			continue
		}
		if len(backs) != 1 || !strings.Contains(backs[0], detail) || !strings.Contains(backs[0], routes[i].RouteID) {
			t.Fatalf("past the bound the chief's hand-back is %v; want one carrying %q", backs, detail)
		}
		if ps := f.refusalPushes(id); len(ps) != 1 || !strings.Contains(ps[0].Body, "your chief") {
			t.Fatalf("past the bound the owner's push is %+v; want it to say it went to the chief", ps)
		}
		if s := f.listedState(t, m.ID); s != listedRouteSendFailed {
			t.Fatalf("past the bound plugin_messages_list says %q; want %q", s, listedRouteSendFailed)
		}
	}
}

// TestARefusedReplyFallsToTheChiefWhenItsSessionIsGone: the session the
// message was routed to no longer shows in any window, so the hand-back cannot
// go there — it goes to the chief, as a timed-out route's does, saying why.
func TestARefusedReplyFallsToTheChiefWhenItsSessionIsGone(t *testing.T) {
	f := newRefusalFixture(t)
	const body = "An answer for a session that leaves."
	id, rid := f.routedDraft(t, "rf-gone", body)
	f.approveClaimFail(t, f.plugTok, id, body, "rule numbat-5 refused it")
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "5e55a1d0-0000-4000-8000-0000000000ff" })
	f.sweep(t)
	r, _ := f.plugins.Route(f.asOwner, rid)
	if routes := f.routesOf(t, r.MessageID); len(routes) != 1 {
		t.Fatalf("a hand-back to a gone session made a route: %+v", routes)
	}
	backs := f.chiefNotes(t, "back with you")
	if len(backs) != 1 || !strings.Contains(backs[0], "rule numbat-5") || !strings.Contains(backs[0], "could not hand it back to the session") {
		t.Fatalf("the chief's hand-back: %v", backs)
	}
}

// TestARefusedChiefDraftIsNotedToTheChief: a draft the chief filed itself
// (reply_draft, no route) that the plugin reports failed — the owner is pushed
// with the refusal, and the chief is told in its plugin session, with the
// refusal, so it can draft again.
func TestARefusedChiefDraftIsNotedToTheChief(t *testing.T) {
	f := newRefusalFixture(t)
	const body = "The chief's own reply."
	id := f.draft(t, "rf-chief", "Ada <ada@example.invalid>", true, body)
	f.approveClaimFail(t, f.plugTok, id, body, "upstream refused: HTTP 422 rule kakapo-12")
	if ps := f.refusalPushes(id); len(ps) != 1 || !strings.Contains(ps[0].Body, "rule kakapo-12") || !strings.Contains(ps[0].Body, "your chief") {
		t.Fatalf("the owner's push: %+v", ps)
	}
	notes := f.chiefNotes(t, "rule kakapo-12")
	if len(notes) != 1 || !strings.Contains(notes[0], "reply draft "+strconv.FormatInt(id, 10)) || !strings.Contains(notes[0], "reply_draft") {
		t.Fatalf("the chief's note: %v", notes)
	}
}

// TestAPluginWithDraftChecksHoldsTheApprovalUntilItsVerdict is the
// pre-approval check: a routed reply to a message of a draft_checks plugin
// awaits the plugin's verdict — no push to the owner, listed as
// awaiting_plugin_check, and an approval is REFUSED with a clear sentence. The
// plugin lists it on GET /plugin/v1/drafts/checks and refuses it: the draft
// is rejected, approving it is refused naming the refusal, the route is
// send_failed and the sweep hands it back to the session with the reason.
// CONTROL: a second draft the plugin says is sendable gets the owner's push
// and is approved.
func TestAPluginWithDraftChecksHoldsTheApprovalUntilItsVerdict(t *testing.T) {
	f := newRefusalFixture(t)
	ctok, cp := f.checker(t)
	const body = "- a list the chat cannot carry"
	_, m := f.pluginMessage(t, "example-checker", "rf-check", "someone@example.test", true)
	rid := f.route(t, m)
	id, st := f.answer(t, rid, body)
	if st != "pending" {
		t.Fatalf("the answer became %s; want pending", st)
	}
	f.srv.WaitForPushes()
	for _, p := range f.pusher.to(ep("rf-owner")) {
		if p.Tag == "plugin-draft-"+strconv.FormatInt(id, 10) {
			t.Fatalf("the owner was pushed about a draft its plugin has not checked: %+v", p)
		}
	}
	if d := listedByID(f.listed(t, f.tok, otherRouteSID), id); d == nil || d["awaiting_plugin_check"] != true {
		t.Fatalf("plugin_drafts_list: %v; want it marked awaiting_plugin_check", d)
	}
	items, err := f.srv.pendingDraftItems(f.asOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Key == draftKey(id) {
			t.Fatalf("an unchecked draft is an attention item: %+v", it)
		}
	}
	if text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body)); !isErr || text != toolErr("draft %d: %s", id, DraftUncheckedRefusal) {
		t.Fatalf("approving an unchecked draft: %s", text)
	}
	if r := f.row(t, id); r.status != "pending" {
		t.Fatalf("after the refused approval the draft is %+v", r)
	}
	// Another plugin's token (no draft_checks) cannot reach the checks.
	if code, _ := f.plug(t, f.plugTok, http.MethodGet, "/plugin/v1/drafts/checks", ""); code != http.StatusForbidden {
		t.Fatalf("a messages-only token on the checks: %d; want 403", code)
	}
	code, out := f.plug(t, ctok, http.MethodGet, "/plugin/v1/drafts/checks", "")
	ds, _ := out["drafts"].([]any)
	if code != http.StatusOK || len(ds) != 1 || int64(ds[0].(map[string]any)["draft_id"].(float64)) != id ||
		ds[0].(map[string]any)["body"] != body || ds[0].(map[string]any)["message_external_id"] != "rf-check" {
		t.Fatalf("GET checks: %d %v", code, out)
	}
	check := "/plugin/v1/drafts/" + strconv.FormatInt(id, 10) + "/check"
	// Another draft_checks plugin of the SAME owner cannot decide it.
	other, _, err := f.plugins.CreatePlugin(f.asOwner, plugins.NewPlugin{Name: "example-checker-two", Label: "Example checker two",
		DataScope: "example-checker-data", Capabilities: []plugins.Capability{plugins.CapMessages, plugins.CapDraftChecks}})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.plug(t, other, http.MethodPost, check, `{"sendable":true}`); code != http.StatusNotFound {
		t.Fatalf("another plugin's verdict on the draft: %d; want 404", code)
	}
	if code, out := f.plug(t, other, http.MethodGet, "/plugin/v1/drafts/checks", ""); code != http.StatusOK || len(out["drafts"].([]any)) != 0 {
		t.Fatalf("another plugin's checks list: %d %v; want none", code, out)
	}
	if code, _ := f.plug(t, ctok, http.MethodPost, check, `{"sendable":false}`); code != http.StatusBadRequest {
		t.Fatalf("a refusal with no reason: %d; want 400", code)
	}
	if code, out := f.plug(t, ctok, http.MethodPost, check, `{"sendable":false,"detail":"rule list-quokka: line 1 starts with '- '"}`); code != http.StatusOK ||
		out["status"] != "rejected" || out["check"] != "refused" {
		t.Fatalf("the refusal: %d %v", code, out)
	}
	if code, _ := f.plug(t, ctok, http.MethodPost, check, `{"sendable":true}`); code != http.StatusConflict {
		t.Fatalf("the other verdict after a refusal: %d; want 409", code)
	}
	text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body))
	if !isErr || !strings.Contains(text, "refused that draft before it could be approved") || !strings.Contains(text, "rule list-quokka") {
		t.Fatalf("approving a refused draft: %s", text)
	}
	if _, ok, _ := f.plugins.ClaimDraft(f.asOwner, cp.ID); ok {
		t.Fatal("a draft refused by its check was claimable")
	}
	if r, _ := f.plugins.Route(f.asOwner, rid); r.State != plugins.RouteSendFailed || !r.Redraft {
		t.Fatalf("the route after the check refused its reply: %s redraft=%v", r.State, r.Redraft)
	}
	// The owner never saw it, and it goes back to its session: nothing to push.
	if ps := f.refusalPushes(id); len(ps) != 0 {
		t.Fatalf("a check refusal going back to its session pushed the owner: %+v", ps)
	}
	f.sweep(t)
	routes := f.routesOf(t, m.ID)
	if _, paste := f.lastPaste(t); len(routes) != 2 || routes[1].State != plugins.RouteRouted || !strings.Contains(paste, "rule list-quokka") {
		t.Fatalf("the hand-back after the check's refusal: routes %+v, paste:\n%s", routes, paste)
	}

	// CONTROL: sendable — the owner is asked now, and may approve it.
	const good = "A plain answer the chat can carry."
	id2, _ := f.answer(t, routes[1].RouteID, good)
	if code, out := f.plug(t, ctok, http.MethodPost, "/plugin/v1/drafts/"+strconv.FormatInt(id2, 10)+"/check", `{"sendable":true}`); code != http.StatusOK ||
		out["status"] != "pending" || out["check"] != "sendable" {
		t.Fatalf("the sendable verdict: %d %v", code, out)
	}
	f.srv.WaitForPushes()
	asked := 0
	for _, p := range f.pusher.to(ep("rf-owner")) {
		if p.Tag == "plugin-draft-"+strconv.FormatInt(id2, 10) && p.Title == DraftPushTitle {
			asked++
		}
	}
	if asked != 1 {
		t.Fatalf("after the sendable verdict the owner was asked %d time(s); want 1", asked)
	}
	if text, isErr := f.decide(t, f.tok, otherRouteSID, id2, "approve", sha(good)); isErr || !strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("approving a checked draft: %s", text)
	}
}

// TestAPluginWithoutDraftChecksKeepsTodaysFlow: a messages-only plugin's
// routed reply is pending with no check asked for — the owner is pushed at
// once, it is not marked awaiting a check, nothing is listed for a check, and
// it is approved and claimed as before.
func TestAPluginWithoutDraftChecksKeepsTodaysFlow(t *testing.T) {
	f := newRefusalFixture(t)
	const body = "An answer for an unchecked plugin."
	id, _ := f.routedDraft(t, "rf-legacy", body)
	d, err := f.plugins.Draft(f.asOwner, id)
	if err != nil || d.CheckState != "" {
		t.Fatalf("the draft's check: %q (%v); want none asked", d.CheckState, err)
	}
	f.srv.WaitForPushes()
	asked := 0
	for _, p := range f.pusher.to(ep("rf-owner")) {
		if p.Tag == "plugin-draft-"+strconv.FormatInt(id, 10) && p.Title == DraftPushTitle {
			asked++
		}
	}
	if asked != 1 {
		t.Fatalf("the owner was asked %d time(s) at filing; want 1", asked)
	}
	if l := listedByID(f.listed(t, f.tok, otherRouteSID), id); l == nil || l["awaiting_plugin_check"] != nil {
		t.Fatalf("plugin_drafts_list: %v; want no awaiting_plugin_check", l)
	}
	var awaiting int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM plugin_reply_drafts WHERE id = $1 AND check_state IS NOT NULL`, id).Scan(&awaiting); err != nil || awaiting != 0 {
		t.Fatalf("check_state set on a legacy plugin's draft (%d, %v)", awaiting, err)
	}
	if text, isErr := f.decide(t, f.tok, otherRouteSID, id, "approve", sha(body)); isErr {
		t.Fatalf("approve: %s", text)
	}
	if c, ok, err := f.plugins.ClaimDraft(f.asOwner, f.plugin.ID); err != nil || !ok || c.DraftID != id {
		t.Fatalf("claim: %+v %v %v", c, ok, err)
	}
}
