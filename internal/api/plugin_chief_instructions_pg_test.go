package api

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/plugins"
)

// A PLUGIN'S CHIEF INSTRUCTIONS through the real server against a real
// Postgres (plugin_chief_instructions.go): the plugin door's PUT and GET under
// chief_extension, the owner's card on /plugins, its step-up Approve of one
// exact sha, and revoke. Every text is distinct and spelled by no constant.

// chiefInstr is one request on the plugin door's chief instructions route.
func (f *draftToolFixture) chiefInstr(t *testing.T, tok, method, body string) (int, map[string]any) {
	t.Helper()
	w := f.call(t, method, pluginChiefInstructionsPath, strings.NewReader(body),
		map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// ownerForm posts a /plugins form as the owner's browser, with cookie.
func (f *draftToolFixture) ownerForm(t *testing.T, path string, form url.Values, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, http.MethodPost, path, strings.NewReader(form.Encode()), map[string]string{
		"Cookie": cookie, "Origin": "http://example.com", "Content-Type": "application/x-www-form-urlencoded",
		"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate"})
}

func (f *draftToolFixture) pluginsPage(t *testing.T) string {
	t.Helper()
	w := f.call(t, http.MethodGet, pluginsPath, nil, map[string]string{"Cookie": sessionCookieFor(f.srv)})
	if w.Code != http.StatusOK {
		t.Fatalf("GET /plugins: %d", w.Code)
	}
	return w.Body.String()
}

func markdownBody(text string) string {
	b, _ := json.Marshal(map[string]string{"markdown": text})
	return string(b)
}

// guide mints a plugin with chief_extension (beside messages) for the
// fixture's owner and returns its token.
func (f *draftToolFixture) guide(t *testing.T, name string) (string, plugins.Plugin) {
	t.Helper()
	tok, p, err := f.plugins.CreatePlugin(f.asOwner, plugins.NewPlugin{Name: name, Label: "Example guide", DataScope: name,
		Capabilities: []plugins.Capability{plugins.CapMessages, plugins.CapChiefExtension}})
	if err != nil {
		t.Fatal(err)
	}
	return tok, p
}

// TestATokenWithoutChiefExtensionIsRefusedItsChiefInstructions: the
// fixture's messages-only token gets 403 naming the capability on PUT and
// GET, and stores nothing; the owner turns Chief instructions on from its
// /plugins card and the SAME token is then served (200, pending).
func TestATokenWithoutChiefExtensionIsRefusedItsChiefInstructions(t *testing.T) {
	f := newDraftToolFixture(t)
	for _, m := range []string{http.MethodPut, http.MethodGet} {
		code, out := f.chiefInstr(t, f.plugTok, m, markdownBody("Wren notes belong on the wren board."))
		if code != http.StatusForbidden || out["error"] != PluginCapabilityRefusal(plugins.CapChiefExtension) {
			t.Fatalf("%s without chief_extension: %d %v; want 403 naming the capability", m, code, out)
		}
	}
	if _, ok, err := f.plugins.ChiefInstructionsOf(f.asOwner, f.plugin.ID); ok || err != nil {
		t.Fatalf("a refused PUT stored something: %v %v", ok, err)
	}
	if w := f.ownerForm(t, pluginsPath+"/"+strconv.FormatInt(f.plugin.ID, 10)+"/chief-extension", url.Values{"on": {"1"}}, sessionCookieFor(f.srv)); w.Code != http.StatusSeeOther {
		t.Fatalf("the owner turning Chief instructions on: %d %s", w.Code, w.Body)
	}
	code, out := f.chiefInstr(t, f.plugTok, http.MethodPut, markdownBody("Wren notes belong on the wren board."))
	if code != http.StatusOK || out["state"] != "pending" || out["changed"] != true {
		t.Fatalf("after the grant the SAME token's PUT: %d %v; want 200 pending", code, out)
	}
}

// TestTheChiefInstructionsCapIsFourKiBAtTheDoor: 4096 bytes are 200, 4097
// bytes 400 naming the cap, and the stored text is still the 4096.
func TestTheChiefInstructionsCapIsFourKiBAtTheDoor(t *testing.T) {
	f := newDraftToolFixture(t)
	tok, p := f.guide(t, "example-yew")
	fits := strings.Repeat("y", plugins.MaxChiefInstructionsBytes)
	if code, out := f.chiefInstr(t, tok, http.MethodPut, markdownBody(fits)); code != http.StatusOK {
		t.Fatalf("4096 bytes: %d %v", code, out)
	}
	code, out := f.chiefInstr(t, tok, http.MethodPut, markdownBody(strings.Repeat("z", plugins.MaxChiefInstructionsBytes+1)))
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "4096 bytes") {
		t.Fatalf("4097 bytes: %d %v; want 400 naming the cap", code, out)
	}
	if c, _, _ := f.plugins.ChiefInstructionsOf(f.asOwner, p.ID); c.Text != fits {
		t.Fatalf("the refused PUT replaced the stored text (%d bytes)", len(c.Text))
	}
}

