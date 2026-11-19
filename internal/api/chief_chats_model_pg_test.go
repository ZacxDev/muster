package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// A SIDE CHAT ON A PICKED MODEL (the "+" menu, docs/chief.md#primary-and-side-chats):
// POST /agents/{id}/sessions/side takes an optional model, checked by the one
// predicate every session door uses (runtimeSessionModel), stored on the chat
// for its life, and carried on every turn as the lane's --model.

// postSideChat POSTs the side chat route with model ("" sends none), as the
// "+" menu's entry does on the page.
func (f *chatsFixture) postSideChat(t *testing.T, model string) *httptest.ResponseRecorder {
	t.Helper()
	v := url.Values{}
	if model != "" {
		v.Set("model", model)
	}
	body, hdr := form(v)
	return f.call(t, http.MethodPost, ui.ChiefSideChatPath(f.chief.ID), body, merge(f.operator(), hdr))
}

// sessionCount is the chief's stored session count (system scope).
func (f *chatsFixture) sessionCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM chat_sessions WHERE agent_id = $1`, f.chief.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// newSideChatOn opens a side chat on model through the route and returns the
// one session it made, checking the answer lands on it with the composer
// focused.
func (f *chatsFixture) newSideChatOn(t *testing.T, model string) agents.ChatSession {
	t.Helper()
	// The primary first, so the only new session is the side chat.
	if _, err := f.srv.primarySession(asUser(1), f.chief); err != nil {
		t.Fatal(err)
	}
	before := f.sessions(t)
	w := f.postSideChat(t, model)
	if w.Code != http.StatusOK {
		t.Fatalf("POST side chat on %q: %d %s", model, w.Code, w.Body)
	}
	var made []agents.ChatSession
	for id, s := range f.sessions(t) {
		if _, ok := before[id]; !ok {
			made = append(made, s)
		}
	}
	if len(made) != 1 {
		t.Fatalf("POST side chat on %q made %d sessions, want 1 (a new chat, not a reused one)", model, len(made))
	}
	if want := ui.AgentSessionHref(f.chief.Name, made[0].ID) + "&" + ui.ComposerFocusParam + "=1"; w.Header().Get("HX-Redirect") != want {
		t.Fatalf("the side chat's answer goes to %q, want %q", w.Header().Get("HX-Redirect"), want)
	}
	return made[0]
}

// TestASideChatOpensOnThePickedModel: with the runtimes gate offering
// claude-code to a claude-code chief, "+" → Sonnet stores the chat on model
// sonnet, and its turn runs in a lane of its own started with --model sonnet
// (the route's X-Muster-CC-Model, which ccd turns into argv). A pick on another
// model while that chat is still empty opens a NEW chat on it (an empty chat is
// reused only on the same model), and Default opens one on the chief's own
// model ("", no --model).
//
// The models are not the default (""), so a mutant that drops the model, or
// hardcodes "" or one model, fails here.
func TestASideChatOpensOnThePickedModel(t *testing.T) {
	f := newChatsFixture(t, agents.KindClaudeCode)
	f.srv.ext.SessionRuntimes = []string{agents.SessionRuntimeClaudeCode}
	ctx := asUser(1)

	sonnet := f.newSideChatOn(t, "sonnet")
	if sonnet.Model != "sonnet" || sonnet.Runtime != agents.SessionRuntimeClaudeCode || sonnet.IsDefault {
		t.Fatalf("the side chat picked on sonnet is stored as runtime %q model %q default %v; want claude-code/sonnet, not the primary",
			sonnet.Runtime, sonnet.Model, sonnet.IsDefault)
	}
	// Still empty: a second pick, on haiku, is a new chat, not the sonnet one.
	haiku := f.newSideChatOn(t, "haiku")
	if haiku.Model != "haiku" || haiku.ID == sonnet.ID {
		t.Fatalf("the haiku pick: chat %d on %q; want a new chat on haiku (the sonnet chat is %d)", haiku.ID, haiku.Model, sonnet.ID)
	}
	def := f.newSideChatOn(t, "")
	if def.Model != "" || def.Runtime != agents.SessionRuntimeClaudeCode {
		t.Fatalf("Default: chat on runtime %q model %q; want claude-code on the chief's own model (\"\")", def.Runtime, def.Model)
	}

	// The real turn path: each chat's turn carries its own lane and model.
	for _, sess := range []agents.ChatSession{sonnet, haiku, def} {
		if !f.srv.turns.enter(f.chief.ID) {
			t.Fatal("turn gate refused")
		}
		if _, err := f.srv.runEnteredTurn(ctx, f.chief, sess, "hello", 1, nil, nil); err != nil {
			t.Fatal(err)
		}
		f.srv.turns.leave(f.chief.ID)
	}
	keys, lanes, _ := f.gw.turns()
	models := f.gw.modelsSeen()
	want := []string{"sonnet", "haiku", ""}
	if strings.Join(models, ",") != strings.Join(want, ",") {
		t.Errorf("the turns' %s = %q, want %q (the lane's --model)", agents.HeaderCCModel, models, want)
	}
	for i, sess := range []agents.ChatSession{sonnet, haiku, def} {
		if keys[i] != sess.SessionKey || lanes[i] != sess.SessionKey {
			t.Errorf("turn %d ran as key %q lane %q; want chat %d's own lane %q", i, keys[i], lanes[i], sess.ID, sess.SessionKey)
		}
	}
}

// TestASideChatRefusesABadModel: a model the session validator refuses is a
// 400 naming why, and writes nothing — no side chat, and no primary elected
// either (the model is checked before the route touches the store).
func TestASideChatRefusesABadModel(t *testing.T) {
	f := newChatsFixture(t, agents.KindClaudeCode)
	f.srv.ext.SessionRuntimes = []string{agents.SessionRuntimeClaudeCode}
	if n := f.sessionCount(t); n != 0 {
		t.Fatalf("control: the fresh chief has %d sessions", n)
	}
	for _, bad := range []string{"not a model", "opus --dangerously-skip-permissions", "Sonnet"} {
		w := f.postSideChat(t, bad)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "is not a Claude model") {
			t.Errorf("POST side chat on %q: %d %q; want 400 \"… is not a Claude model …\"", bad, w.Code, w.Body)
		}
	}
	if n := f.sessionCount(t); n != 0 {
		t.Errorf("refused picks wrote %d sessions, want none", n)
	}
	// Control: the same fixture opens a chat on a good model.
	if s := f.newSideChatOn(t, "sonnet"); s.Model != "sonnet" {
		t.Fatalf("control: a good pick made a chat on %q", s.Model)
	}
}

// TestASideChatModelIsRefusedWhereNoPickerIsOffered: with the runtimes gate off
// (or on a gateway chief, which has no claude-code runtime), a model is
// REFUSED, not ignored: a chat's model is fixed once made, so dropping the pick
// would open, for good, a chat on a model nobody asked for. "+" with no model
// opens a chat as before.
func TestASideChatModelIsRefusedWhereNoPickerIsOffered(t *testing.T) {
	for _, c := range []struct {
		name     string
		kind     string
		runtimes []string
	}{
		{"gate off, claude-code chief", agents.KindClaudeCode, nil},
		{"gate on, gateway chief", "", []string{agents.SessionRuntimeClaudeCode}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newChatsFixture(t, c.kind)
			f.srv.ext.SessionRuntimes = c.runtimes
			w := f.postSideChat(t, "sonnet")
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), errRuntimeNotOffered.Error()) {
				t.Errorf("POST side chat on sonnet: %d %q; want 400 %q", w.Code, w.Body, errRuntimeNotOffered)
			}
			if n := f.sessionCount(t); n != 0 {
				t.Errorf("a refused pick wrote %d sessions", n)
			}
			// Today's "+": no model, a chat on the chief's own.
			if s := f.newSideChatOn(t, ""); s.Model != "" {
				t.Errorf("a plain \"+\" made a chat on %q", s.Model)
			}
		})
	}
}

// newChatEntryRE is a "+" menu entry: its model attribute.
var newChatEntryRE = regexp.MustCompile(`data-chief-new-chat-model="([^"]*)"`)

