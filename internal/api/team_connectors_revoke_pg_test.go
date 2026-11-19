package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// TEAM SHARING 4.8, ROUND 0: a MANAGER REVOKES a repository already approved
// for the team (operator ruling: "add a manager revoke in this slice"), and a
// team agent's connector request pushes the team's managers a link to the
// team's own page. Synthetic throughout (teamAppWorld, fanTeam).

// standingFor is the team's live standing grant for repo (0 for none), read
// under System.
func (tw *teamAppWorld) standingFor(t *testing.T, repo string) int64 {
	t.Helper()
	var id int64
	err := tw.w.pool.QueryRow(tw.w.sys, `SELECT coalesce(max(id), 0) FROM connector_standing_grants WHERE owner_id = $1
		AND connector_name = 'github' AND repo = lower($2) AND revoked_at IS NULL`, tw.team, repo).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// live reports whether standing grant id is unrevoked, read under System.
func (tw *teamAppWorld) live(t *testing.T, id int64) bool {
	t.Helper()
	var live bool
	if err := tw.w.pool.QueryRow(tw.w.sys, `SELECT revoked_at IS NULL FROM connector_standing_grants WHERE id = $1`, id).Scan(&live); err != nil {
		t.Fatal(err)
	}
	return live
}

func (tw *teamAppWorld) revoke(standing, uid int64, signedIn time.Time) *httptest.ResponseRecorder {
	return tw.browserAs(http.MethodPost, "/teams/"+strconv.FormatInt(tw.team, 10)+"/connectors/standing/"+strconv.FormatInt(standing, 10)+"/revoke", uid, signedIn)
}

// approvedOn is a team agent on repo whose github request A (a manager, fresh)
// approved: the agent's grant id and the team's standing grant for repo.
func (tw *teamAppWorld) approvedOn(t *testing.T, name, repo string) (grant, standing int64) {
	t.Helper()
	a, tok := tw.teamWorkerOn(t, name, repo)
	id, _ := tw.fileGitHub(t, a, tok)
	if rec := tw.decide("approve", id, tw.w.a.uid, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("fixture: A approving %s: %d %s", repo, rec.Code, truncate(rec.Body.String(), 300))
	}
	sg := tw.standingFor(t, repo)
	if sg == 0 {
		t.Fatalf("fixture: no standing grant for %s after the approval", repo)
	}
	return id, sg
}

// TestAManagerRevokesATeamsApprovedRepository: the team's approved repository
// is listed on /teams/{id}/connectors with Revoke; A (manager, fresh sign-in)
// revokes it — the standing grant ends, the approval it was made from is
// revoked with it, and the team's NEXT agent on that repository asks again.
// The team's audit holds one connector.revoke row naming A.
//
// 🔴 RED at c67e38e4: there was no revoke (the route 404s, the page offers no
// Revoke), so the repository stayed approved for the team.
func TestAManagerRevokesATeamsApprovedRepository(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	grant, sg := tw.approvedOn(t, "trev-one", teamAppRepo)
	page := tw.browserAs(http.MethodGet, "/teams/"+strconv.FormatInt(tw.team, 10)+"/connectors", tw.w.a.uid, time.Now())
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-revoke="`+strconv.FormatInt(sg, 10)+`"`) {
		t.Fatalf("A's team connectors page: %d; want Revoke on standing grant %d:\n%s", page.Code, sg, truncate(page.Body.String(), 800))
	}
	if rec := tw.revoke(sg, tw.w.a.uid, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("A (manager, fresh) revoking: %d %s", rec.Code, truncate(rec.Body.String(), 400))
	}
	if tw.live(t, sg) {
		t.Fatal("after A's revoke the team's standing grant is still live")
	}
	if st, _ := tw.grant(t, grant); st != connectors.StatusRevoked {
		t.Errorf("the approval the standing grant was made from: %s; want revoked with it", st)
	}
	two, tok2 := tw.teamWorkerOn(t, "trev-two", teamAppRepo)
	if _, st := tw.fileGitHub(t, two, tok2); st != connectors.StatusPending {
		t.Errorf("the team's next agent on the revoked repository: %s; want pending (it asks again)", st)
	}
	if n := tw.teamAudits(t, "connector.revoke", tw.w.a.uid); n != 1 {
		t.Errorf("the team's audit: %d connector.revoke row(s) by A; want 1", n)
	}
}

// TestATeamRevokeIsRefusedBelowAFreshManager: B (an operator) — 403 with the
// manager-only refusal; A with a sign-in older than the step-up window — 403
// asking for a fresh sign-in; N (no member) — the 404 a missing team gets.
// Each leaves the standing grant live and writes no revoke audit row.
//
// 🔴 RED at c67e38e4 for the reason the route did not exist (every leg got
// 404 and no Revoke was offered); each refusal's own code and text is what
// the mutation battery breaks.
func TestATeamRevokeIsRefusedBelowAFreshManager(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	_, sg := tw.approvedOn(t, "trev-refuse", teamAppRepo)
	stale := time.Now().Add(-2 * stepup.Window())
	rec := tw.revoke(sg, tw.w.b.uid, time.Now())
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Only a manager of the team") {
		t.Errorf("B (operator) revoking: %d %s; want 403 manager-only", rec.Code, truncate(rec.Body.String(), 300))
	}
	rec = tw.revoke(sg, tw.w.a.uid, stale)
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Muster-Step-Up") != "required" ||
		!strings.Contains(rec.Body.String(), "Revoking a repository approved for the team needs a recent sign-in") {
		t.Errorf("A revoking with a stale sign-in: %d (step-up %q); want 403 asking for a fresh sign-in", rec.Code, rec.Header().Get("X-Muster-Step-Up"))
	}
	if rec := tw.revoke(sg, tw.n.uid, time.Now()); rec.Code != http.StatusNotFound {
		t.Errorf("N (non-member) revoking: %d; want 404", rec.Code)
	}
	if !tw.live(t, sg) {
		t.Fatal("a refused revoke ended the team's standing grant")
	}
	if n := tw.teamAuditsWhere(t, "connector.revoke", tw.w.a.uid, ""); n != 0 {
		t.Errorf("a refused revoke wrote %d audit row(s)", n)
	}
}

// TestATeamRevokeReachesOnlyTheTeamsOwnStandingGrants: through team T's
// route, A (manager of T, fresh) names a standing grant of ANOTHER team and
// one of a PERSON (A's own, personal): each is the 404 a missing one gets —
// row-level security under T's acting principal shows neither — and each
// stays live. The control: T's own is revoked through the same route.
//
// 🔴 RED at c67e38e4 (no route); the RLS legs are what a revoke run under
// System, or as A's own principal, fails.
func TestATeamRevokeReachesOnlyTheTeamsOwnStandingGrants(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	us := users.NewPG(tw.w.pool)
	other, err := us.CreateTeam(tw.w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "other team "+tw.w.suffix, tw.n.uid)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(owner int64, repo string) int64 {
		t.Helper()
		var id int64
		if err := tw.w.pool.QueryRow(tw.w.sys, `INSERT INTO connector_standing_grants (owner_id, connector_name, source, repo)
			VALUES ($1, 'github', 'app', $2) RETURNING id`, owner, repo).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	otherSG := insert(other.ID, "example-org/other-team")
	personSG := insert(tw.w.a.uid, "example-org/a-personal")
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = tw.w.pool.Exec(bg, `DELETE FROM connector_standing_grants WHERE id IN ($1, $2)`, otherSG, personSG)
		_, _ = tw.w.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, other.ID)
		_, _ = tw.w.pool.Exec(bg, `DELETE FROM audit_log WHERE team_id = $1`, other.ID)
		_, _ = tw.w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, other.ID)
	})
	for who, sg := range map[string]int64{"another team's": otherSG, "A's own personal": personSG} {
		if rec := tw.revoke(sg, tw.w.a.uid, time.Now()); rec.Code != http.StatusNotFound {
			t.Errorf("A revoking %s standing grant through T's page: %d %s; want 404", who, rec.Code, truncate(rec.Body.String(), 300))
		}
		if !tw.live(t, sg) {
			t.Errorf("%s standing grant was revoked through T's page", who)
		}
	}
	_, own := tw.approvedOn(t, "trev-own", teamAppRepo)
	if rec := tw.revoke(own, tw.w.a.uid, time.Now()); rec.Code != http.StatusOK || tw.live(t, own) {
		t.Fatalf("control: A revoking T's own standing grant: %d; want it revoked", rec.Code)
	}
}

// TestUnlistingATeamPausesItsApprovalsAndRelistingRestoresThem: the claim the
// docs make (docs/github-app.md, design 4.8). With T's repository approved,
// T is unlisted: the agent's next ask is not approved (the App is no
// connector of T's), while the team's standing grant stays live. T is listed
// again: the same agent's ask is approved from it, and so is a new agent's on
// that repository — nobody approved anything again.
//
// INVARIANT guard (no code of this round changes it): it pins the sentence
// the round corrects ("unsetting the variable re-opens every App approval"
// read as a removal; it is a pause).
func TestUnlistingATeamPausesItsApprovalsAndRelistingRestoresThem(t *testing.T) {
	tw := newTeamAppWorld(t, true)
	one, tok := tw.teamWorkerOn(t, "tpause-one", teamAppRepo)
	id, _ := tw.fileGitHub(t, one, tok)
	if rec := tw.decide("approve", id, tw.w.a.uid, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("fixture: A approving: %d", rec.Code)
	}
	sg := tw.standingFor(t, teamAppRepo)
	tw.list(false)
	if _, st := tw.fileGitHub(t, one, tok); st != connectors.StatusPending {
		t.Errorf("unlisted, the team agent's ask: %s; want its grant re-opened to pending (paused)", st)
	}
	if !tw.live(t, sg) {
		t.Fatal("unlisting the team ended its standing grant; want it kept (a pause)")
	}
	tw.list(true)
	if _, st := tw.fileGitHub(t, one, tok); st != connectors.StatusApproved {
		t.Errorf("listed again, the same agent's ask: %s; want approved from the team's standing grant", st)
	}
	two, tok2 := tw.teamWorkerOn(t, "tpause-two", teamAppRepo)
	if _, st := tw.fileGitHub(t, two, tok2); st != connectors.StatusApproved {
		t.Errorf("listed again, a new agent on the repository: %s; want approved", st)
	}
}

// TestATeamAgentsConnectorRequestPushesTheTeamsManagersItsPage: a team
// agent's request (its owner the team: the team acting principal) is pushed
// to the team's MANAGERS (ana) only — ben, an operator, cannot decide it —
// and links the team's own page, /teams/{id}/connectors. The control: a
// person's own request still links /connectors and reaches that person.
//
// 🔴 RED at c67e38e4: the team's push went to every operator and manager and
// linked /connectors, which is empty for a member (row-level security hides
// the team's rows).
func TestATeamAgentsConnectorRequestPushesTheTeamsManagersItsPage(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	teamCtx := authz.WithPrincipal(context.Background(), teamActingPrincipal(f.team, authz.ViaAgent))
	w.srv.onConnectorRequested(teamCtx, connectors.Grant{ID: 990401, AgentName: "team-worker", Connector: "github"})
	anaCtx := authz.WithPrincipal(context.Background(), w.srv.principalFor(context.Background(), f.ana, authz.ViaAgent))
	w.srv.onConnectorRequested(anaCtx, connectors.Grant{ID: 990402, AgentName: "ana-worker", Connector: "github"})
	w.srv.WaitForPushes()
	if got := rec.to("connector-990401", f.ana, f.ben, f.cal); !sameIDs(got, ids(f.ana)) {
		t.Errorf("the team agent's connector request reached %v; want only the manager ana %d", got, f.ana)
	}
	url := func(p push.Payload) string {
		raw, _ := json.Marshal(p)
		var v struct {
			Data map[string]string `json:"data"`
		}
		_ = json.Unmarshal(raw, &v)
		return v.Data["url"]
	}
	// The page, at the request's card (connectorRequestLink).
	want := "/teams/" + strconv.FormatInt(f.team, 10) + "/connectors#connector-request-990401"
	for _, p := range rec.sent(f.ana, "connector-990401", false) {
		if got := url(p); got != want {
			t.Errorf("the team request's push links %q; want the team's page %q", got, want)
		}
	}
	personal := rec.sent(f.ana, "connector-990402", false)
	if len(personal) != 1 || url(personal[0]) != "/connectors#connector-request-990402" {
		t.Errorf("control: ana's own request: %d push(es) linking %v; want one linking /connectors#connector-request-990402", len(personal), personal)
	}
}
