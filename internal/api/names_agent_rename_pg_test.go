package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestSavingAnAgentsShownNameIsNotARename: through the real route
// (POST /agents/{id}/name) on a real store, a task-dispatched agent whose
// prefilled name is saved unchanged keeps its name's source (dispatch) — it
// does not become the person's; a changed name does; and a plain form post
// (no script) is answered with a 303 back to the agent's page.
func TestSavingAnAgentsShownNameIsNotARename(t *testing.T) {
	f := newAttnFixture(t)
	slug := "rename-noop-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a := f.agentFor(t, 1, slug, slug, "rename-noop-tok-"+slug)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET display_name = '#42 · Fix the login bug',
		display_name_source = 'dispatch' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	path := "/agents/" + strconv.FormatInt(a.ID, 10) + "/name"
	source := func() string {
		t.Helper()
		got, err := agents.NewPG(f.pool).Get(asUser(1), a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.DisplayNameSource
	}
	shown := f.srv.agentName(asUser(1), mustAgent(t, f, a.ID)).Text
	if shown != "#42 · Fix the login bug" {
		t.Fatalf("control: the shown name is %q", shown)
	}
	if rec := pinPost(t, f.srv, f.srv.Handler(), path, url.Values{"name": {shown}}, true); rec.Code != http.StatusOK {
		t.Fatalf("save unchanged = %d", rec.Code)
	}
	if got := source(); got != agents.NameDispatch {
		t.Fatalf("saving the shown task name made it %q, want it kept %q", got, agents.NameDispatch)
	}
	rec := pinPost(t, f.srv, f.srv.Handler(), path, url.Values{"name": {"Login fix"}}, false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != ui.AgentPath(slug) {
		t.Fatalf("a plain post = %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), ui.AgentPath(slug))
	}
	if got := source(); got != agents.NameManual {
		t.Fatalf("a changed name's source is %q, want manual", got)
	}
}

func mustAgent(t *testing.T, f *attnFixture, id int64) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(f.pool).Get(asUser(1), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestTheOffersRenameOnAnAgentIsAnsweredInPlace: through the real route on a
// real store, every htmx rename of an agent — the drift offer's Rename and the
// rename dialog's Save post the same fields — makes its name manual (an
// ordinary rename) and is answered with NO reload, on the page that posted
// it: from the agent's page its title box, both rename dialogs and the
// banner, the tab's title and focus on #agent-title; from its Terminal page
// (no #agent-title there) the Terminal heading, "Terminal · … · muster" and
// focus on #term-title. A CLEARED name (no human name left) draws the slug.
// Without script it is still a 303 back to the agent's page.
func TestTheOffersRenameOnAnAgentIsAnsweredInPlace(t *testing.T) {
	f := newAttnFixture(t)
	slug := "rename-inplace-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a := f.agentFor(t, 1, slug, slug, "rename-inplace-tok-"+slug)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET display_name = '#42 · Fix the login bug',
		display_name_source = 'dispatch' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	path := "/agents/" + strconv.FormatInt(a.ID, 10) + "/name"
	form := func(name string) url.Values {
		return url.Values{"kind": {"agent"}, "agent_id": {strconv.FormatInt(a.ID, 10)}, "back": {ui.AgentPath(slug)}, "name": {name}}
	}
	for _, tc := range []struct {
		what, name, page, swapped, focusOn, title string
	}{
		{"the offer's Rename", "Wiring the refund flow", ui.AgentPath(slug), `<div id="agent-title" hx-swap-oob="outerHTML"`, "agent-title", "Wiring the refund flow · muster"},
		{"the dialog's Save", "Login fix", ui.AgentPath(slug), `<div id="agent-title" hx-swap-oob="outerHTML"`, "agent-title", "Login fix · muster"},
		{"the Terminal page's dialog", "Login fix two", ui.AgentTerminalPath(slug), `<h1 id="term-title" hx-swap-oob="outerHTML"`, "term-title", "Terminal · Login fix two · muster"},
		{"a cleared name", "", ui.AgentPath(slug), `<div id="agent-title" hx-swap-oob="outerHTML"`, "agent-title", slug + " · muster"},
	} {
		rec := postFrom(t, f.srv, path, form(tc.name), tc.page)
		expectNoReload(t, tc.what, rec)
		got := mustAgent(t, f, a.ID)
		if tc.name != "" && (got.DisplayName != tc.name || got.DisplayNameSource != agents.NameManual) {
			t.Fatalf("%s stored %q (%s), want %q manual", tc.what, got.DisplayName, got.DisplayNameSource, tc.name)
		}
		if title, focusOn := settled(t, rec); title != tc.title || focusOn != tc.focusOn {
			t.Errorf("%s: settles title %q focus %q, want %q on %s", tc.what, title, focusOn, tc.title, tc.focusOn)
		}
		if got := rec.Header().Get("HX-Trigger"); got != "agents:changed" {
			t.Errorf("%s: HX-Trigger %q, want agents:changed", tc.what, got)
		}
		body := rec.Body.String()
		if !strings.Contains(body, tc.swapped) || strings.Contains(body, "<title") || strings.Contains(body, "Fix the login bug") {
			t.Errorf("%s: the answer does not swap %s (or carries a <title>, or the old name):\n%s", tc.what, tc.swapped, body)
		}
		if tc.focusOn == "term-title" && strings.Contains(body, `id="agent-title"`) {
			t.Errorf("%s: a Terminal page's answer swaps #agent-title, which it has not", tc.what)
		}
		if tc.name == "" && !strings.Contains(elementOf(body, tc.swapped, "</h1>"), ">"+slug+"<") {
			t.Errorf("a cleared name: the title box does not fall back to the slug:\n%s", body)
		}
	}
	if rec := pinPost(t, f.srv, f.srv.Handler(), path, form("Login fix"), false); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != ui.AgentPath(slug) {
		t.Fatalf("without script = %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), ui.AgentPath(slug))
	}
}
