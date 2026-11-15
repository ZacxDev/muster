package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// TEAM SHARING PHASE 4, SLICE 4.8: THE DEPLOYMENT'S GITHUB APP FOR A LISTED
// TEAM (MUSTER_TEAMS_ON_GITHUB_APP, Extensions.GitHubAppTeams), its manager's
// approval of the team agents' `github` requests (/teams/{id}/connectors), and
// the repository picker decided by the object's owner. Synthetic throughout:
// placeholder repositories, an in-memory App (listingApp), and a virtual
// `github` connector that mints nothing (appVirtual).

const (
	teamAppRepo  = "example-org/widget"
	teamAppOther = "example-org/gadget"
)

// appVirtual is the `github` virtual connector as connectors.WithVirtual
// presents the App: its description only. No test here reaches the proxy.
type appVirtual struct{}

func (appVirtual) Describe() connectors.Connector {
	return connectors.Connector{Name: githubapp.ConnectorName, Platform: "github-app", Hosts: []string{"api.github.com", "github.com"},
		Scheme: connectors.SchemeBasic, Username: "x-access-token", CreatedAt: connectors.VirtualCreatedAt, UpdatedAt: connectors.VirtualCreatedAt}
}
func (appVirtual) Credential(context.Context, connectors.Caller, string, string) (string, bool, error) {
	return "", false, connectors.ErrTargetRefused
}
func (appVirtual) Rejected(connectors.Caller) {}
func (appVirtual) Problem(int64) string       { return "" }

// teamAppWorld is the team dispatch world with the App configured (two
// repositories) and, when listed, the team lent it — wired as buildApp wires
// it: the api's set and the connector store's predicate from one list.
type teamAppWorld struct {
	*teamDispatchWorld
	store connectors.Store
}

func newTeamAppWorld(t *testing.T, listed bool) *teamAppWorld {
	t.Helper()
	td := newTeamDispatchWorld(t)
	var teams map[int64]bool
	if listed {
		teams = map[int64]bool{td.team: true}
	}
	base := td.w.srv.ext.Connectors
	td.w.srv.ext.GitHubApp = &listingApp{repos: []string{teamAppRepo, teamAppOther}}
	td.w.srv.ext.GitHubAppTeams = teams
	td.w.srv.ext.Connectors = connectors.WithVirtual(base, appVirtual{}, githubapp.Admits(teams))
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = td.w.pool.Exec(bg, `DELETE FROM connector_grants WHERE owner_id = $1`, td.team)
		_, _ = td.w.pool.Exec(bg, `DELETE FROM connector_standing_grants WHERE owner_id = $1`, td.team)
		_, _ = td.w.pool.Exec(bg, `DELETE FROM connector_audit WHERE owner_id = $1`, td.team)
	})
	return &teamAppWorld{teamDispatchWorld: td, store: td.w.srv.ext.Connectors}
}

// teamWorkerOn is a team agent on the team task, in repo, with its own token.
func (tw *teamAppWorld) teamWorkerOn(t *testing.T, name, repo string) (agents.Agent, string) {
	t.Helper()
	tok := "team-app-" + name + "-" + tw.w.suffix
	a, err := agents.NewPG(tw.w.pool).Create(tw.teamCtx, agents.Agent{Name: name + "-" + tw.w.suffix, Namespace: "ns-" + name + "-" + tw.w.suffix,
		Status: agents.StatusRunning, Kind: agents.KindClaudeCode, CCAccount: isoEnvAccount, HooksToken: tok, NoteID: &tw.task.ID, Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	return a, tok
}

// fileGitHub is the team agent's own `github` request, through its token's
// door (POST /agent/connectors/github/request), as ccd files it: the grant's
// id and status.
func (tw *teamAppWorld) fileGitHub(t *testing.T, a agents.Agent, tok string) (int64, string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/agent/connectors/github/request", strings.NewReader(`{"reason":"clone the repository"}`))
	r.Header.Set("X-Muster-Token", tok)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	tw.w.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("filing the team agent's github request: %d %s", rec.Code, rec.Body)
	}
	var id int64
	var status string
	if err := tw.w.pool.QueryRow(tw.w.sys, `SELECT id, status FROM connector_grants WHERE owner_id = $1 AND connector_name = 'github'
		AND agent_id = $2 ORDER BY id DESC LIMIT 1`, tw.team, a.ID).Scan(&id, &status); err != nil {
		t.Fatalf("the team agent's github grant: %v (answer %d %s)", err, rec.Code, rec.Body)
	}
	return id, status
}

// grant is grant id's status and source, read under System.
func (tw *teamAppWorld) grant(t *testing.T, id int64) (string, string) {
	t.Helper()
	var status, source string
	if err := tw.w.pool.QueryRow(tw.w.sys, `SELECT status, COALESCE(source, '') FROM connector_grants WHERE id = $1`, id).Scan(&status, &source); err != nil {
		t.Fatal(err)
	}
	return status, source
}

// browserAs is a same-origin browser request by uid's session, signed in at
// signedIn (a document form post for a POST).
func (tw *teamAppWorld) browserAs(method, path string, uid int64, signedIn time.Time) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("")).WithContext(context.Background())
	r.Host = "muster.test"
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("Cookie", sessionCookieName+"="+auth.SignSession(tw.w.srv.auth.uiSessionSecret(), signedIn, sessionTTL))
	r.Header.Set("Origin", "http://muster.test")
	r.Header.Set(isoUserHeader, strconv.FormatInt(uid, 10))
	r.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	tw.w.h.ServeHTTP(rec, r)
	return rec
}