// TestTheOwnerApprovesTheExactChiefInstructionsTheCardShows: the card draws
// the plugin's text ESCAPED (its markup is text), with an Approve naming its
// sha; that approval needs a fresh sign-in (403 on a stale one, nothing
// approved); with one it is approved and the plugin's GET says so. 🔴 A STALE
// SHA — the plugin changed the text after the page was drawn — is 409 and
// approves nothing.
func TestTheOwnerApprovesTheExactChiefInstructionsTheCardShows(t *testing.T) {
	f := newDraftToolFixture(t)
	tok, p := f.guide(t, "example-alder")
	const first = "Alder <b>bold</b> & <script>alert(1)</script> requests go to the alder desk."
	if code, out := f.chiefInstr(t, tok, http.MethodPut, markdownBody(first)); code != http.StatusOK {
		t.Fatalf("PUT: %d %v", code, out)
	}
	page := f.pluginsPage(t)
	if strings.Contains(page, "<b>bold</b>") || strings.Contains(page, "<script>alert(1)") {
		t.Fatal("the card drew the plugin's markup as HTML")
	}
	if !strings.Contains(page, html.EscapeString(first)) {
		t.Fatal("the card does not show the plugin's text, escaped")
	}
	shaFirst := sha(first)
	if !strings.Contains(page, `name="sha" value="`+shaFirst+`"`) {
		t.Fatalf("the card's Approve does not name the text's sha %s", shaFirst)
	}
	approvePath := pluginsPath + "/" + strconv.FormatInt(p.ID, 10) + "/approve-chief-instructions"

	// A stale sign-in approves nothing.
	if w := f.ownerForm(t, approvePath, url.Values{"sha": {shaFirst}}, staleCookieFor(f.srv)); w.Code != http.StatusForbidden || w.Header().Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("approve with a stale sign-in: %d; want 403 asking for a step-up", w.Code)
	}
	if _, out := f.chiefInstr(t, tok, http.MethodGet, ""); out["state"] != "pending" {
		t.Fatalf("after a stale-sign-in approve: %v; want still pending", out)
	}

	// The plugin changes its text after the page was drawn: the page's sha is stale.
	const second = "Alder requests now go to the birch desk."
	if code, _ := f.chiefInstr(t, tok, http.MethodPut, markdownBody(second)); code != http.StatusOK {
		t.Fatal(code)
	}
	w := f.ownerForm(t, approvePath, url.Values{"sha": {shaFirst}}, sessionCookieFor(f.srv))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), html.EscapeString(ChiefInstructionsStaleRefusal)) {
		t.Fatalf("approving the old sha after a change: %d; want 409 saying it changed", w.Code)
	}
	if _, out := f.chiefInstr(t, tok, http.MethodGet, ""); out["state"] != "pending" || out["approved_sha"] != nil {
		t.Fatalf("after a stale approve: %v; want the new text still pending, nothing approved", out)
	}

	// The current sha, fresh: approved.
	if w := f.ownerForm(t, approvePath, url.Values{"sha": {sha(second)}}, sessionCookieFor(f.srv)); w.Code != http.StatusSeeOther {
		t.Fatalf("approve the current sha: %d %s", w.Code, w.Body)
	}
	code, out := f.chiefInstr(t, tok, http.MethodGet, "")
	if code != http.StatusOK || out["state"] != "approved" || out["approved_sha"] != sha(second) || out["approved_at"] == nil || out["markdown"] != second {
		t.Fatalf("the plugin's GET after approval: %d %v", code, out)
	}
	secs, err := f.plugins.ApprovedChiefSections(context.Background(), f.owner)
	if err != nil || len(secs) != 1 || secs[0].Text != second || secs[0].Plugin != "example-alder" {
		t.Fatalf("the owner's chief renders %+v (%v); want the approved text alone", secs, err)
	}

	// Another owner's session cannot approve it (the ghost's 404).
	stranger, owner := f.stranger, f.owner
	f.srv.sessionUserOverride = func(*http.Request) int64 { return stranger }
	if w := f.ownerForm(t, approvePath, url.Values{"sha": {sha(second)}}, sessionCookieFor(f.srv)); w.Code != http.StatusNotFound {
		t.Errorf("another owner's approve: %d; want 404", w.Code)
	}
	f.srv.sessionUserOverride = func(*http.Request) int64 { return owner }
}

