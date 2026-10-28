package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/pins"
)

// listingApp is a GitHub App stand-in for the API's identity readers: it lists
// fixed installation repositories and records which agent asked for PRs.
type listingApp struct {
	repos   []string
	problem string
	prAgent int64
}

func (l *listingApp) Repos(context.Context) ([]github.Repo, error) {
	out := make([]github.Repo, 0, len(l.repos))
	for _, n := range l.repos {
		out = append(out, github.Repo{FullName: n, Private: true})
	}
	return out, nil
}
func (l *listingApp) DefaultBranch(context.Context, string) (string, error) {
	return "trunk", nil
}
func (l *listingApp) ListPullRequests(_ context.Context, agentID int64, _, _ string) ([]github.PullRequest, error) {
	l.prAgent = agentID
	return []github.PullRequest{{Number: 3, Title: "from the app"}}, nil
}
func (l *listingApp) Problem(int64) string { return l.problem }

// TestAClaudeCodeDispatchChecksTheAppsRepositories: at the dispatch door an App
// repository is accepted (on the App's default branch), and one outside the
// installation is refused.
func TestAClaudeCodeDispatchChecksTheAppsRepositories(t *testing.T) {
	s, h, store := kindsServer(t, stubKinds{pick: "work"})
	s.ext.GitHubApp = &listingApp{repos: []string{"example-org/widget"}}
	rec := postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "note_text": {"t"}, "repo": {"other-account/not-installed"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), chiefDispatchRepo) || len(store.rows()) != 0 {
		t.Fatalf("a repository outside the installation: %d %q rows=%d", rec.Code, rec.Body, len(store.rows()))
	}
	rec = postDispatch(t, s, h, url.Values{"kind": {"claude-code"}, "note_text": {"t"}, "repo": {"Example-Org/Widget"}})
	if rec.Code != http.StatusOK || len(store.rows()) != 1 {
		t.Fatalf("an installation repository: %d %q rows=%d", rec.Code, rec.Body, len(store.rows()))
	}
	if b := store.rows()[0].RepoBranch; b != "trunk" {
		t.Fatalf("the default branch came from %q, want the App's (trunk)", b)
	}
}

// TestTaskDispatchsEffectiveRepoIsCheckedAgainstTheApp: task_dispatch's
// effective-repository check (#150) uses the App's installation repositories
// for an ADMIN owner's chief — and, for a MEMBER owner's chief (R2), the App's
// repositories are not connected at all.
func TestTaskDispatchsEffectiveRepoIsCheckedAgainstTheApp(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	n := f.task(t, "needs a repo")
	f.srv.ext.GitHubApp = &listingApp{repos: []string{"example/app"}}
	callAs := func(role authz.Role, args map[string]any) string {
		p, _ := authz.From(f.asOwner)
		p.Role = role
		raw, _ := json.Marshal(args)
		return f.srv.dispatchAgentTool(authz.WithPrincipal(f.asOwner, p), f.chief, taskDispatchTool, string(raw))
	}
	f.mustRefuse(t, callAs(authz.RoleMember, map[string]any{"task_id": n.ID, "repo": "example/app"}), chiefDispatchNoGitHub, n.ID)
	f.mustRefuse(t, callAs(authz.RoleAdmin, map[string]any{"task_id": n.ID, "repo": "stranger/app"}), chiefDispatchRepo, n.ID)
	text := callAs(authz.RoleAdmin, map[string]any{"task_id": n.ID, "repo": "example/app"})
	if strings.Contains(text, `"error"`) {
		t.Fatalf("an installation repository, admin owner: %s", text)
	}
}

