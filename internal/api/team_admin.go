package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// A TEAM MANAGER'S DOOR (team sharing phase 4, slice 4.5;
// docs/design/team-sharing.md §2). A manager of a team, signed in, acts on
// what the TEAM owns — its agents' lifecycle (start, stop, delete, credential
// rotation) and its chief (/teams/{id}/chief) — AS THE TEAM: the team acting
// principal through authz.ViaTeamAdmin. So the credentials the provisioner
// picks, the budget, the rows written and the account the pod runs on are the
// team's, never the manager's own.
//
//   - teamAdminContext is the ONE site that mints authz.ViaTeamAdmin, after a
//     FRESH read of the person's role (manager of that team) — so a manager
//     removed or downgraded since their page loaded is refused, not obeyed.
//   - lifecycleActingContext is the agent lifecycle routes' use of it: the
//     owner of an agent acts as themselves, as always; a member on a team
//     agent acts as the team or not at all (admit's teamAgentManage already
//     refused an operator and a viewer at the door).
//   - Every manager action writes an audit_log row owned by the team, naming
//     the manager (auditTeam), as phase 3's charter edits do.
// ---------------------------------------------------------------------------

// ErrNotTeamManager is a manager's door used by someone who is not — or is no
// longer, on a fresh read — a manager of the team.
var ErrNotTeamManager = errors.New("only a manager of the team does that")

// teamAdminContext is ctx acting AS TEAM team through the manager's door
// (authz.ViaTeamAdmin): THE ONE SITE that mints it.
//
// 🔴 THE PERSON'S ROLE IS READ AGAIN HERE, FRESH. The request's memberships
// were read when it was admitted; this reads them again, under System
// (accountOfChecked), and requires manager of team. An unreadable account is a
// refusal. The door must be a person's (Principal.TeamScope: a session or a
// paired phone): a machine door never carries memberships, so never acts as a
// team here.
func (s *Server) teamAdminContext(ctx context.Context, team int64) (context.Context, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.Team || team <= 0 {
		return ctx, ErrNotTeamManager
	}
	if _, in := p.TeamScope().RoleIn(team); !in {
		return ctx, ErrNotTeamManager
	}
	acct, err := s.accountOfChecked(ctx, p.UserID)
	if err != nil {
		return ctx, fmt.Errorf("your team role could not be read just now: %w", err)
	}
	if role, in := acct.Teams.RoleIn(team); !in || role != authz.TeamManager {
		return ctx, ErrNotTeamManager
	}
	return authz.WithPrincipal(ctx, teamActingPrincipal(team, authz.ViaTeamAdmin)), nil
}

// lifecycleActingContext is the context an agent lifecycle route (start, stop,
// delete, rotate) runs agent id under, and the team it acts for (0 when the
// caller owns the agent). The agent is read as the caller (row-level
// security): one the caller cannot see answers pgx.ErrNoRows, for the route's
// own 404 or ghost answer.
//
// It costs nothing — no read — for a principal with no team memberships on its
// door (refuseTeamAgentRoute's rule): such a principal cannot see a team agent,
// so whatever it names is its own or nothing, and the route proceeds as it
// always did (a.ID 0: "not read here").
func (s *Server) lifecycleActingContext(ctx context.Context, id int64) (context.Context, int64, agents.Agent, error) {
	if s.ext.Agents == nil {
		return ctx, 0, agents.Agent{}, nil
	}
	if p, ok := authz.From(ctx); !ok || p.All() || p.TeamScope() == "" {
		return ctx, 0, agents.Agent{}, nil
	}
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		return ctx, 0, agents.Agent{}, err
	}
	if !memberOnTeamAgent(ctx, a) {
		return ctx, 0, a, nil
	}
	tctx, err := s.teamAdminContext(ctx, a.OwnerID)
	if err != nil {
		return ctx, 0, a, err
	}
	return tctx, a.OwnerID, a, nil
}