func (tw *teamAppWorld) decide(verb string, grant, uid int64, signedIn time.Time) *httptest.ResponseRecorder {
	return tw.browserAs(http.MethodPost, "/teams/"+strconv.FormatInt(tw.team, 10)+"/connectors/"+strconv.FormatInt(grant, 10)+"/"+verb, uid, signedIn)
}

// TestAListedTeamsChiefDispatchesIntoAnAppRepository: with the team listed,
// B's fresh typed turn has the team chief start a Claude Code agent on the
// team task IN an App repository: the agent is the team's, in that
// repository, on the App's default branch. repos_list, as the team chief,
// lists the App's repositories.
//
// 🔴 RED before slice 4.8: githubApp answered nil to every non-admin
// principal, the team acting principal included, so the start was refused
// with chiefDispatchNoGitHub.
func TestAListedTeamsChiefDispatchesIntoAnAppRepository(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	tw.typed(t, tw.w.b.uid, true, "start a Claude agent on the team task in "+teamAppRepo)
	in := tw.onTask("k-app-repo", true)
	in.Repo = teamAppRepo
	text := tw.dispatch(in)
	if strings.Contains(text, `"error"`) {
		t.Fatalf("a listed team's dispatch into an App repository: %s", truncate(text, 400))
	}
	var owner int64
	var repo, branch string
	if err := tw.w.pool.QueryRow(tw.w.sys, `SELECT owner_id, repo, COALESCE(repo_branch, '') FROM agents WHERE note_id = $1 AND id <> $2`,
		tw.task.ID, tw.chief.ID).Scan(&owner, &repo, &branch); err != nil {
		t.Fatalf("the started agent: %v (answer %s)", err, truncate(text, 300))
	}
	if owner != tw.team || repo != teamAppRepo || branch != "trunk" {
		t.Fatalf("the started agent: owner %d, repo %q, branch %q; want the team %d, %q on the App's default branch trunk",
			owner, repo, branch, tw.team, teamAppRepo)
	}
	if got := tw.w.srv.toolReposList(tw.teamCtx, `{}`); !strings.Contains(got, teamAppRepo) || !strings.Contains(got, teamAppOther) {
		t.Errorf("repos_list as a listed team's chief: %s; want the App's repositories", truncate(got, 300))
	}
}

