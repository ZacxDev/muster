package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// THE TEAM MANAGER'S DOOR (team sharing phase 4, slice 4.5; team_admin.go):
// the team-agent walk's team T (A manages it, B operates in it, V views it,
// N is in no team), its running chief, and the iso world's REAL credential
// plane — so whether a team is listed in MUSTER_TEAMS_ON_SERVER_POOL decides
// what a start, a provision, an account choice actually get.

// poolPlane is the iso world's credential plane, the one its Kinds read.
func poolPlane(t *testing.T, w *isoWorld) *credentials.Plane {
	t.Helper()
	k, ok := w.srv.ext.Kinds.(isoKinds)
	if !ok {
		t.Fatalf("fixture: the iso world's Kinds is %T", w.srv.ext.Kinds)
	}
	return k.plane
}

// teamWorker is a claude-code agent team owns, stopped.
func (tw *teamAgentWalk) teamWorker(t *testing.T, name string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(tw.w.pool).Create(tw.teamCtx, agents.Agent{Name: name + "-" + tw.w.suffix, Namespace: "ns-" + name + "-" + tw.w.suffix,
		Status: agents.StatusStopped, Kind: agents.KindClaudeCode, CCAccount: isoEnvAccount, HooksToken: "old-hooks-" + name + "-" + tw.w.suffix})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// teamAudits is how many audit rows of action team T holds by user by.
func (tw *teamAgentWalk) teamAudits(t *testing.T, action string, by int64) int {
	t.Helper()
	var n int
	if err := tw.w.pool.QueryRow(tw.w.sys, `SELECT count(*) FROM audit_log WHERE team_id = $1 AND action = $2 AND user_id = $3`,
		tw.team, action, by).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAManagersLifecycleOnATeamAgentRunsAsTheTeam: A, a manager of T, starts,
// stops and deletes a team agent — each through the provisioner AS THE TEAM
// (the acting uid is T, the door authz.ViaTeamAdmin, never A), each with an
// audit row naming A owned by T. B (operator) and V (viewer) get 403 with the
// manager-only refusal, and their attempts reach no instance. The team chief
// itself is refused deletion. The pool decides the start: with T unlisted
// the start is refused for want of a Claude account; listed, it runs.
//
// 🔴 RED at origin/main 2ddbd7ba: every lifecycle route on a team agent was
// refused to every member (teamAgentRefused), A included — 403.
func TestAManagersLifecycleOnATeamAgentRunsAsTheTeam(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	plane := poolPlane(t, w)
	worker := tw.teamWorker(t, "tlife")
	id := strconv.FormatInt(worker.ID, 10)

	for _, who := range []isoUser{w.b, tw.v} {
		for _, r := range []string{"POST /agents/" + id + "/start", "POST /agents/" + id + "/stop", "DELETE /agents/" + id} {
			m, p, _ := strings.Cut(r, " ")
			rec := w.do(w.h, m, p, "", cred{sessionUser: who.uid})
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), TeamAgentManagerOnly) {
				t.Errorf("%s %s → %d %q; want 403 %q", who.name, r, rec.Code, truncate(rec.Body.String(), 120), TeamAgentManagerOnly)
			}
		}
	}
	if n := w.prov.actedOn(worker.ID, tw.team) + w.prov.actedOn(worker.ID, w.b.uid) + w.prov.actedOn(worker.ID, tw.v.uid); n != 0 {
		t.Fatalf("a non-manager's lifecycle reached the instance %d time(s)", n)
	}

	// Unlisted: the team has no Claude account, so the start is refused.
	plane.TeamsOnServerPool = nil
	if rec := w.do(w.h, "POST", "/agents/"+id+"/start", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), credentials.NeedClaude) {
		t.Fatalf("A starting an UNLISTED team's agent: %d %q; want 409 naming the missing Claude account", rec.Code, truncate(rec.Body.String(), 200))
	}
	plane.TeamsOnServerPool = map[int64]bool{tw.team: true}
	t.Cleanup(func() { plane.TeamsOnServerPool = nil })

	for _, r := range []string{"POST /agents/" + id + "/start", "POST /agents/" + id + "/stop"} {
		m, p, _ := strings.Cut(r, " ")
		if rec := w.do(w.h, m, p, "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusOK {
			t.Fatalf("A (manager) %s: %d %q; want 200", r, rec.Code, truncate(rec.Body.String(), 200))
		}
	}
	if got := w.prov.actedOn(worker.ID, tw.team); got != 2 {
		t.Errorf("the start and the stop acted as the team %d time(s); want 2", got)
	}
	if got := w.prov.actedOn(worker.ID, w.a.uid); got != 0 {
		t.Errorf("LEAK: the manager's OWN principal acted on the team's agent %d time(s)", got)
	}
	if v := w.prov.lastVia(worker.ID); v != authz.ViaTeamAdmin {
		t.Errorf("the lifecycle ran through door %q; want %q", v, authz.ViaTeamAdmin)
	}
	if rec := w.do(w.h, "DELETE", "/agents/"+strconv.FormatInt(tw.chief.ID, 10), "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusConflict {
		t.Errorf("A deleting the TEAM CHIEF: %d; want 409 (a chief is never deleted here)", rec.Code)
	}
	if rec := w.do(w.h, "DELETE", "/agents/"+id, "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusOK {
		t.Fatalf("A (manager) deleting the team's worker: %d %q", rec.Code, truncate(rec.Body.String(), 200))
	}
	w.srv.destroyInFlight.Wait()
	if got := w.prov.actedOn(worker.ID, tw.team); got != 3 {
		t.Errorf("the delete did not act as the team (%d actions as the team; want 3)", got)
	}
	for _, action := range []string{"agent.start", "agent.stop", "agent.destroy"} {
		if n := tw.teamAudits(t, action, w.a.uid); n != 1 {
			t.Errorf("audit: %d %q row(s) by manager A owned by the team; want 1", n, action)
		}
	}
}