// TestTheChiefPageOffersTheModelMenuOnlyWithTheGate: the chief page's "+" is a
// menu of Default and the New session picker's models
// (agents.ClaudeCodeModels) when the gate offers claude-code, and today's
// one-tap form when it does not. A chat opened on sonnet carries a "sonnet"
// tag on its chip on a fresh load; a chat on the chief's own model none.
func TestTheChiefPageOffersTheModelMenuOnlyWithTheGate(t *testing.T) {
	f := newChatsFixture(t, agents.KindClaudeCode)
	page := func(sid int64) string {
		t.Helper()
		w := f.call(t, http.MethodGet, ui.AgentSessionHref(f.chief.Name, sid), nil, f.operator())
		if w.Code != http.StatusOK {
			t.Fatalf("GET chief page: %d", w.Code)
		}
		return w.Body.String()
	}
	primary, err := f.srv.primarySession(asUser(1), f.chief)
	if err != nil {
		t.Fatal(err)
	}

	off := page(primary.ID)
	if !strings.Contains(off, `data-chief-side-chat=""`) {
		t.Fatal("control: the gate-off page has no \"+\"")
	}
	if m := newChatEntryRE.FindAllStringSubmatch(off, -1); len(m) != 0 || strings.Contains(off, "data-chief-new-chat-menu") {
		t.Errorf("gate off: the page draws a model menu (%d entries)", len(m))
	}

	f.srv.ext.SessionRuntimes = []string{agents.SessionRuntimeClaudeCode}
	var got []string
	for _, m := range newChatEntryRE.FindAllStringSubmatch(page(primary.ID), -1) {
		got = append(got, m[1])
	}
	if want := []string{"", "opus", "sonnet", "haiku"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("gate on: the menu's entries are %q, want %q", got, want)
	}

	// The chip tag, on a fresh load of a chat opened on sonnet.
	sonnet := f.newSideChatOn(t, "sonnet")
	def := f.newSideChatOn(t, "")
	on := page(sonnet.ID)
	// A chip is its <li>: from its data-chief-chip to the next chip's.
	chip := func(html string, id int64) string {
		t.Helper()
		i := strings.Index(html, `data-chief-chip="`+strconv.FormatInt(id, 10)+`"`)
		if i < 0 {
			t.Fatalf("no chip for chat %d", id)
		}
		rest := html[i:]
		if j := strings.Index(rest[1:], "data-chief-chip=\""); j >= 0 {
			rest = rest[:j+1]
		}
		return rest
	}
	if c := chip(on, sonnet.ID); !strings.Contains(c, `data-chief-chip-model="sonnet"`) {
		t.Errorf("the sonnet chat's chip has no sonnet tag:\n%s", c)
	}
	if c := chip(on, def.ID); strings.Contains(c, "data-chief-chip-model") {
		t.Errorf("the Default chat's chip has a model tag:\n%s", c)
	}
}

