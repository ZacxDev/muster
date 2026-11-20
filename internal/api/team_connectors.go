package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// /teams/{id}/connectors: A MANAGER APPROVES THE TEAM'S AGENTS ONTO THE
// GITHUB APP (team sharing phase 4, slice 4.8; operator decision: "managers
// approve each repository once for the team, with step-up").
//
// A team agent's `github` request (filed by its own token, as the team) waits
// here. A MANAGER approves or denies it, AS THE TEAM: teamAdminContext, the
// one site that mints authz.ViaTeamAdmin after a FRESH read of the manager
// role — so the decision is THE operator decision (decideConnectorGrant)
// under the principal that owns the rows.
//
// 🔴 WHY THE TEAM ACTING PRINCIPAL AND NOT THE SYSTEM SCOPE. connector_grants,
// connector_standing_grants and connector_audit are PERSONAL tables
// (owner_isolation): a member's own principal reads none of the team's rows.
// The team acting principal is their OWNER (muster.uid = the team), so it
// reads and writes exactly the team's rows and nobody else's — narrower than
// a System handler, which would run across every user's grants. No new door,
// no new System site: the manager's door is the one the lifecycle already
// uses.
//
//   - 🔴 A FRESH SIGN-IN (stepup.Fresh) for an APPROVAL, read before anything
//     else is: it hands a team agent a token for one of the deployment's
//     repositories. A denial changes no credential and takes none.
//   - 🔴 APPROVED ONCE PER REPOSITORY FOR THE TEAM: the approval is shared
//     (shareApproval) as the TEAM's standing grant keyed on (github, the App,
//     the agent's repository), so the team's next agent on that repository is
//     approved when it asks, and an agent on another repository asks again.
//   - Only `github`, and only for a team listed in MUSTER_TEAMS_ON_GITHUB_APP:
//     for any other team the App is not its connector (githubapp.Admits), and
//     the approval is refused naming why.
//   - Every decision writes the team's audit row naming the manager.
//   - 🔴 A MANAGER REVOKES a repository already approved for the team (round-0
//     ruling: "add a manager revoke in this slice"), through the same door,
//     with a fresh sign-in: the team's standing grant ends, and with it every
//     approval it gave (connectors.Store.RevokeStandingGrant), so the team's
//     next agent on that repository asks again. Row-level security scopes it
//     to the team's own standing grants: another team's, or a person's, is the
//     404 a missing one gets. Unlisting the team only PAUSES its approvals
//     (the App is not its connector while unlisted, and re-listing restores
//     them); a revoke is what removes one.
// ---------------------------------------------------------------------------

// The team connector page's sentences, one spelling each.
const (
	TeamConnectorsManagerOnly = "Only a manager of the team approves its agents' connector requests."
	TeamConnectorsStepUp      = "Approving a team agent onto a repository needs a recent sign-in. Confirm it's you, then approve again."
	TeamConnectorsNotGitHub   = "Only the github connector is approved for a team's agents here; this request was not approved."
	TeamConnectorsNoApp       = "This team is not lent the deployment's GitHub App: an admin lists the team in MUSTER_TEAMS_ON_GITHUB_APP. Nothing was approved."
	TeamConnectorsGone        = "That request no longer exists, or was already decided."
	TeamConnectorsNone        = "No connector request is waiting."
	TeamConnectorsRevokeStale = "Revoking a repository approved for the team needs a recent sign-in. Confirm it's you, then revoke again."
	TeamConnectorsRevokeGone  = "That approval was already revoked."
	TeamConnectorsNoSuchGrant = "There is no such approval for this team."
)

func (s *Server) registerTeamConnectorPages(mux Mux) {
	mux.HandleFunc("GET /teams/{id}/connectors", s.requireSession(s.handleTeamConnectorsPage))
	mux.HandleFunc("POST /teams/{id}/connectors/{grant}/approve", s.requireOperatorWrite(s.handleTeamConnectorApprove))
	mux.HandleFunc("POST /teams/{id}/connectors/{grant}/deny", s.requireOperatorWrite(s.handleTeamConnectorDeny))
	mux.HandleFunc("POST /teams/{id}/connectors/standing/{standing}/revoke", s.requireOperatorWrite(s.handleTeamStandingRevoke))
}