// TestAMemberGetsNoAppRepositories (R2): for a member principal the App's
// repositories are not listed, not connected, and the dispatch dialog's
// repository picker offers none — while an admin principal gets the App's.
func TestAMemberGetsNoAppRepositories(t *testing.T) {
	s, _, _ := kindsServer(t, stubKinds{pick: "work"})
	s.ext.GitHubApp = &listingApp{repos: []string{"example-org/widget"}}
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	admin := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if why := s.connectedRepo(member, "example-org/widget"); why == "" {
		t.Fatal("a member's dispatch check accepted an App repository")
	}
	if why := s.connectedRepo(admin, "example-org/widget"); why != "" {
		t.Fatalf("control: the admin's dispatch check refused an App repository: %q", why)
	}
	if list, err := s.githubRepos(member); err == nil && len(list) > 0 {
		t.Fatalf("a member's repository list holds App repositories: %+v", list)
	}
	for _, c := range []struct {
		ctx  context.Context
		want bool
	}{{member, false}, {admin, true}} {
		req := httptest.NewRequest(http.MethodGet, "/ui/agents/repos", nil).WithContext(c.ctx)
		rec := httptest.NewRecorder()
		s.handleAgentRepoOptions(rec, req)
		if got := strings.Contains(rec.Body.String(), "example-org/widget"); got != c.want {
			t.Errorf("the repository picker offers the App repository = %v, want %v:\n%s", got, c.want, rec.Body)
		}
	}
}

// TestWithoutAnAppNoRepositoryIsConnected: the GitHub App is muster's only
// GitHub identity (the OAuth fallback is gone), so with none configured every
// reader answers "no GitHub" — for an admin too — and the picker offers none.
func TestWithoutAnAppNoRepositoryIsConnected(t *testing.T) {
	s, _, _ := kindsServer(t, stubKinds{pick: "work"})
	admin := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if why := s.connectedRepo(admin, "example-org/widget"); why != chiefDispatchNoGitHub {
		t.Fatalf("no App: connectedRepo = %q, want %q", why, chiefDispatchNoGitHub)
	}
	if _, err := s.githubRepos(admin); !errors.Is(err, errNoGitHub) {
		t.Fatalf("no App: githubRepos err = %v, want errNoGitHub", err)
	}
	if _, err := s.githubDefaultBranch(admin, "example-org/widget"); !errors.Is(err, errNoGitHub) {
		t.Fatalf("no App: githubDefaultBranch err = %v, want errNoGitHub", err)
	}
	// Control: the same admin with an App configured is connected.
	s.ext.GitHubApp = &listingApp{repos: []string{"example-org/widget"}}
	if why := s.connectedRepo(admin, "example-org/widget"); why != "" {
		t.Fatalf("control: with the App the admin's repository is refused: %q", why)
	}
}

// TestTheAgentCardShowsTheAppsProblemForARepoAgent.
func TestTheAgentCardShowsTheAppsProblemForARepoAgent(t *testing.T) {
	s, _, _ := kindsServer(t, stubKinds{pick: "work"})
	s.ext.GitHubApp = &listingApp{problem: "GitHub refused the GitHub App's credentials (401) while minting"}
	withRepo := s.cardViewIndexed(agents.Agent{ID: 1, Name: "a", Repo: "example-org/widget", OwnerID: 1}, nil, time.Time{}, 0,
		func(agents.Agent) bool { return false }, pins.Set{}, nil)
	if !strings.Contains(withRepo.GitHubProblem, "(401)") {
		t.Fatalf("a repository agent's card carries no App problem: %+v", withRepo.GitHubProblem)
	}
	noRepo := s.cardViewIndexed(agents.Agent{ID: 2, Name: "b"}, nil, time.Time{}, 0,
		func(agents.Agent) bool { return false }, pins.Set{}, nil)
	if noRepo.GitHubProblem != "" {
		t.Fatalf("an agent without a repository shows the App's problem: %q", noRepo.GitHubProblem)
	}
	memberOwned := s.cardViewIndexed(agents.Agent{ID: 3, Name: "c", Repo: "example-org/widget", OwnerID: 2}, nil, time.Time{}, 0,
		func(agents.Agent) bool { return false }, pins.Set{}, nil)
	if memberOwned.GitHubProblem != "" {
		t.Fatalf("a member's agent shows the (admin-only) App's problem: %q", memberOwned.GitHubProblem)
	}
}
