package api

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/push"
)

// The owner's Attention ask about an officer item a plugin message became
// carries that message, built by muster (officer_ask_context.go). These tests
// read ONLY surfaces that existed before the context did — the Asks page, the
// ask page, attention_list's JSON and the push body — so they run unchanged
// against the code before it, where the message is absent and they fail.

// Fixture message: every field distinct from every other and from every
// constant the assertions name, so a renderer that swaps or hardcodes one is
// seen.
const (
	ctxVenue  = "example.forum"
	ctxThread = "t9_fixture_thread_77"
	ctxSender = "example-asker"
	ctxURL    = "https://example.test/thread/t9_fixture_thread_77"
	ctxLine1  = "Does the queued message still go out"
	ctxLine2  = "if the session stops to ask a question?"
	ctxNote   = "The sender asks about queued messages; I hold no facts on it."
)

// postFixtureMessage posts the fixture reply through the bound plugin and
// answers its item id.
func postFixtureMessage(t *testing.T, w *outboundWorld, externalID string) int64 {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"external_id": externalID, "sender": ctxSender, "body": ctxLine1 + "\n" + ctxLine2,
		"kind": "reply", "venue": ctxVenue, "thread": ctxThread, "url": ctxURL})
	rec := w.do("POST", "/plugin/v1/messages", string(body), bearerTok(w.tok))
	var got struct {
		ItemID int64 `json:"item_id"`
	}
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.ItemID == 0 {
		t.Fatalf("post the fixture message: %d %s", rec.Code, rec.Body.String())
	}
	return got.ItemID
}

// operatorGet is user 1's browser GET of path.
func operatorGet(w *outboundWorld, path string) string {
	rec := w.do("GET", path, "", func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(w.srv))
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
	})
	if rec.Code != http.StatusOK {
		w.t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// checkContextShown asserts every surface of ask a shows the fixture message
// — venue, thread, sender marked unverified, both body lines, the url as text
// — and the push stays short (no outside words on the lock screen).
func checkContextShown(t *testing.T, w *outboundWorld, a attention.Ask, item int64) {
	t.Helper()
	want := []string{ctxVenue, ctxThread, ctxSender, "UNVERIFIED", ctxLine1, ctxLine2, ctxURL}
	page := operatorGet(w, askPagePath(a.ID))
	list := operatorGet(w, attentionPath)
	tools := w.srv.toolAttentionList(w.ctx)
	for name, surface := range map[string]string{"the ask page": page, "the Asks page": list, "attention_list": tools} {
		text := html.UnescapeString(surface)
		for _, s := range want {
			if name == "attention_list" && s == "UNVERIFIED" {
				s = `"sender_verified":false` // the JSON says it as a field
			}
			if !strings.Contains(text, s) {
				t.Errorf("%s does not show %q", name, s)
			}
		}
	}
	// The url is text, never a link.
	for name, surface := range map[string]string{"the ask page": page, "the Asks page": list} {
		if strings.Contains(surface, `href="`+ctxURL) || strings.Contains(surface, `href="`+html.EscapeString(ctxURL)) {
			t.Errorf("%s links the outside url", name)
		}
	}
	push := attentionPushBody(askItem(a))
	if want := "Officer " + builtinOfficerID + " · item " + strconv.FormatInt(item, 10) + " · "; !strings.HasPrefix(push, want) {
		t.Errorf("push body %q, want it to start %q", push, want)
	}
	for _, s := range []string{ctxLine1, ctxLine2, ctxSender, ctxURL} {
		if strings.Contains(push, s) {
			t.Errorf("push body %q carries the outside sender's %q", push, s)
		}
	}
}

// TestAnOfficerEscalationOfAPluginItemShowsItsSourceMessage is the incident:
// an officer escalates a plugin.reply item with only its own prose; the
// owner's ask must still show the reply, where it was and who sent it.
func TestAnOfficerEscalationOfAPluginItemShowsItsSourceMessage(t *testing.T) {
	w := newOutboundWorld(t, true)
	item := postFixtureMessage(t, w, "ctx:esc:1")
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolComplete,
		`{"item_id":"`+strconv.FormatInt(item, 10)+`","outcome":"escalated","summary":"`+ctxNote+`"}`); isToolErr(out) {
		t.Fatalf("complete: %s", out)
	}
	w.srv.SweepOfficerAttention(context.Background())
	asks := officerAsks(t, w.srv)
	if len(asks) != 1 || !strings.Contains(asks[0].Question, ctxNote) {
		t.Fatalf("after the escalation: %+v, want one ask carrying the officer's note", asks)
	}
	checkContextShown(t, w, asks[0], item)
	if push := attentionPushBody(askItem(asks[0])); !strings.HasSuffix(push, " · "+ctxNote) {
		t.Errorf("push body %q, want the note's first line after officer and item", push)
	}
}

// TestAnOfficerAskAboutAPluginItemShowsItsSourceMessage: the same through
// officer_ask_person.
func TestAnOfficerAskAboutAPluginItemShowsItsSourceMessage(t *testing.T) {
	w := newOutboundWorld(t, true)
	item := postFixtureMessage(t, w, "ctx:ask:1")
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolAskPerson,
		`{"item_id":"`+strconv.FormatInt(item, 10)+`","to":"owner","question":"`+ctxNote+`"}`); isToolErr(out) {
		t.Fatalf("ask: %s", out)
	}
	asks := officerAsks(t, w.srv)
	if len(asks) != 1 || !strings.Contains(asks[0].Question, ctxNote) {
		t.Fatalf("after the ask: %+v, want one ask carrying the officer's question", asks)
	}
	checkContextShown(t, w, asks[0], item)
}