// TestThePanelsPlusMenuOpensASonnetChatInThePanel: the chief PANEL draws the
// same "+" menu (chief-panel-new-chat-menu) with the gate on, and its Sonnet
// entry, posted as drawn (its own hx-post and hidden model), opens a side chat
// on sonnet answered with the panel body re-rendered on it (no redirect: the
// grid stays), its chip tagged sonnet.
func TestThePanelsPlusMenuOpensASonnetChatInThePanel(t *testing.T) {
	f := newChatsFixture(t, agents.KindClaudeCode)
	f.srv.ext.SessionRuntimes = []string{agents.SessionRuntimeClaudeCode}
	w := f.call(t, http.MethodGet, "/ui/chief/panel", nil, f.operator())
	if w.Code != http.StatusOK {
		t.Fatalf("GET the chief panel: %d", w.Code)
	}
	panel := w.Body.String()
	i := strings.Index(panel, `<div id="chief-panel-new-chat-menu"`)
	if i < 0 {
		t.Fatal("the panel draws no \"+\" model menu (chief-panel-new-chat-menu)")
	}
	menu := panel[i:]
	menu = menu[:strings.Index(menu, "</div>")]
	entry := regexp.MustCompile(`<form hx-post="([^"]+)"[^>]*><input type="hidden" name="model" value="([^"]+)"><button type="submit" data-chief-new-chat-model="sonnet"`).FindStringSubmatch(menu)
	if entry == nil {
		t.Fatalf("the panel's menu has no Sonnet entry:\n%s", menu)
	}
	path := strings.ReplaceAll(entry[1], "&amp;", "&")
	if want := ui.ChiefSideChatPath(f.chief.ID) + "?surface=chief-panel"; path != want {
		t.Fatalf("the Sonnet entry posts to %q, want %q", path, want)
	}
	if _, err := f.srv.primarySession(asUser(1), f.chief); err != nil {
		t.Fatal(err)
	}
	before := f.sessions(t)
	body, hdr := form(url.Values{"model": {entry[2]}})
	w = f.call(t, http.MethodPost, path, body, merge(f.operator(), hdr))
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != "" {
		t.Fatalf("POST the panel's Sonnet entry: %d redirect %q; want 200 with the panel body", w.Code, w.Header().Get("HX-Redirect"))
	}
	var made []agents.ChatSession
	for id, s := range f.sessions(t) {
		if _, ok := before[id]; !ok {
			made = append(made, s)
		}
	}
	if len(made) != 1 || made[0].Model != "sonnet" || made[0].Runtime != agents.SessionRuntimeClaudeCode {
		t.Fatalf("the panel's Sonnet entry made %+v; want one claude-code chat on sonnet", made)
	}
	sid := strconv.FormatInt(made[0].ID, 10)
	if !strings.Contains(w.Body.String(), `data-chief-active-session="`+sid+`"`) {
		t.Errorf("the panel's answer is not on the new chat %s", sid)
	}
	if !regexp.MustCompile(`data-chief-chip="` + sid + `"(?s:.)*?data-chief-chip-model="sonnet"`).MatchString(w.Body.String()) {
		t.Errorf("the panel's answer draws no sonnet tag on chat %s's chip", sid)
	}
}