// teamConnectorsManager is the page's gate: team {id}, the caller is a member
// of it (the request's memberships), then a manager of it by a FRESH read
// (teamAdminContext, the one role check), and the team-acting context the page
// reads and decides under. A non-member gets the 404 a missing team gets; it
// answers the request itself on false.
func (s *Server) teamConnectorsManager(w http.ResponseWriter, r *http.Request) (int64, context.Context, bool) {
	team, _, ok := s.teamChiefRole(w, r)
	if !ok {
		return 0, nil, false
	}
	tctx, err := s.teamAdminContext(r.Context(), team)
	switch {
	case errors.Is(err, ErrNotTeamManager):
		s.renderNotice(w, r, http.StatusForbidden, "Team connectors", TeamConnectorsManagerOnly)
		return 0, nil, false
	case err != nil:
		s.logger.Printf("teams: connectors of team %d: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Team connectors", "Your team role could not be read just now. Try again.")
		return 0, nil, false
	}
	return team, tctx, true
}

func (s *Server) handleTeamConnectorsPage(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamConnectorsManager(w, r)
	if !ok {
		return
	}
	s.renderTeamConnectors(w, r, tctx, team, http.StatusOK, "", "")
}

// renderTeamConnectorsOff is a team's Connectors page on a server with no
// connector store: the plain sentence, the variable in an admin's ⓘ (the
// jargon audit's F112). The machine routes keep ConnectorsUnconfigured.
func (s *Server) renderTeamConnectorsOff(w http.ResponseWriter, r *http.Request) {
	admin := authz.IsAdmin(r.Context())
	words := TeamConnectorsOff
	if !admin {
		words += " Your admin can turn them on."
	}
	s.renderNotice(w, r, http.StatusServiceUnavailable, "Team connectors", words,
		ui.SetupSheet("team-connectors-setup", "How to turn on connected accounts", "Set this in the server's .env and restart muster:",
			"MUSTER_CONNECTOR_ENCRYPTION_KEY", admin))
}