// TestAnUnlistedTeamAndAMembersOwnPrincipalHaveNoRepositories: a team NOT in
// the list is refused the App repository (chiefDispatchNoGitHub) and starts
// nothing; and with the team listed, B — an operator of it — still has no
// repositories under B's OWN principal (B's personal chief or agent): the
// App is lent to the team's principal, never to its members'.
//
// The unlisted leg is an INVARIANT guard (no team had the App before 4.8);
// the member leg is red against a predicate that admits a listed team's
// MEMBERS (mutation M2).
func TestAnUnlistedTeamAndAMembersOwnPrincipalHaveNoRepositories(t *testing.T) {
	tw := newTeamAppWorld(t, false)
	tw.typed(t, tw.w.b.uid, true, "start a Claude agent on the team task in "+teamAppRepo)
	in := tw.onTask("k-app-unlisted", true)
	in.Repo = teamAppRepo
	if text := tw.dispatch(in); !strings.Contains(text, chiefDispatchNoGitHub) {
		t.Errorf("an unlisted team's dispatch into an App repository: %s; want %q", truncate(text, 400), chiefDispatchNoGitHub)
	}
	if on := tw.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("an unlisted team's dispatch started %+v", on)
	}

	tw.w.srv.ext.GitHubAppTeams = map[int64]bool{tw.team: true}
	tw.w.srv.ext.Connectors = connectors.WithVirtual(tw.w.srv.ext.Connectors, appVirtual{}, githubapp.Admits(tw.w.srv.ext.GitHubAppTeams))
	if why := tw.w.srv.connectedRepo(tw.teamCtx, teamAppRepo); why != "" {
		t.Fatalf("control: the listed team's own principal: %q; want the App repository connected", why)
	}
	for name, via := range map[string]authz.Via{"B's personal agent": authz.ViaAgent, "B's session": authz.ViaSession} {
		bctx := authz.WithPrincipal(context.Background(), tw.w.srv.principalFor(context.Background(), tw.w.b.uid, via))
		if _, in := authz.MustFrom(bctx).TeamScope().RoleIn(tw.team); via == authz.ViaSession && !in {
			t.Fatalf("fixture: B's session carries no membership of the team")
		}
		if why := tw.w.srv.connectedRepo(bctx, teamAppRepo); why != chiefDispatchNoGitHub {
			t.Errorf("%s, an operator of the listed team: %q; want %q (a member's own principal has no repositories)", name, why, chiefDispatchNoGitHub)
		}
		if got := tw.w.srv.toolReposList(bctx, `{}`); strings.Contains(got, teamAppRepo) {
			t.Errorf("%s: repos_list %s; want no repository", name, truncate(got, 200))
		}
	}
}

// TestTheRepoPickerIsDecidedByTheObjectsOwner: /ui/agents/repos?owner=<team>
// lists the repositories THAT owner's agents may use, whoever views it.
//
//   - B (operator) on the listed team's object: the App's repositories — B's
//     own list (no owner) has none. 🔴 RED before 4.8: the picker answered by
//     the viewer, a member, and listed nothing.
//   - V (viewer) and N (non-member) on the team's object: nothing.
//   - The admin (an operator of the team) on the object of a team NOT listed:
//     nothing, though the admin's own list has them. 🔴 RED before 4.8: the
//     admin's own repositories were offered on the team's task, for a
//     dispatch that would be refused.
//   - The team task's edit modal asks for its OWNER's list.
func TestTheRepoPickerIsDecidedByTheObjectsOwner(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	if err := users.NewPG(tw.w.pool).SetTeamMember(tw.w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, tw.team,
		authz.OperatorUserID, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	picker := func(uid int64, owner string) string {
		path := "/ui/agents/repos"
		if owner != "" {
			path += "?owner=" + owner
		}
		rec := tw.browserAs(http.MethodGet, path, uid, now)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s as %d: %d %s", path, uid, rec.Code, truncate(rec.Body.String(), 200))
		}
		return rec.Body.String()
	}
	team := strconv.FormatInt(tw.team, 10)
	if got := picker(tw.w.b.uid, team); !strings.Contains(got, teamAppRepo) {
		t.Errorf("B (operator) on the listed team's object: %s; want %s", truncate(got, 300), teamAppRepo)
	}
	if got := picker(tw.w.b.uid, ""); strings.Contains(got, teamAppRepo) {
		t.Errorf("B's own list (a member): %s; want no repository", truncate(got, 300))
	}
	for who, uid := range map[string]int64{"V (viewer)": tw.v.uid, "N (non-member)": tw.n.uid} {
		if got := picker(uid, team); strings.Contains(got, teamAppRepo) {
			t.Errorf("%s on the team's object: %s; want no repository", who, truncate(got, 300))
		}
	}
	if got := picker(authz.OperatorUserID, ""); !strings.Contains(got, teamAppRepo) {
		t.Fatalf("control: the admin's own list: %s; want %s", truncate(got, 300), teamAppRepo)
	}
	tw.w.srv.ext.GitHubAppTeams = nil
	if got := picker(authz.OperatorUserID, team); strings.Contains(got, teamAppRepo) {
		t.Errorf("the admin on an UNLISTED team's object: %s; want no repository (decided by the owner, not the viewer)", truncate(got, 300))
	}
	tw.w.srv.ext.GitHubAppTeams = map[int64]bool{tw.team: true}

	rec := tw.browserAs(http.MethodGet, "/ui/tasks/"+strconv.FormatInt(tw.task.ID, 10)+"/edit", tw.w.b.uid, now)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `/ui/agents/repos?owner=`+team) {
		t.Errorf("the team task's edit modal: %d; want its picker to ask for the team's list (?owner=%s)", rec.Code, team)
	}
}