// TestOnlyAFreshManagerActsAsTheTeam: teamAdminContext re-reads the person's
// role. A principal that CLAIMS to manage T — memberships the database no
// longer holds (B is an operator), or a non-member's forged claim — is
// refused; so is the manager on a door that carries no memberships (a
// personal API token). The control is A on a session: the team acting
// principal through authz.ViaTeamAdmin.
func TestOnlyAFreshManagerActsAsTheTeam(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	claim, err := authz.NewMemberships([]authz.Membership{{Team: tw.team, Role: authz.TeamManager}})
	if err != nil {
		t.Fatal(err)
	}
	stale := func(uid int64, via authz.Via) context.Context {
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: via, Teams: claim})
	}
	for name, ctx := range map[string]context.Context{
		"B, an operator, with a stale manager claim": stale(w.b.uid, authz.ViaSession),
		"N, a non-member, with a forged claim":       stale(tw.n.uid, authz.ViaSession),
		"A on a personal API token":                  stale(w.a.uid, authz.ViaToken),
	} {
		if _, err := w.srv.teamAdminContext(ctx, tw.team); !errors.Is(err, ErrNotTeamManager) {
			t.Errorf("%s: %v; want ErrNotTeamManager", name, err)
		}
	}
	got, err := w.srv.teamAdminContext(stale(w.a.uid, authz.ViaSession), tw.team)
	if err != nil {
		t.Fatalf("control: A (manager): %v", err)
	}
	if p := authz.MustFrom(got); p != (authz.Principal{UserID: tw.team, Team: true, Role: authz.RoleMember, Via: authz.ViaTeamAdmin}) {
		t.Fatalf("control: A acts as %+v; want the team acting principal through %s", p, authz.ViaTeamAdmin)
	}
}