// renderTeamConnectors draws the team's pending connector requests (each with
// Approve and Deny) and the repositories already approved for the team (each
// with Revoke), read as the team (tctx).
func (s *Server) renderTeamConnectors(w http.ResponseWriter, r *http.Request, tctx context.Context, team int64, code int, problem, notice string) {
	if s.ext.Connectors == nil {
		s.renderTeamConnectorsOff(w, r)
		return
	}
	pending, err := s.ext.Connectors.ListPendingGrants(tctx)
	if err != nil {
		s.logger.Printf("teams: list team %d's connector requests: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Team connectors", "Could not load the team's connector requests. Try again.")
		return
	}
	standing, err := s.ext.Connectors.ListStandingGrants(tctx)
	if err != nil {
		s.logger.Printf("teams: list team %d's standing grants: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Team connectors", "Could not load the team's connector requests. Try again.")
		return
	}
	id := strconv.FormatInt(team, 10)
	name := s.teamNames(r.Context(), []int64{team})[team]
	var b strings.Builder
	b.WriteString(problemNode(problem))
	if notice != "" {
		b.WriteString(`<p class="text-sm text-fg" id="team-connectors-notice" role="status">` + html.EscapeString(notice) + `</p>`)
	}
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Connectors · ` + html.EscapeString(name) + `</h1>`)
	lent := s.ext.GitHubApp != nil && s.ext.GitHubAppTeams[team]
	if lent {
		b.WriteString(`<p class="text-sm text-fg2" id="team-connectors-app">The team's agents may use the deployment's GitHub App. ` +
			`Approve each repository once: the team's later agents on the same repository are approved when they ask. ` +
			`Approving needs a recent sign-in.</p>`)
	} else {
		b.WriteString(`<p class="text-sm text-fg2" id="team-connectors-app">` + html.EscapeString(TeamConnectorsNoApp) + `</p>`)
	}
	b.WriteString(`<ul class="flex flex-col gap-2" id="team-connector-requests">`)
	if len(pending) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">` + TeamConnectorsNone + `</li>`)
	}
	for _, g := range pending {
		gid := strconv.FormatInt(g.ID, 10)
		b.WriteString(`<li class="flex flex-col gap-1" data-team-request="` + gid + `"><p class="text-sm text-fg">` +
			html.EscapeString(g.AgentName) + ` asks for ` + html.EscapeString(g.Connector) + `</p>`)
		if g.Reason != "" {
			b.WriteString(`<p class="text-sm text-fg2">` + html.EscapeString(g.Reason) + `</p>`)
		}
		if grants := s.requestCardGrants(tctx, g); grants != "" {
			b.WriteString(`<p class="text-sm text-fg2">Approving grants: ` + html.EscapeString(grants) + `</p>`)
		}
		b.WriteString(`<div class="flex flex-wrap gap-2">`)
		if lent && g.Connector == githubapp.ConnectorName {
			b.WriteString(`<form method="POST" action="/teams/` + id + `/connectors/` + gid + `/approve" data-step-up-form><button type="submit" class="` +
				adminButton + `" data-approve="` + gid + `">Approve</button></form>`)
		}
		b.WriteString(`<form method="POST" action="/teams/` + id + `/connectors/` + gid + `/deny"><button type="submit" class="` +
			adminQuiet + `" data-deny="` + gid + `">Deny</button></form></div></li>`)
	}
	b.WriteString(`</ul></section>`)
	b.WriteString(`<section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">Approved for the team</h2><ul id="team-standing-grants">`)
	shown := 0
	for _, sg := range standing {
		if sg.Source != connectors.SourceApp {
			continue
		}
		shown++
		sid := strconv.FormatInt(sg.ID, 10)
		b.WriteString(`<li class="flex flex-wrap items-center gap-2 text-sm text-fg" data-standing="` + sid + `"><span>` + html.EscapeString(sg.Connector) +
			` · ` + html.EscapeString(sg.Repo) + `</span><form method="POST" action="/teams/` + id + `/connectors/standing/` + sid +
			`/revoke" data-step-up-form data-step-up-why="` + html.EscapeString(TeamConnectorsRevokeWhy()) + `"><button type="submit" class="` + adminQuiet + `" data-revoke="` + sid + `">Revoke</button></form></li>`)
	}
	if shown == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No repository yet.</li>`)
	}
	b.WriteString(`</ul></section>`)
	// Approve and the standing revoke are step-up gated (decideTeamConnector,
	// handleTeamStandingRevoke): their forms confirm in place (stepUpFormsHTML).
	b.WriteString(s.stepUpFormsHTML(r, TeamConnectorsApproveWhy()))
	s.writeNavDocument(w, r, code, "Team connectors", ui.NavAccount, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// TeamConnectorsApproveWhy and TeamConnectorsRevokeWhy are the confirm
// dialog's sentences for the page's two gated forms.
func TeamConnectorsApproveWhy() string {
	return "Approving a team agent onto a repository needs a sign-in from the last " + stepup.WindowText() + "."
}

func TeamConnectorsRevokeWhy() string {
	return "Revoking a repository approved for the team needs a sign-in from the last " + stepup.WindowText() + "."
}

func (s *Server) handleTeamConnectorApprove(w http.ResponseWriter, r *http.Request) {
	s.decideTeamConnector(w, r, true)
}

func (s *Server) handleTeamConnectorDeny(w http.ResponseWriter, r *http.Request) {
	s.decideTeamConnector(w, r, false)
}

// decideTeamConnector is a manager's Approve or Deny of the team's pending
// connector request {grant}: the gate (manager, fresh read), the step-up for
// an approval, the github-only and listed-team refusals, then THE operator
// decision (decideConnectorGrant) as the team, and the team's audit row.
func (s *Server) decideTeamConnector(w http.ResponseWriter, r *http.Request, approve bool) {
	team, tctx, ok := s.teamConnectorsManager(w, r)
	if !ok {
		return
	}
	if s.ext.Connectors == nil {
		s.renderTeamConnectorsOff(w, r)
		return
	}
	gid, err := strconv.ParseInt(r.PathValue("grant"), 10, 64)
	if err != nil || gid <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// 🔴 THE STEP-UP FIRST, before the request is even read: a stale sign-in
	// learns nothing and changes nothing.
	if approve && !stepup.Fresh(r.Context(), s.stepUpNow()) {
		w.Header().Set("X-Muster-Step-Up", "required")
		s.renderTeamConnectors(w, r, tctx, team, http.StatusForbidden, TeamConnectorsStepUp, "")
		return
	}
	g, err := s.ext.Connectors.GetGrant(tctx, gid)
	if err != nil || g.Status != connectors.StatusPending {
		s.renderTeamConnectors(w, r, tctx, team, http.StatusConflict, TeamConnectorsGone, "")
		return
	}
	if approve {
		switch {
		case g.Connector != githubapp.ConnectorName:
			s.renderTeamConnectors(w, r, tctx, team, http.StatusConflict, TeamConnectorsNotGitHub, "")
			return
		case connectors.SourceFor(tctx, s.ext.Connectors, g.Connector) != connectors.SourceApp:
			s.renderTeamConnectors(w, r, tctx, team, http.StatusConflict, TeamConnectorsNoApp, "")
			return
		}
	}
	by := actorOf(r)
	dg, shared, err := s.decideConnectorGrant(tctx, gid, approve, connectors.DecisionSource{})
	action := "connector.deny"
	if approve {
		action = "connector.approve"
	}
	detail := map[string]any{"grant_id": gid, "agent": g.AgentName, "connector": g.Connector, "outcome": "succeeded"}
	if err != nil {
		detail["outcome"] = "failed"
	} else if approve {
		detail["repo"] = dg.Repo
	}
	s.auditTeamLevel(r.Context(), by, team, action, "connector:"+g.Connector, detail, approve)
	var poolWorker errPoolWorkerGitHubGrant
	switch {
	case err == nil:
	case errors.Is(err, errGrantUnreadable), errors.Is(err, connectors.ErrNotPending), errors.Is(err, connectors.ErrNotFound):
		s.renderTeamConnectors(w, r, tctx, team, http.StatusConflict, TeamConnectorsGone, "")
		return
	case errors.As(err, &poolWorker):
		s.renderTeamConnectors(w, r, tctx, team, http.StatusConflict, poolWorker.Error()+".", "")
		return
	case errors.As(err, new(errOfficerConnector)), errors.As(err, new(errConnectorMissing)):
		s.renderTeamConnectors(w, r, tctx, team, http.StatusConflict, err.Error()+"; the request was not approved.", "")
		return
	default:
		s.logger.Printf("teams: decide team %d's connector request %d: %v", team, gid, err)
		s.renderTeamConnectors(w, r, tctx, team, http.StatusInternalServerError, "The decision could not be recorded. Try again.", "")
		return
	}
	s.logger.Printf("teams: manager %d %s team %d's request %d (%s for %s)", authz.MustFrom(r.Context()).UserID,
		strings.TrimPrefix(action, "connector."), team, gid, g.Connector, g.AgentName)
	verb := "denied"
	if approve {
		verb = "approved"
	}
	s.renderTeamConnectors(w, r, tctx, team, http.StatusOK, "", dg.AgentName+" "+verb+" for "+dg.Connector+"."+shareNotice(shared))
}

// handleTeamStandingRevoke is a manager's Revoke of a repository already
// approved for the team: standing grant {standing} ends, with every approval
// it gave (RevokeStandingGrant), so the team's next agent on that repository
// asks again. The gate (manager, fresh read), then the step-up, then the
// revoke AS THE TEAM — row-level security makes another owner's standing grant
// the 404 a missing one is — and the team's audit row naming the manager.
func (s *Server) handleTeamStandingRevoke(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamConnectorsManager(w, r)
	if !ok {
		return
	}
	if s.ext.Connectors == nil {
		s.renderTeamConnectorsOff(w, r)
		return
	}
	sid, err := strconv.ParseInt(r.PathValue("standing"), 10, 64)
	if err != nil || sid <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// 🔴 THE STEP-UP FIRST, before the standing grant is even read: a revoke
	// ends a credential the team's running agents hold.
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		w.Header().Set("X-Muster-Step-Up", "required")
		s.renderTeamConnectors(w, r, tctx, team, http.StatusForbidden, TeamConnectorsRevokeStale, "")
		return
	}
	sg, revoked, err := s.ext.Connectors.RevokeStandingGrant(tctx, sid, "operator")
	switch {
	case err == nil:
	case errors.Is(err, connectors.ErrNotFound):
		s.renderTeamConnectors(w, r, tctx, team, http.StatusNotFound, TeamConnectorsNoSuchGrant, "")
		return
	case errors.Is(err, connectors.ErrNotPending):
		s.renderTeamConnectors(w, r, tctx, team, http.StatusConflict, TeamConnectorsRevokeGone, "")
		return
	default:
		s.logger.Printf("teams: revoke team %d's standing grant %d: %v", team, sid, err)
		s.renderTeamConnectors(w, r, tctx, team, http.StatusInternalServerError, "The approval could not be revoked. Try again.", "")
		return
	}
	s.auditTeamLevel(r.Context(), actorOf(r), team, "connector.revoke", "connector:"+sg.Connector, map[string]any{
		"standing_grant_id": sg.ID, "connector": sg.Connector, "repo": sg.Repo, "grants_revoked": len(revoked), "outcome": "succeeded"}, true)
	s.auditGrant(tctx, connectors.Grant{Connector: sg.Connector}, "standing grant "+strconv.FormatInt(sg.ID, 10)+
		" ("+sg.Connector+standingScope(sg)+") revoked by a team manager; "+strconv.Itoa(len(revoked))+" grant(s) it gave revoked with it")
	for _, g := range revoked {
		s.auditGrant(tctx, g, "revoked with standing grant "+strconv.FormatInt(sg.ID, 10))
	}
	s.broadcast(tctx, EventConnectorChanged, sg.Connector)
	s.logger.Printf("teams: manager %d revoked team %d's standing grant %d (%s%s, %d grant(s))", authz.MustFrom(r.Context()).UserID,
		team, sg.ID, sg.Connector, standingScope(sg), len(revoked))
	s.renderTeamConnectors(w, r, tctx, team, http.StatusOK, "", "Revoked "+sg.Connector+standingScope(sg)+
		" for the team, with "+strconv.Itoa(len(revoked))+" agent grant(s) it gave. The team's next agent there will need a manager's approval.")
}
