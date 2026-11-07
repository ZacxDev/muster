package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
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