// TestThePluginsPageChiefInstructionsApproveConfirmsInPlace: a pending
// section's Approve is a data-step-up-form on a page that draws the confirm
// dialog saying this gate (stepUpChannels' proof).
func TestThePluginsPageChiefInstructionsApproveConfirmsInPlace(t *testing.T) {
	f := newDraftToolFixture(t)
	tok, _ := f.guide(t, "example-cedar")
	if code, _ := f.chiefInstr(t, tok, http.MethodPut, markdownBody("Cedar changes are announced in the cedar room.")); code != http.StatusOK {
		t.Fatal(code)
	}
	page := f.pluginsPage(t)
	approve := func(a string) bool { return strings.HasSuffix(a, "/approve-chief-instructions") }
	if got := gatedForms(t, pluginsPath, page, approve, ChiefInstructionsStepUpWhy()); len(got) != 1 {
		t.Errorf("the chief instructions approve forms %v; want the one plugin's", got)
	}
}

// TestARevokedPluginsChiefInstructionsAreGone: the owner revokes the plugin on
// /plugins; its approved section no longer renders for the owner's chief (at
// its next provision), and its token is refused (401).
func TestARevokedPluginsChiefInstructionsAreGone(t *testing.T) {
	f := newDraftToolFixture(t)
	tok, p := f.guide(t, "example-dogwood")
	const text = "Dogwood tickets are triaged by the dogwood rota."
	if code, _ := f.chiefInstr(t, tok, http.MethodPut, markdownBody(text)); code != http.StatusOK {
		t.Fatal(code)
	}
	if _, err := f.plugins.ApproveChiefInstructions(f.asOwner, p.ID, sha(text)); err != nil {
		t.Fatal(err)
	}
	if secs, _ := f.plugins.ApprovedChiefSections(context.Background(), f.owner); len(secs) != 1 {
		t.Fatalf("control: the approved section renders %+v", secs)
	}
	if w := f.ownerForm(t, pluginsPath+"/"+strconv.FormatInt(p.ID, 10)+"/revoke", url.Values{}, sessionCookieFor(f.srv)); w.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", w.Code)
	}
	if secs, err := f.plugins.ApprovedChiefSections(context.Background(), f.owner); err != nil || len(secs) != 0 {
		t.Fatalf("after the revoke the owner's chief renders %+v (%v); want none", secs, err)
	}
	if code, _ := f.chiefInstr(t, tok, http.MethodGet, ""); code != http.StatusUnauthorized {
		t.Fatalf("the revoked token's GET: %d; want 401", code)
	}
}