// TestAnOfficerEscalationOfAnyOtherItemIsUnchanged (an INVARIANT guard: the
// code before the context never drew one, so it passes there too): an item no plugin message
// became gets the ask it always did — the question alone, the push its one
// line, nothing drawn beside it.
func TestAnOfficerEscalationOfAnyOtherItemIsUnchanged(t *testing.T) {
	w := newOutboundWorld(t, true)
	code, out := adminCall(t, w.h, "POST", "/api/officers/defs/"+builtinOfficerID+"/items",
		`{"kind":"release.post","subject_key":"ctx-v1","payload":{"message_id":41,"plugin_id":`+strconv.FormatInt(w.plugin.ID, 10)+`}}`)
	if code != http.StatusCreated {
		t.Fatalf("enqueue: %d %v", code, out)
	}
	item := strconv.FormatInt(int64(out["id"].(float64)), 10)
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"escalated","summary":"`+ctxNote+`"}`); isToolErr(out) {
		t.Fatalf("complete: %s", out)
	}
	w.srv.SweepOfficerAttention(context.Background())
	asks := officerAsks(t, w.srv)
	if len(asks) != 1 {
		t.Fatalf("after the escalation: %+v, want one ask", asks)
	}
	a := asks[0]
	wantQ := "Officer " + builtinOfficerID + " escalated item " + item + " (release.post ctx-v1) to you:\n\n" + ctxNote + officerEscalationTail
	if a.Question != wantQ {
		t.Fatalf("question %q, want %q", a.Question, wantQ)
	}
	if push, want := attentionPushBody(askItem(a)), push.CutClusters(pushLine(wantQ), attentionPushBodyMax); push != want {
		t.Fatalf("push body %q, want the question's one line %q", push, want)
	}
	page := operatorGet(w, askPagePath(a.ID))
	if strings.Contains(page, "data-officer-") || strings.Contains(w.srv.toolAttentionList(w.ctx), `"context"`) {
		t.Fatal("an ask about an item no plugin message became draws or lists a context")
	}
}

// TestAURLIsABoundPluginsAlone: url is refused (400) on a plugin with no
// officer binding, as kind and venue are, and on a bound one when it is not
// an http(s) URL — nothing stored either time.
func TestAURLIsABoundPluginsAlone(t *testing.T) {
	w := newOutboundWorld(t, true)
	if rec := w.do("POST", "/plugin/v1/messages", `{"external_id":"u1","body":"b","url":"https://example.test/u1"}`, bearerTok(w.outTok)); rec.Code != http.StatusBadRequest {
		t.Fatalf("url on an unbound plugin: %d %s; want 400", rec.Code, rec.Body.String())
	}
	if rec := w.do("POST", "/plugin/v1/messages", `{"external_id":"u2","body":"b","kind":"note","url":"javascript:alert(1)"}`, bearerTok(w.tok)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a javascript: url: %d %s; want 400", rec.Code, rec.Body.String())
	}
	var n int
	_ = w.f.pool.QueryRow(w.ctx, `SELECT count(*) FROM plugin_messages WHERE external_id IN ('u1', 'u2')`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d refused messages stored", n)
	}
}

// TestAnAskShowsOnlyItsOwnItemsMessage: the context reads the message the
// item's payload names only when that message IS the item's (same plugin,
// its officer_item_id this item); a payload naming another item's message, or
// another plugin's, is shown unread — never another message's words.
func TestAnAskShowsOnlyItsOwnItemsMessage(t *testing.T) {
	w := newOutboundWorld(t, true)
	item := postFixtureMessage(t, w, "ctx:own:1")
	var msg int64
	if err := w.f.pool.QueryRow(w.ctx, `SELECT id FROM plugin_messages WHERE officer_item_id = $1`, item).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	payload := func(plugin int64) []byte {
		return []byte(`{"plugin":"` + outboundPluginName + `","plugin_id":` + strconv.FormatInt(plugin, 10) + `,"message_id":` + strconv.FormatInt(msg, 10) + `}`)
	}
	read := func(itemID, plugin int64) *attention.SourceMessage {
		c := w.srv.officerItemContext(w.ctx, 1, builtinOfficerID, builtin.AskAbout{ItemID: itemID, Kind: "plugin.reply", SubjectKey: "s", Payload: payload(plugin)})
		if c == nil || c.Source == nil {
			t.Fatalf("item %d plugin %d: no source", itemID, plugin)
		}
		return c.Source
	}
	if got := read(item, w.plugin.ID); got.Unread || got.Sender != ctxSender || !strings.HasPrefix(got.Body, ctxLine1) {
		t.Fatalf("the item's own message: %+v, want it read", got)
	}
	for name, got := range map[string]*attention.SourceMessage{"another item": read(item+1000, w.plugin.ID), "another plugin": read(item, w.plugin.ID+1000)} {
		if !got.Unread || got.Sender != "" || got.Body != "" {
			t.Errorf("a payload naming %s's message: %+v, want it unread", name, got)
		}
	}
}