// teamLifecycle is the lifecycle routes' prologue: r's context to run the
// action under, and the team it acts for. It answers the request itself and
// reports false on a refusal: 403 for a member who is not a manager on a
// fresh read, 500 for a read that failed. A missing or invisible agent passes
// through unchanged (the route's own answer, as before).
func (s *Server) teamLifecycle(w http.ResponseWriter, r *http.Request) (*http.Request, int64, agents.Agent, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return r, 0, agents.Agent{}, true
	}
	ctx, team, a, err := s.lifecycleActingContext(r.Context(), id)
	switch {
	case err == nil:
		return r.WithContext(ctx), team, a, true
	case errors.Is(err, pgx.ErrNoRows):
		return r, 0, a, true
	case errors.Is(err, ErrNotTeamManager):
		http.Error(w, TeamAgentManagerOnly, http.StatusForbidden)
	default:
		s.logger.Printf("agents: lifecycle %d: %v", id, err)
		http.Error(w, "could not check your team role; nothing was done", http.StatusInternalServerError)
	}
	return r, 0, a, false
}

// auditTeamLifecycle writes a manager's lifecycle action on team agent a to
// the team's audit log (as the MANAGER: actorOf the original request).
func (s *Server) auditTeamLifecycle(r *http.Request, team int64, action string, a agents.Agent) {
	if team <= 0 {
		return
	}
	s.auditTeam(r.Context(), actorOf(r), team, action, "agent:"+a.Name, map[string]any{"agent_id": a.ID})
}

// CredentialRotator is the optional provisioner capability that rotates an
// agent's credentials (agentprovision.Adapter.RotateCredentials): a new hooks
// token and, for a claude-code agent, a new turn credential, written through
// the agents store and re-applied to a live instance.
type CredentialRotator interface {
	RotateCredentials(ctx context.Context, agentID int64) error
}

