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
// real store, the drift offer's Rename as htmx posts it (ui.SessionDriftParam)
// makes the agent's name manual — an ordinary rename — and is answered with
// NO reload: the header's title, the <title> and both rename dialogs redrawn
// with the new name, the banner deleted, agents:changed for the sidebar. The
// same post without the marker (the rename dialog's Save) still reloads, and
// without script it is still a 303 back to the agent's page.
func TestTheOffersRenameOnAnAgentIsAnsweredInPlace(t *testing.T) {
	f := newAttnFixture(t)
	slug := "rename-inplace-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a := f.agentFor(t, 1, slug, slug, "rename-inplace-tok-"+slug)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET display_name = '#42 · Fix the login bug',
		display_name_source = 'dispatch' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	path := "/agents/" + strconv.FormatInt(a.ID, 10) + "/name"
	const offer = "Wiring the refund flow"
	form := url.Values{"kind": {"agent"}, "agent_id": {strconv.FormatInt(a.ID, 10)}, "back": {ui.AgentPath(slug)},
		"name": {offer}, "drift": {"1"}}
	rec := pinPost(t, f.srv, f.srv.Handler(), path, form, true)
	expectNoReload(t, "the offer's rename", rec)
	if got := mustAgent(t, f, a.ID); got.DisplayName != offer || got.DisplayNameSource != agents.NameManual {
		t.Fatalf("the offer's Rename stored %q (%s), want %q manual", got.DisplayName, got.DisplayNameSource, offer)
	}
	if got := rec.Header().Get("HX-Trigger"); got != "agents:changed" {
		t.Errorf("HX-Trigger %q, want agents:changed", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<div id="session-drift" hx-swap-oob="delete"></div>`,
		`hx-swap-oob="innerHTML">Renamed to “` + offer + `”.</p>`,
		`<div id="agent-title" hx-swap-oob="outerHTML"`,
		`<title>` + offer + ` · muster</title>`,
		`id="agent-rename" hx-swap-oob="outerHTML"`,
		`value="` + offer + `" data-session-rename-input`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Fix the login bug") {
		t.Errorf("the answer still names the agent by its old name:\n%s", body)
	}
	form.Del("drift")
	form.Set("name", "Login fix")
	if rec := pinPost(t, f.srv, f.srv.Handler(), path, form, true); rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("the dialog's Save = %d (HX-Refresh %q), want the reload", rec.Code, rec.Header().Get("HX-Refresh"))
	}
	form.Set("drift", "1")
	if rec := pinPost(t, f.srv, f.srv.Handler(), path, form, false); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != ui.AgentPath(slug) {
		t.Fatalf("without script = %d to %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), ui.AgentPath(slug))
	}
}