// TestAManagerApprovesEachRepositoryOnceForTheTeam: a team agent's `github`
// request, filed through its own token, waits on /teams/{id}/connectors. A,
// a manager with a fresh sign-in, approves it — as the team, onto the App —
// and the approval is the TEAM's for that repository: a second team agent on
// the same repository is approved when it asks; one on another repository
// waits. The decision is in the team's audit log, naming A.
//
// 🔴 RED before 4.8: no team door decided a team's grant (the route did not
// exist), and the virtual github connector did not exist for a team.
func TestAManagerApprovesEachRepositoryOnceForTheTeam(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	one, tok := tw.teamWorkerOn(t, "tapp-one", teamAppRepo)
	id, status := tw.fileGitHub(t, one, tok)
	if status != connectors.StatusPending {
		t.Fatalf("the first request is %s; want pending", status)
	}
	page := tw.browserAs(http.MethodGet, "/teams/"+strconv.FormatInt(tw.team, 10)+"/connectors", tw.w.a.uid, time.Now())
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-approve="`+strconv.FormatInt(id, 10)+`"`) {
		t.Fatalf("A's team connectors page: %d; want Approve on request %d:\n%s", page.Code, id, truncate(page.Body.String(), 600))
	}
	if rec := tw.decide("approve", id, tw.w.a.uid, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("A (manager, fresh) approving: %d %s", rec.Code, truncate(rec.Body.String(), 400))
	}
	if st, src := tw.grant(t, id); st != connectors.StatusApproved || src != connectors.SourceApp {
		t.Fatalf("the approved grant: %s / %s; want approved onto the App", st, src)
	}
	var sgRepo string
	if err := tw.w.pool.QueryRow(tw.w.sys, `SELECT repo FROM connector_standing_grants WHERE owner_id = $1 AND connector_name = 'github'
		AND revoked_at IS NULL`, tw.team).Scan(&sgRepo); err != nil || !strings.EqualFold(sgRepo, teamAppRepo) {
		t.Fatalf("the team's standing grant: %q %v; want one for %s", sgRepo, err, teamAppRepo)
	}
	two, tok2 := tw.teamWorkerOn(t, "tapp-two", teamAppRepo)
	if _, st := tw.fileGitHub(t, two, tok2); st != connectors.StatusApproved {
		t.Errorf("a second team agent on the SAME repository: %s; want approved from the team's approval", st)
	}
	three, tok3 := tw.teamWorkerOn(t, "tapp-three", teamAppOther)
	if _, st := tw.fileGitHub(t, three, tok3); st != connectors.StatusPending {
		t.Errorf("a team agent on ANOTHER repository: %s; want pending (approved per repository)", st)
	}
	if n := tw.teamAudits(t, "connector.approve", tw.w.a.uid); n != 1 {
		t.Errorf("the team's audit: %d connector.approve row(s) by A; want 1", n)
	}
}

// TestATeamConnectorApprovalIsRefusedBelowAFreshManager: the same request,
// approved by B (an operator: not a manager) — 403; by A with a sign-in older
// than the step-up window — 403 asking for a fresh sign-in; by N — the 404 a
// missing team gets; each leaves it pending. A's stale sign-in may still
// DENY (a denial hands out nothing). On a team NOT listed, A's fresh approval
// is refused (409) and nothing is approved.
func TestATeamConnectorApprovalIsRefusedBelowAFreshManager(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	ref, tok := tw.teamWorkerOn(t, "tapp-refuse", teamAppRepo)
	id, _ := tw.fileGitHub(t, ref, tok)
	pending := func(what string) {
		t.Helper()
		if st, _ := tw.grant(t, id); st != connectors.StatusPending {
			t.Fatalf("%s: the request is %s; want it still pending", what, st)
		}
	}
	stale := time.Now().Add(-2 * stepup.Window())
	if rec := tw.decide("approve", id, tw.w.b.uid, time.Now()); rec.Code != http.StatusForbidden {
		t.Errorf("B (operator) approving: %d; want 403", rec.Code)
	}
	pending("B approving")
	rec := tw.decide("approve", id, tw.w.a.uid, stale)
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Muster-Step-Up") != "required" {
		t.Errorf("A approving with a stale sign-in: %d (step-up %q); want 403 asking for a fresh sign-in", rec.Code, rec.Header().Get("X-Muster-Step-Up"))
	}
	pending("A's stale approval")
	if rec := tw.decide("approve", id, tw.n.uid, time.Now()); rec.Code != http.StatusNotFound {
		t.Errorf("N (non-member) approving: %d; want 404", rec.Code)
	}
	pending("N approving")
	if n := tw.teamAudits(t, "connector.approve", tw.w.a.uid); n != 0 {
		t.Errorf("a refused approval wrote %d audit row(s)", n)
	}

	tw.w.srv.ext.GitHubAppTeams = nil
	tw.w.srv.ext.Connectors = connectors.WithVirtual(tw.w.srv.ext.Connectors, appVirtual{}, githubapp.Admits(nil))
	if rec := tw.decide("approve", id, tw.w.a.uid, time.Now()); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "not lent the deployment&#39;s GitHub App") {
		t.Errorf("A approving on an UNLISTED team: %d %s; want 409 saying the team is not lent the App", rec.Code, truncate(rec.Body.String(), 600))
	}
	pending("A approving on an unlisted team")

	if rec := tw.decide("deny", id, tw.w.a.uid, stale); rec.Code != http.StatusOK {
		t.Fatalf("control: A denying with a stale sign-in: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	if st, _ := tw.grant(t, id); st != connectors.StatusDenied {
		t.Fatalf("A's denial left the request %s", st)
	}
}

// TestAListedTeamsApprovalCardOffersChangeRepo: on a team task with no
// repository, a LISTED team's approval card offers "Change repo" — the repo
// card behind it lists the App's repositories as the team — where an unlisted
// team's offers only Approve and Decline
// (TestATeamsApprovalCardHasNoChangeRepo, the control).
//
// 🔴 RED before 4.8: no team's card offered it.
func TestAListedTeamsApprovalCardOffersChangeRepo(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	if tw.task.Repo != "" {
		t.Fatalf("fixture: the team task has repository %q; want none", tw.task.Repo)
	}
	in := tw.onTask("k-app-change-repo", true)
	in.Prompt = "Do it."
	id := heldCard(tw.dispatch(in))
	if id <= 0 {
		t.Fatal("fixture: no card")
	}
	a, err := tw.w.srv.ext.Attention.Get(tw.teamCtx, id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Options, "|") != approveLabel+"|"+changeRepoLabel+"|"+declineLabel {
		t.Fatalf("a listed team's approval card offers %q; want %q, %q and %q", a.Options, approveLabel, changeRepoLabel, declineLabel)
	}

	// B taps "Change repo" in a browser with a fresh sign-in; the chief's
	// next call files the repo card, listing the App's repositories AS THE
	// TEAM; B picks one; the chief's call with it starts the team's agent
	// there.
	bFresh := tw.personCtx(tw.w.b.uid, authz.ViaSession, true)
	if _, err := tw.w.srv.answerAskFrom(tw.personCtx(tw.w.b.uid, authz.ViaSession, false), id, changeRepoLabel, attention.AnswerSource{}); err == nil {
		t.Fatal("B's Change repo tap with a STALE sign-in was taken (the team card's step-up)")
	}
	if _, err := tw.w.srv.answerAskFrom(bFresh, id, changeRepoLabel, attention.AnswerSource{}); err != nil {
		t.Fatalf("B tapping Change repo: %v", err)
	}
	repoCard := heldCard(tw.dispatch(in))
	if repoCard <= 0 || repoCard == id {
		t.Fatalf("after Change repo: card %d; want a new repo card", repoCard)
	}
	rc, err := tw.w.srv.ext.Attention.Get(tw.teamCtx, repoCard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rc.Options, "|"), teamAppRepo) {
		t.Fatalf("the team's repo card offers %q; want the App's repositories", rc.Options)
	}
	if _, err := tw.w.srv.answerAskFrom(bFresh, repoCard, teamAppRepo, attention.AnswerSource{}); err != nil {
		t.Fatalf("B picking %s: %v", teamAppRepo, err)
	}
	in.Repo = teamAppRepo
	if text := tw.dispatch(in); strings.Contains(text, `"error"`) || heldCard(text) > 0 {
		t.Fatalf("the chief's call with the picked repository: %s", truncate(text, 400))
	}
	var repo string
	if err := tw.w.pool.QueryRow(tw.w.sys, `SELECT repo FROM agents WHERE note_id = $1 AND owner_id = $2 AND id <> $3`,
		tw.task.ID, tw.team, tw.chief.ID).Scan(&repo); err != nil || repo != teamAppRepo {
		t.Fatalf("the team's agent started by the repo card: repo %q %v; want %s", repo, err, teamAppRepo)
	}
}
