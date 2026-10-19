package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/notes"
)

// TestTaskDispatchRefusesATaskStoredRepoThatIsNotConnected: the connected-repo
// guard covers the EFFECTIVE repository — the task's own when the chief passes
// none — for both kinds, before the reservation (mustRefuse: no agent, no unit,
// no provisioner call). A task's repository can have been written by an agent
// (task_create_gated), and a claude-code agent's session starts pre-trusted in
// its checkout. A connected task repository still starts.
func TestTaskDispatchRefusesATaskStoredRepoThatIsNotConnected(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	f.withConnectedRepos(t, "example/app")
	for _, kind := range []string{agents.KindClaudeCode, agents.KindGateway} {
		n, err := f.notes.Create(f.asOwner, notes.Note{Title: "stranger repo " + kind, Body: "b", Repo: "stranger/x"})
		if err != nil {
			t.Fatal(err)
		}
		f.mustRefuse(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID, "kind": kind}), chiefDispatchRepo, n.ID)
	}
	ok, err := f.notes.Create(f.asOwner, notes.Note{Title: "connected repo", Body: "b", Repo: "example/app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.startedOne(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": ok.ID}), ok.ID); got.Repo != "example/app" {
		t.Fatalf("agent %+v", got)
	}
}

// TestTaskDispatchRefusesATaskWhoseRepoChangedAfterTheCheck: the task's
// repository is checked before the reservation and re-read under its lock; one
// rewritten in between (here: while the owner's repositories are being listed)
// is refused, not started on the unchecked repository.
func TestTaskDispatchRefusesATaskWhoseRepoChangedAfterTheCheck(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n, err := f.notes.Create(f.asOwner, notes.Note{Title: "moving repo", Body: "b", Repo: "example/app"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" || r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE notes SET repo = 'stranger/x' WHERE id = $1`, n.ID); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"full_name": "example/app"}})
	}))
	t.Cleanup(srv.Close)
	github.UseAPIBaseURL(srv.URL)
	t.Cleanup(func() { github.UseAPIBaseURL("https://api.github.com") })
	f.srv.ext.GitHub = fakeGitHubConn{}
	f.mustRefuse(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID}), "repository changed while it was being checked", n.ID)
}