// TestATeamManagerRotatesATeamAgentsCredentialsIntoAgentSecretsOnly: A rotates
// a team agent's credentials through the REAL adapter and agents store. The
// new hooks token and turn credential are in agent_secrets ONLY — the shared
// agents row's legacy columns stay '' (0131's agents_team_no_secret) — the old
// token no longer identifies the agent and the new one does, and the team's
// audit log names A. B's rotate is refused and changes nothing. The CONTROL
// is A's own agent: rotated the same way, its legacy column carries the new
// token too (a person's agent is dual-written), so the blank is the trigger's.
//
// 🔴 RED at origin/main 2ddbd7ba: there was no rotation route (404).
func TestATeamManagerRotatesATeamAgentsCredentialsIntoAgentSecretsOnly(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	store := agents.NewPG(w.pool)
	adapter, err := agentprovision.New(agentprovision.Config{Driver: provision.MustNewNoop(), Store: store,
		Spec:               agentspec.Config{ImageRepo: "registry.example.test/muster/agent-runtime", APIBaseURL: "http://muster.example.test:8105"},
		KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	w.srv.ext.Provisioner = adapter
	worker := tw.teamWorker(t, "trot")
	oldTok := "old-hooks-trot-" + w.suffix
	if err := store.SetGatewaySecret(tw.teamCtx, worker.ID, "old-turn-"+w.suffix); err != nil {
		t.Fatal(err)
	}
	type creds struct{ rowTok, rowSecret, tok, secret string }
	read := func(id int64) creds {
		var c creds
		if err := w.pool.QueryRow(w.sys, `SELECT a.hooks_token, a.gateway_secret, coalesce(s.hooks_token, ''), coalesce(s.gateway_secret, '')
			FROM agents a LEFT JOIN agent_secrets s ON s.agent_id = a.id WHERE a.id = $1`, id).Scan(&c.rowTok, &c.rowSecret, &c.tok, &c.secret); err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := read(worker.ID)
	if before.tok != oldTok || before.rowTok != "" {
		t.Fatalf("fixture: %+v", before)
	}
	path := "/agents/" + strconv.FormatInt(worker.ID, 10) + "/credentials/rotate"
	if rec := w.do(w.h, "POST", path, "", cred{sessionUser: w.b.uid}); rec.Code != http.StatusForbidden {
		t.Errorf("B (operator) rotating: %d; want 403", rec.Code)
	}
	if got := read(worker.ID); got != before {
		t.Fatalf("B's refused rotate changed the credentials: %+v → %+v", before, got)
	}
	if rec := w.do(w.h, "POST", path, "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusOK {
		t.Fatalf("A (manager) rotating: %d %q", rec.Code, truncate(rec.Body.String(), 300))
	}
	after := read(worker.ID)
	if after.rowTok != "" || after.rowSecret != "" {
		t.Errorf("LEAK: the shared agents row carries a credential after the rotate (hooks %d bytes, turn %d bytes); want both ''",
			len(after.rowTok), len(after.rowSecret))
	}
	if after.tok == "" || after.tok == before.tok || after.secret == "" || after.secret == before.secret || after.tok == after.secret {
		t.Errorf("agent_secrets after the rotate: hooks changed %v (%d bytes), turn changed %v (%d bytes); want two new, distinct credentials",
			after.tok != before.tok, len(after.tok), after.secret != before.secret, len(after.secret))
	}
	if _, err := store.GetByHooksToken(w.sys, oldTok); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("the OLD hooks token still identifies an agent: %v", err)
	}
	if got, err := store.GetByHooksToken(w.sys, after.tok); err != nil || got.ID != worker.ID {
		t.Errorf("the new hooks token identifies %d (%v); want the agent %d", got.ID, err, worker.ID)
	}
	if n := tw.teamAudits(t, "agent.credentials.rotate", w.a.uid); n != 1 {
		t.Errorf("audit: %d rotate row(s) by A owned by the team; want 1", n)
	}

	// CONTROL: A's own agent, dual-written.
	mine, err := store.Create(asUser(w.a.uid), agents.Agent{Name: "arot-" + w.suffix, Namespace: "ns-arot-" + w.suffix,
		Status: agents.StatusStopped, Kind: agents.KindClaudeCode, CCAccount: isoEnvAccount, HooksToken: "a-own-old-" + w.suffix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.pool.Exec(w.sys, `DELETE FROM agents WHERE id = $1`, mine.ID) })
	if rec := w.do(w.h, "POST", "/agents/"+strconv.FormatInt(mine.ID, 10)+"/credentials/rotate", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusOK {
		t.Fatalf("control: A rotating their own agent: %d %q", rec.Code, truncate(rec.Body.String(), 300))
	}
	if c := read(mine.ID); c.rowTok == "" || c.rowTok != c.tok || c.tok == "a-own-old-"+w.suffix {
		t.Errorf("control: a person's rotated agent: row token %d bytes, secrets token %d bytes, equal %v; want the new token in both",
			len(c.rowTok), len(c.tok), c.rowTok == c.tok)
	}
	if n := tw.teamAudits(t, "agent.credentials.rotate", w.a.uid); n != 1 {
		t.Errorf("audit: a person's own rotate wrote a TEAM row (%d rows now; want still 1)", n)
	}
}

// newChieflessTeam is a second team in the walk's world with no chief: A
// manages it, B operates in it, V views it.
func (tw *teamAgentWalk) newChieflessTeam(t *testing.T) int64 {
	t.Helper()
	w := tw.w
	store := users.NewPG(w.pool)
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team, err := store.CreateTeam(w.sys, by, "chief team "+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	for uid, role := range map[int64]authz.TeamRole{w.b.uid: authz.TeamOperator, tw.v.uid: authz.TeamViewer} {
		if err := store.SetTeamMember(w.sys, by, team.ID, uid, role); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM chat_messages WHERE owner_id = $1`, `DELETE FROM chat_sessions WHERE owner_id = $1`,
			`DELETE FROM agent_outcomes WHERE owner_id = $1`, `DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = w.pool.Exec(w.sys, q, team.ID)
		}
	})
	return team.ID
}

// teamChiefRow is team's chief row (read under System), found false.
func (tw *teamAgentWalk) teamChiefRow(t *testing.T, team int64) (agents.Agent, bool) {
	t.Helper()
	a, err := agents.NewPG(tw.w.pool).GetByName(tw.w.sys, agents.ChiefNameFor(team))
	if errors.Is(err, pgx.ErrNoRows) {
		return agents.Agent{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return a, true
}

// TestATeamManagerProvisionsTheTeamChiefAsTheTeam: on a team listed in
// MUSTER_TEAMS_ON_SERVER_POOL, A's provision creates chief-<team>: owned by
// the TEAM, a Claude Code agent on the server pool's account, dispatched as
// the team through authz.ViaTeamAdmin; the page tells every member where it
// runs; A's stop acts as the team; both are in the team's audit log. B and V
// are refused (403) and N gets the 404 a missing team gets — and none of
// their attempts creates a row.
//
// 🔴 RED at origin/main 2ddbd7ba: /teams/{id}/chief did not exist (404).
func TestATeamManagerProvisionsTheTeamChiefAsTheTeam(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	team := tw.newChieflessTeam(t)
	plane := poolPlane(t, w)
	plane.TeamsOnServerPool = map[int64]bool{team: true}
	t.Cleanup(func() { plane.TeamsOnServerPool = nil })
	base := "/teams/" + strconv.FormatInt(team, 10) + "/chief"

	for _, c := range []struct {
		who  isoUser
		want int
	}{{w.b, http.StatusForbidden}, {tw.v, http.StatusForbidden}, {tw.n, http.StatusNotFound}} {
		if rec := w.do(w.h, "POST", base+"/provision", "", cred{sessionUser: c.who.uid}); rec.Code != c.want {
			t.Errorf("%s provisioning the team chief: %d; want %d", c.who.name, rec.Code, c.want)
		}
	}
	if _, has := tw.teamChiefRow(t, team); has {
		t.Fatal("a non-manager's provision created the team chief")
	}
	if rec := w.do(w.h, "GET", base, "", cred{sessionUser: tw.n.uid}); rec.Code != http.StatusNotFound {
		t.Errorf("N (not a member) reading the team chief's page: %d; want 404", rec.Code)
	}

	if rec := w.do(w.h, "POST", base+"/provision", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("A (manager) provisioning: %d %q", rec.Code, truncate(rec.Body.String(), 300))
	}
	chief, has := tw.teamChiefRow(t, team)
	if !has {
		t.Fatal("A's provision created no chief-<team>")
	}
	if chief.OwnerID != team || agents.ResolveKind(chief.Kind) != agents.KindClaudeCode || chief.CCAccount != isoEnvAccount ||
		chief.Name != agents.ChiefNameFor(team) || chief.DisplayName != ChiefDisplayName {
		t.Errorf("the team chief: name %q owner %d kind %q account %q display %q; want %q owned by the team %d, claude-code on the pool's %q",
			chief.Name, chief.OwnerID, chief.Kind, chief.CCAccount, chief.DisplayName, agents.ChiefNameFor(team), team, isoEnvAccount)
	}
	if got := w.prov.actedOn(chief.ID, team); got != 1 || w.prov.lastVia(chief.ID) != authz.ViaTeamAdmin {
		t.Errorf("the dispatch acted as the team %d time(s) through %q; want once through %q", got, w.prov.lastVia(chief.ID), authz.ViaTeamAdmin)
	}
	if got := w.prov.actedOn(chief.ID, w.a.uid); got != 0 {
		t.Errorf("LEAK: the manager's own principal dispatched the team chief %d time(s)", got)
	}
	rec := w.do(w.h, "GET", base, "", cred{sessionUser: tw.v.uid})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Runs on the server pool, account "+isoEnvAccount) ||
		strings.Contains(rec.Body.String(), `id="team-chief-controls"`) {
		t.Errorf("V (viewer) reading the page: %d; want 200 naming the pool account, with no manager controls", rec.Code)
	}
	if rec := w.do(w.h, "POST", base+"/stop", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("A stopping the team chief: %d", rec.Code)
	}
	if got := w.prov.actedOn(chief.ID, team); got != 2 {
		t.Errorf("the stop did not act as the team (%d actions; want 2)", got)
	}
	tw2 := &teamAgentWalk{w: w, team: team}
	for _, action := range []string{"chief.provision", "chief.stop"} {
		if n := tw2.teamAudits(t, action, w.a.uid); n != 1 {
			t.Errorf("audit: %d %q row(s) by A owned by the team; want 1", n, action)
		}
	}
}

// claudeOff is the iso world's kinds with the claude-code kind not enabled.
type claudeOff struct{ isoKinds }

func (claudeOff) Enabled() []string { return []string{agents.KindGateway} }

// TestTheTeamChiefIsRefusedWithItsReason: a manager's provision is refused,
// naming why and creating nothing, on a team NOT on the server pool, on the
// Docker Compose install, and where the claude-code kind is not enabled (a
// team's chief is never a gateway agent). The control is the same team once
// listed: provisioned.
func TestTheTeamChiefIsRefusedWithItsReason(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	team := tw.newChieflessTeam(t)
	plane := poolPlane(t, w)
	t.Cleanup(func() { plane.TeamsOnServerPool = nil })
	base := "/teams/" + strconv.FormatInt(team, 10) + "/chief"
	provision := func() (int, string) {
		rec := w.do(w.h, "POST", base+"/provision", "", cred{sessionUser: w.a.uid})
		return rec.Code, rec.Body.String()
	}

	plane.TeamsOnServerPool = nil
	if code, body := provision(); code != http.StatusConflict || !strings.Contains(body, htmlText(TeamChiefPoolRefusal)) {
		t.Errorf("an unlisted team: %d; want 409 with the pool refusal", code)
	}
	plane.TeamsOnServerPool = map[int64]bool{team: true}

	kinds := w.srv.ext.Kinds
	w.srv.ext.Kinds = claudeOff{kinds.(isoKinds)}
	if code, body := provision(); code != http.StatusConflict || !strings.Contains(body, htmlText(TeamChiefKindRefusal)) {
		t.Errorf("no claude-code kind: %d; want 409 with the kind refusal", code)
	}
	w.srv.ext.Kinds = kinds

	w.srv.ext.ComposeChief = &ComposeChief{}
	if code, body := provision(); code != http.StatusConflict || !strings.Contains(body, htmlText(TeamChiefComposeRefusal)) {
		t.Errorf("the Compose install: %d; want 409 with the Compose refusal", code)
	}
	w.srv.ext.ComposeChief = nil
	if _, has := tw.teamChiefRow(t, team); has {
		t.Fatal("a refused provision created the team chief")
	}

	if code, body := provision(); code != http.StatusSeeOther {
		t.Fatalf("control: the listed team: %d %q", code, truncate(body, 300))
	}
	if _, has := tw.teamChiefRow(t, team); !has {
		t.Fatal("control: the listed team's provision created nothing")
	}
}

// htmlText is s as the page renders it (html.EscapeString's apostrophe).
func htmlText(s string) string { return strings.ReplaceAll(s, "'", "&#39;") }

// TestATeamsChiefIsRefusedAHostAGatewayAndAWorkerPool: agent_dispatch from a
// TEAM's chief (its tool call runs as the team acting principal) refuses a
// host target, the gateway kind and a worker pool — each with its own
// sentence, before anything is reserved — because a team's agents are pod
// Claude Code agents in phase 4 (team hosts are phase 2b). The CONTROL is a
// person's chief on the same deployment with the same arguments: none of the
// team refusals.
//
// 🔴 RED at origin/main 2ddbd7ba: a team chief's host target went on to the
// host-dispatch flow, and its gateway target to the kind checks.
func TestATeamsChiefIsRefusedAHostAGatewayAndAWorkerPool(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	w.srv.ext.TaskRouting = &TaskRouting{DispatchOwners: map[int64]bool{tw.team: true, w.a.uid: true}}
	w.srv.ext.ChiefDispatch = plugins.NewPG(w.pool)
	personCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: w.a.uid, Role: authz.RoleMember, Via: authz.ViaAgent})
	personChief := agents.Agent{ID: tw.chief.ID + 1000000, Name: agents.ChiefNameFor(w.a.uid), OwnerID: w.a.uid, Kind: agents.KindClaudeCode}
	var reserved int
	count := func() int {
		var n int
		if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM agents WHERE owner_id = $1`, tw.team).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	reserved = count()
	for _, c := range []struct {
		name string
		in   agentDispatchIn
		want string
	}{
		{"a host target", agentDispatchIn{Target: "host/devbox/claude", Prompt: "p", RequestKey: "k1"}, agentDispatchTeamHost},
		{"mirrored", agentDispatchIn{Target: "mirrored", Prompt: "p", RequestKey: "k2"}, agentDispatchTeamHost},
		{"the gateway kind", agentDispatchIn{Target: "pod/" + agents.KindGateway, Prompt: "p", RequestKey: "k3"}, agentDispatchTeamGateway},
		{"a worker pool", agentDispatchIn{Target: "pod/" + agents.KindClaudeCode, WorkerPool: "lane", Prompt: "p", RequestKey: "k4"}, agentDispatchTeamPool},
	} {
		if got := w.srv.toolAgentDispatch(tw.teamCtx, tw.chief, c.in); !strings.Contains(got, c.want) {
			t.Errorf("team chief, %s: %s; want %q", c.name, truncate(got, 300), c.want)
		}
		if got := w.srv.toolAgentDispatch(personCtx, personChief, c.in); strings.Contains(got, c.want) {
			t.Errorf("control: a person's chief, %s, got the team refusal: %s", c.name, truncate(got, 300))
		}
	}
	if n := count(); n != reserved {
		t.Errorf("the refused dispatches created %d team agent(s)", n-reserved)
	}
}

// TestRotatingCredentialsNeedsAFreshSignIn: the rotate route refuses a
// request whose sign-in is not within the step-up window — for the manager on
// a team agent — with the step-up header, and rotates nothing. The CONTROL is
// the same request with a fresh sign-in.
func TestRotatingCredentialsNeedsAFreshSignIn(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	worker := tw.teamWorker(t, "tstep")
	req := func(fresh bool) *httptest.ResponseRecorder {
		ctx := authz.WithPrincipal(context.Background(), w.srv.principalFor(context.Background(), w.a.uid, authz.ViaSession))
		if fresh {
			ctx = stepup.WithAuthTime(ctx, time.Now())
		} else {
			ctx = stepup.WithAuthTime(ctx, time.Now().Add(-2*stepup.Window()))
		}
		r := httptest.NewRequest("POST", "/agents/"+strconv.FormatInt(worker.ID, 10)+"/credentials/rotate", nil).WithContext(ctx)
		r.SetPathValue("id", strconv.FormatInt(worker.ID, 10))
		rec := httptest.NewRecorder()
		w.srv.handleAgentRotateCredentials(rec, r)
		return rec
	}
	if rec := req(false); rec.Code != http.StatusForbidden || rec.Header().Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale sign-in: %d (step-up header %q); want 403 asking for a fresh sign-in", rec.Code, rec.Header().Get("X-Muster-Step-Up"))
	}
	if n := w.prov.actedOn(worker.ID, tw.team); n != 0 {
		t.Fatalf("a stale sign-in's rotate reached the provisioner %d time(s)", n)
	}
	if rec := req(true); rec.Code != http.StatusOK {
		t.Fatalf("control: a fresh sign-in: %d %q", rec.Code, truncate(rec.Body.String(), 200))
	}
	if n := w.prov.actedOn(worker.ID, tw.team); n != 1 {
		t.Fatalf("control: the fresh rotate acted as the team %d time(s); want 1", n)
	}
}