// handleAgentRotateCredentials is POST /agents/{id}/credentials/rotate: the
// remedy for a credential printed where it should not be (the team trust
// model: "team members have to trust each other", docs/chief.md). The owner
// rotates their own agent; a MANAGER rotates a team agent, as the team. Both
// need a fresh sign-in (stepup.Fresh).
//
// 🔴 FOR A TEAM AGENT THE NEW CREDENTIALS LAND IN agent_secrets ONLY: migration
// 0131's agents_team_no_secret trigger blanks the shared row's legacy columns
// on every write, the rotation's included. It answers once the store holds the
// new credentials, so the old token is refused from the moment this answers.
func (s *Server) handleAgentRotateCredentials(w http.ResponseWriter, r *http.Request) {
	actor := r
	r, team, a, ok := s.teamLifecycle(w, r)
	if !ok {
		return
	}
	if a.ID == 0 && s.ext.Agents != nil {
		// Not read by the prologue (a principal with no team memberships):
		// the caller's own agent, or nothing.
		if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
			a, _ = s.ext.Agents.Get(r.Context(), id)
		}
	}
	if a.ID == 0 {
		http.NotFound(w, r)
		return
	}
	// 🔴 A FRESH SIGN-IN, FOR THE OWNER AND THE MANAGER ALIKE. Plan §6 lists
	// no step-up for start, stop or delete, and those take none; it is silent on
	// rotation, so rotation takes the step-up the plan gives a manager's other
	// credential decision (approving a team agent's grants): a new identity for
	// an agent is worth more than an ordinary signed-in request.
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		refuseStale(w)
		return
	}
	rot, ok := s.ext.Provisioner.(CredentialRotator)
	if !ok {
		http.Error(w, "this deployment's provisioner cannot rotate an agent's credentials", http.StatusNotImplemented)
		return
	}
	if err := rot.RotateCredentials(r.Context(), a.ID); err != nil {
		s.logger.Printf("agents: rotate credentials of %d: %v", a.ID, err)
		http.Error(w, "could not rotate the agent's credentials", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("agents: credentials of %s (%d) rotated by user %d", a.Name, a.ID, authz.MustFrom(actor.Context()).UserID)
	s.auditTeamLifecycle(actor, team, "agent.credentials.rotate", a)
	w.Header().Set("HX-Trigger", "agents:changed")
	w.WriteHeader(http.StatusOK)
}

// --- /teams/{id}/chief: the team's chief --------------------------------------
//
// Every member reads the page (status, where it runs); only a MANAGER turns it
// on (provision: create chief-<team>, owned by the team, or start it), off
// (stop) or restarts it — through teamAdminContext, so it is created and
// started as the team, on the team's credentials (the server pool when the
// team is listed in MUSTER_TEAMS_ON_SERVER_POOL). A non-member gets the 404 a
// missing team gets. The writes are the session tier and same-origin, with no
// step-up: plan §6 lists none for provisioning.
//
// 🔴 REFUSED, EACH WITH ITS REASON, BEFORE ANYTHING IS CREATED:
//   - the Docker Compose install (decision 9: a team chief is Kubernetes only);
//   - a deployment without the claude-code kind (a team's agents are Claude
//     Code agents: gateway is not a team agent kind in phase 4);
//   - a team not on the server pool (no Claude account to run on).

func (s *Server) registerTeamChiefPages(mux Mux) {
	mux.HandleFunc("GET /teams/{id}/chief", s.requireSession(s.handleTeamChiefPage))
	// requireOperatorWrite spelled out (session, then same-origin) to put the
	// provisioner's refusal between them, as POST /agents/{name}/send does: "this
	// build cannot run an agent" is the more useful answer.
	mux.HandleFunc("POST /teams/{id}/chief/provision", s.requireSession(s.requireLifecycleProvisioner(s.requireSameOrigin(s.handleTeamChiefProvision))))
	mux.HandleFunc("POST /teams/{id}/chief/stop", s.requireSession(s.requireLifecycleProvisioner(s.requireSameOrigin(s.handleTeamChiefStop))))
	mux.HandleFunc("POST /teams/{id}/chief/restart", s.requireSession(s.requireLifecycleProvisioner(s.requireSameOrigin(s.handleTeamChiefRestart))))
}

// The team chief's refusals, one spelling each.
const (
	TeamChiefComposeRefusal = "A team chief runs on Kubernetes only; this is the Docker Compose install, whose chief is the admin's."
	TeamChiefKindRefusal    = "A team chief is a Claude Code agent, and this deployment does not enable the claude-code kind (MUSTER_AGENT_KINDS)."
	TeamChiefPoolRefusal    = "This team has no Claude account to run on: an admin lists the team in MUSTER_TEAMS_ON_SERVER_POOL to run its agents on the server's Claude pool."
	TeamChiefManagerOnly    = "Only a manager of the team turns its chief on or off."
	TeamChiefNone           = "This team has no chief yet."
)

// teamChiefRefusal is why team's chief cannot be provisioned on this
// deployment, or "".
func (s *Server) teamChiefRefusal(ctx context.Context, team int64) string {
	switch {
	case s.ext.ComposeChief != nil:
		return TeamChiefComposeRefusal
	case !s.ext.kindEnabled(agents.KindClaudeCode):
		return TeamChiefKindRefusal
	case s.ext.Kinds == nil || s.ext.Kinds.Unavailable(ctx, team)[agents.KindClaudeCode] != "":
		return TeamChiefPoolRefusal
	}
	return ""
}

// teamChiefRole is the request principal's role in team {id} (its person
// door's memberships), or the 404 a non-member gets.
func (s *Server) teamChiefRole(w http.ResponseWriter, r *http.Request) (int64, authz.TeamRole, bool) {
	if team, ok := pathID(r); ok {
		if role, in := authz.MustFrom(r.Context()).TeamScope().RoleIn(team); in {
			return team, role, true
		}
	}
	if isDocumentNavigation(r) {
		s.renderNotice(w, r, http.StatusNotFound, "No such team", "There is no team here that you belong to.")
	} else {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such team"})
	}
	return 0, "", false
}

// teamChief is team's chief, read as the caller (agents is shared at viewer:
// any member sees it), or found false.
func (s *Server) teamChief(ctx context.Context, team int64) (agents.Agent, bool, error) {
	if s.ext.Agents == nil {
		return agents.Agent{}, false, nil
	}
	a, err := s.ext.Agents.GetByName(ctx, agents.ChiefNameFor(team))
	if errors.Is(err, pgx.ErrNoRows) {
		return agents.Agent{}, false, nil
	}
	if err != nil || a.OwnerID != team {
		return agents.Agent{}, false, err
	}
	return a, true, nil
}

func (s *Server) handleTeamChiefPage(w http.ResponseWriter, r *http.Request) {
	team, role, ok := s.teamChiefRole(w, r)
	if !ok {
		return
	}
	s.renderTeamChief(w, r, team, role, http.StatusOK, "")
}

func (s *Server) renderTeamChief(w http.ResponseWriter, r *http.Request, team int64, role authz.TeamRole, code int, problem string) {
	ctx := r.Context()
	a, has, err := s.teamChief(ctx, team)
	if err != nil {
		s.logger.Printf("teams: read team %d's chief: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the team's chief", "Try again in a moment.")
		return
	}
	id := strconv.FormatInt(team, 10)
	name := s.teamNames(ctx, []int64{team})[team]
	refusal := s.teamChiefRefusal(ctx, team)
	var b strings.Builder
	b.WriteString(problemNode(problem))
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Team chief · ` + html.EscapeString(name) + `</h1>
<p class="text-sm text-fg2">One chief for the whole team: operators and managers type into its one shared conversation, viewers read it. ` +
		`Its managers turn it on and off. Team members have to trust each other: anything the chief prints is readable by every member.</p>`)
	status := TeamChiefNone
	if has {
		status = "Status: " + a.Status
	}
	b.WriteString(`<p class="text-sm text-fg" id="team-chief-status" data-has-chief="` + strconv.FormatBool(has) + `">` + html.EscapeString(status) + `</p>`)
	switch {
	case refusal != "":
		b.WriteString(`<p class="text-sm text-fg2" id="team-chief-credentials">` + html.EscapeString(refusal) + `</p>`)
	case has && a.CCAccount != "":
		b.WriteString(`<p class="text-sm text-fg2" id="team-chief-credentials">Runs on the server pool, account ` + html.EscapeString(a.CCAccount) + `.</p>`)
	default:
		b.WriteString(`<p class="text-sm text-fg2" id="team-chief-credentials">Runs on the server pool.</p>`)
	}
	if has {
		b.WriteString(`<p class="text-sm"><a class="text-accent underline" href="/agents/` + html.EscapeString(a.Name) + `">Open the team chief's chat</a></p>`)
	}
	b.WriteString(`</section>`)
	if role == authz.TeamManager {
		b.WriteString(`<section class="` + adminCard + `" id="team-chief-controls"><h2 class="text-base font-semibold text-fg">Manage</h2><div class="flex flex-wrap gap-2">`)
		if refusal == "" {
			label := "Turn on"
			if has {
				label = "Start"
			}
			b.WriteString(`<form method="POST" action="/teams/` + id + `/chief/provision"><button type="submit" class="` + adminButton + `">` + label + `</button></form>`)
		}
		if has {
			b.WriteString(`<form method="POST" action="/teams/` + id + `/chief/restart"><button type="submit" class="` + adminQuiet + `">Restart</button></form>`)
			b.WriteString(`<form method="POST" action="/teams/` + id + `/chief/stop"><button type="submit" class="` + adminQuiet + `">Turn off</button></form>`)
		}
		b.WriteString(`</div></section>`)
	}
	s.writeNavDocument(w, r, code, "Team chief", ui.NavAccount, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// teamChiefWrite is a manager write's gate: team {id}, the caller manages it
// (the request's memberships, then a FRESH read in teamAdminContext), and the
// team-acting context it runs under.
func (s *Server) teamChiefWrite(w http.ResponseWriter, r *http.Request) (int64, context.Context, bool) {
	team, role, ok := s.teamChiefRole(w, r)
	if !ok {
		return 0, nil, false
	}
	if role != authz.TeamManager {
		s.renderTeamChief(w, r, team, role, http.StatusForbidden, TeamChiefManagerOnly)
		return 0, nil, false
	}
	tctx, err := s.teamAdminContext(r.Context(), team)
	if errors.Is(err, ErrNotTeamManager) {
		s.renderTeamChief(w, r, team, role, http.StatusForbidden, TeamChiefManagerOnly)
		return 0, nil, false
	}
	if err != nil {
		s.logger.Printf("teams: chief of team %d: %v", team, err)
		s.renderTeamChief(w, r, team, role, http.StatusInternalServerError, "Your team role could not be read just now. Try again.")
		return 0, nil, false
	}
	return team, tctx, true
}

func (s *Server) teamChiefDone(w http.ResponseWriter, r *http.Request, team int64) {
	http.Redirect(w, r, "/teams/"+strconv.FormatInt(team, 10)+"/chief", http.StatusSeeOther)
}

// handleTeamChiefProvision is POST /teams/{id}/chief/provision: create the
// team's chief (chief-<team>, owned by the team, a Claude Code agent) or start
// it, as the team.
func (s *Server) handleTeamChiefProvision(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamChiefWrite(w, r)
	if !ok {
		return
	}
	if why := s.teamChiefRefusal(tctx, team); why != "" {
		s.renderTeamChief(w, r, team, authz.TeamManager, http.StatusConflict, why)
		return
	}
	rec, created, err := s.provisionChief(tctx, team, "", agents.KindClaudeCode)
	if err != nil {
		code, msg := http.StatusInternalServerError, "The team chief could not be provisioned. Try again."
		if errors.Is(err, ErrDispatchRefused) {
			code, msg = http.StatusConflict, err.Error()
		}
		s.renderTeamChief(w, r, team, authz.TeamManager, code, msg)
		return
	}
	action := "chief.start"
	if created {
		action = "chief.provision"
	}
	s.logger.Printf("teams: manager %d: %s team %d's chief %s (%d)", authz.MustFrom(r.Context()).UserID, action, team, rec.Name, rec.ID)
	s.auditTeam(r.Context(), actorOf(r), team, action, "agent:"+rec.Name, map[string]any{"agent_id": rec.ID})
	s.teamChiefDone(w, r, team)
}

// teamChiefLifecycle runs action on team's chief as the team, in the
// background (as the agent page's Start and Stop do), and audits it.
func (s *Server) teamChiefLifecycle(w http.ResponseWriter, r *http.Request, label string, action func(context.Context, int64) error) {
	team, tctx, ok := s.teamChiefWrite(w, r)
	if !ok {
		return
	}
	a, has, err := s.teamChief(tctx, team)
	if err != nil {
		s.logger.Printf("teams: %s team %d's chief: %v", label, team, err)
		s.renderTeamChief(w, r, team, authz.TeamManager, http.StatusInternalServerError, "Could not load the team's chief. Try again.")
		return
	}
	if !has {
		s.renderTeamChief(w, r, team, authz.TeamManager, http.StatusConflict, TeamChiefNone)
		return
	}
	opCtx := context.WithoutCancel(tctx)
	safeGo(s.logger, "team chief "+label, func() {
		if err := action(opCtx, a.ID); err != nil {
			s.logger.Printf("teams: %s team %d's chief: %v", label, team, err)
		}
	})
	s.auditTeam(r.Context(), actorOf(r), team, "chief."+label, "agent:"+a.Name, map[string]any{"agent_id": a.ID})
	s.teamChiefDone(w, r, team)
}

func (s *Server) handleTeamChiefStop(w http.ResponseWriter, r *http.Request) {
	s.teamChiefLifecycle(w, r, "stop", func(ctx context.Context, id int64) error {
		s.clearAgentWork(id)
		return s.ext.Provisioner.Stop(ctx, id)
	})
}

func (s *Server) handleTeamChiefRestart(w http.ResponseWriter, r *http.Request) {
	s.teamChiefLifecycle(w, r, "restart", func(ctx context.Context, id int64) error {
		s.clearAgentWork(id)
		if err := s.ext.Provisioner.Stop(ctx, id); err != nil {
			return err
		}
		return s.ext.Provisioner.Start(ctx, id)
	})
}
