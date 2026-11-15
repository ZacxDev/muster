package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/branding"
	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/users"
)

// --- /admin/teams (docs/design/team-sharing.md, phase 1) ---------------------
//
// An admin creates a team, naming its first manager, and adds, re-roles and
// removes its members. Every route is requireAdmin, like the rest of /admin,
// and every change is an audit_log row naming the acting admin, written in the
// same transaction as the change (internal/users teams.go).
//
// What a team shares is said on the page (AdminTeamsShares), with each team's
// credentials line (adminTeamCredentials): whether its agents run on the
// server's Claude pool and whether they may use the GitHub App. No one signs
// in as a team (admit refuses it at every door); its members reach its rows
// through their own sign-in, by role.

// AdminTeamsShares is what /admin/teams says a team shares. It replaced
// phase 1's "Nothing is shared yet", which stayed on the page after phases 2
// to 4 shipped and told an admin the opposite of what a member sees.
const AdminTeamsShares = "A team owns shared work, and its members see it by role: viewers read, operators also file, " +
	"comment and chat, managers also run the team's chief and agents. A team has its own tasks, a team chief " +
	"(set up by a manager on the team's Chief page), the agents its chief starts, its officers, and its hosts' " +
	"sessions. Its agents run on the server's Claude pool only when the team is listed in MUSTER_TEAMS_ON_SERVER_POOL, " +
	"and work in your repositories only when it is listed in MUSTER_TEAMS_ON_GITHUB_APP; each team's line below says which."

// adminTeamCredentials is team's line on /admin/teams: whether its agents
// can run on the server's Claude pool (teamChiefRefusal: the same answer the
// team's Chief page gives) and whether they may use the GitHub App. A team
// LISTED on the server pool on a server with no Claude pool says so, rather
// than "not on the server pool" (credentials.TeamPoolNotConfigured).
func (s *Server) adminTeamCredentials(ctx context.Context, team int64) string {
	pool := "Claude: on the server pool."
	switch why := s.teamChiefRefusal(ctx, team); {
	case why == TeamChiefPoolRefusal && s.ext.Kinds != nil &&
		s.ext.Kinds.Unavailable(ctx, team)[agents.KindClaudeCode] == credentials.TeamPoolNotConfigured:
		pool = "Claude: listed on the server pool, but this server has no Claude pool configured, so its agents cannot run."
	case why == TeamChiefPoolRefusal:
		pool = "Claude: not on the server pool, so its agents cannot run."
	case why != "":
		pool = "Claude: " + why
	}
	app := " GitHub App: not listed, so its agents work in no repository."
	switch {
	case s.ext.GitHubApp == nil:
		app = " GitHub App: none is configured."
	case s.ext.GitHubAppTeams[team]:
		app = " GitHub App: listed; a manager approves each repository."
	}
	return pool + app
}

func (s *Server) registerAdminTeamRoutes(mux Mux) {
	mux.HandleFunc("GET /admin/teams", s.requireAdmin(s.handleAdminTeams))
	mux.HandleFunc("POST /admin/teams", s.requireAdmin(s.handleAdminTeamCreate))
	mux.HandleFunc("POST /admin/teams/{id}/members", s.requireAdmin(s.handleAdminTeamMemberSet))
	mux.HandleFunc("POST /admin/teams/{id}/members/{user}/remove", s.requireAdmin(s.handleAdminTeamMemberRemove))
}

// teamRoles is the role picker's order.
var teamRoles = []authz.TeamRole{authz.TeamViewer, authz.TeamOperator, authz.TeamManager}

func (s *Server) handleAdminTeams(w http.ResponseWriter, r *http.Request) {
	s.renderAdminTeams(w, r, http.StatusOK, "")
}

func (s *Server) renderAdminTeams(w http.ResponseWriter, r *http.Request, code int, problem string) {
	ctx := r.Context()
	teams, err := s.gt.Accounts.ListTeams(ctx)
	if err != nil {
		s.logger.Printf("admin: list teams: %v", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load teams", "Try again in a moment.")
		return
	}
	people, err := s.gt.Accounts.ListUsers(ctx)
	if err != nil {
		s.logger.Printf("admin: list users for teams: %v", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load teams", "Try again in a moment.")
		return
	}
	personOptions := func(skip map[int64]bool) string {
		var b strings.Builder
		for _, u := range people {
			if skip[u.ID] || u.Disabled() {
				continue
			}
			b.WriteString(`<option value="` + strconv.FormatInt(u.ID, 10) + `">` + html.EscapeString(personLabel(u.Email, u.DisplayName, u.ID)) + `</option>`)
		}
		return b.String()
	}
	roleOptions := func(selected authz.TeamRole) string {
		var b strings.Builder
		for _, role := range teamRoles {
			sel := ""
			if role == selected {
				sel = " selected"
			}
			b.WriteString(`<option value="` + string(role) + `"` + sel + `>` + string(role) + `</option>`)
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString(problemNode(problem))
	b.WriteString(`<form method="POST" action="/admin/teams" class="` + adminCard + `">
<h1 class="text-base font-semibold text-fg">New team</h1>
<p class="text-sm text-fg2" id="admin-teams-shares">` + AdminTeamsShares + `</p>
<label for="team-name" class="text-sm font-medium text-fg">Name</label>
<input id="team-name" name="name" required maxlength="64" autocomplete="off" class="` + adminInput + `">
<label for="team-manager" class="text-sm font-medium text-fg">First manager</label>
<select id="team-manager" name="manager" required class="` + adminInput + `">` + personOptions(nil) + `</select>
<button type="submit" class="` + adminButton + `">Create team</button></form>`)
	b.WriteString(`<section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">Teams</h2><ul class="flex flex-col gap-4" id="admin-teams">`)
	if len(teams) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No teams yet.</li>`)
	}
	for _, t := range teams {
		id := strconv.FormatInt(t.ID, 10)
		b.WriteString(`<li class="flex flex-col gap-2 rounded-lg border border-line px-3 py-3" data-team-id="` + id + `">
<div class="flex flex-wrap items-baseline gap-2"><span class="font-semibold text-fg" data-team-name>` + html.EscapeString(t.Name) + `</span>
<span class="text-xs text-fg2">#` + id + ` · created ` + fmtTime(t.CreatedAt) + `</span></div>`)
		b.WriteString(`<p class="text-xs text-fg2" data-team-credentials>` + html.EscapeString(s.adminTeamCredentials(ctx, t.ID)) + `</p><ul class="flex flex-col gap-2">`)
		in := map[int64]bool{}
		if len(t.Members) == 0 {
			b.WriteString(`<li class="text-sm text-fg2">No members.</li>`)
		}
		for _, m := range t.Members {
			in[m.UserID] = true
			uid := strconv.FormatInt(m.UserID, 10)
			b.WriteString(`<li class="flex flex-wrap items-center gap-2" data-member-id="` + uid + `" data-member-role="` + string(m.Role) + `">
<span class="text-sm text-fg">` + html.EscapeString(personLabel(m.Email, m.DisplayName, m.UserID)) + `</span>
<form method="POST" action="/admin/teams/` + id + `/members" class="flex items-center gap-2"><input type="hidden" name="user" value="` + uid + `">
<label class="sr-only" for="role-` + id + `-` + uid + `">Role</label>
<select id="role-` + id + `-` + uid + `" name="role" class="` + adminInput + `">` + roleOptions(m.Role) + `</select>
<button type="submit" class="` + adminQuiet + `">Change</button></form>
<form method="POST" action="/admin/teams/` + id + `/members/` + uid + `/remove"><button type="submit" class="` + adminQuiet + `">Remove</button></form></li>`)
		}
		b.WriteString(`</ul>`)
		if opts := personOptions(in); opts != "" {
			b.WriteString(`<form method="POST" action="/admin/teams/` + id + `/members" class="flex flex-wrap items-center gap-2">
<label class="sr-only" for="add-` + id + `">Person</label><select id="add-` + id + `" name="user" class="` + adminInput + `">` + opts + `</select>
<label class="sr-only" for="add-role-` + id + `">Role</label><select id="add-role-` + id + `" name="role" class="` + adminInput + `">` + roleOptions(authz.TeamViewer) + `</select>
<button type="submit" class="` + adminQuiet + `">Add member</button></form>`)
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></section>`)
	s.writeAdminDocument(w, r, code, "Teams", "/admin/teams", b.String())
}

// personLabel is how a person is named on the page: email, else display name,
// else their number.
func personLabel(email, display string, id int64) string {
	switch {
	case email != "":
		return email
	case display != "":
		return display
	}
	return "user #" + strconv.FormatInt(id, 10)
}

// nameRefused reports whether err is branding.CheckNameText's refusal: a
// character it names (ErrNameControl) or bytes that are not UTF-8
// (ErrNameInvalid).
func nameRefused(err error) bool {
	return errors.Is(err, branding.ErrNameControl) || errors.Is(err, branding.ErrNameInvalid)
}

// nameCharSentence is the page's sentence for a name refused by
// branding.CheckNameText, naming the character (as U+XXXX: it may draw as
// nothing at all). what is the sentence's subject, "Your name" or "A team name".
func nameCharSentence(what string, err error) string {
	var e *branding.NameCharError
	switch {
	case errors.As(err, &e):
		return fmt.Sprintf("%s cannot contain U+%04X: control, text-direction and invisible formatting characters are not allowed.", what, e.Rune)
	case errors.Is(err, branding.ErrNameInvalid):
		return what + " is not valid text: it holds bytes that are not UTF-8."
	}
	return what + " cannot contain control, text-direction or invisible formatting characters."
}

// teamProblem maps a store refusal to its status and the page's sentence, and
// reports whether err is one.
func teamProblem(err error) (int, string, bool) {
	switch {
	case errors.Is(err, users.ErrTeamNameTaken):
		return http.StatusConflict, "A team already has that name.", true
	case errors.Is(err, users.ErrTeamName):
		return http.StatusBadRequest, "A team name is 1 to 64 characters.", true
	case nameRefused(err):
		return http.StatusBadRequest, nameCharSentence("A team name", err), true
	case errors.Is(err, users.ErrNotATeam):
		return http.StatusNotFound, "There is no such team.", true
	case errors.Is(err, users.ErrNotAPerson):
		return http.StatusBadRequest, "Choose a person: a team cannot be a member of a team.", true
	case errors.Is(err, users.ErrNotAMember):
		return http.StatusNotFound, "That person is not a member of that team.", true
	case errors.Is(err, users.ErrTeamRole):
		return http.StatusBadRequest, "Choose viewer, operator or manager.", true
	}
	return 0, "", false
}

// teamDone answers a team write: back to the page on success, the page with
// its problem on a refusal, a 500 otherwise.
func (s *Server) teamDone(w http.ResponseWriter, r *http.Request, what string, err error) {
	if err == nil {
		http.Redirect(w, r, "/admin/teams", http.StatusSeeOther)
		return
	}
	if code, msg, ok := teamProblem(err); ok {
		s.renderAdminTeams(w, r, code, msg)
		return
	}
	s.logger.Printf("admin: %s: %v", what, err)
	s.renderAdminTeams(w, r, http.StatusInternalServerError, "That change could not be made. Try again.")
}

func formID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) handleAdminTeamCreate(w http.ResponseWriter, r *http.Request) {
	manager, ok := formID(r, "manager")
	if !ok {
		s.renderAdminTeams(w, r, http.StatusBadRequest, "Choose the team's first manager.")
		return
	}
	by := actorOf(r)
	t, err := s.gt.Accounts.CreateTeam(r.Context(), by, r.FormValue("name"), manager)
	if err == nil {
		s.logger.Printf("admin: team %d created by user %d, managed by user %d", t.ID, by.UserID, manager)
	}
	s.teamDone(w, r, "create team", err)
}

func (s *Server) handleAdminTeamMemberSet(w http.ResponseWriter, r *http.Request) {
	team, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, ok := formID(r, "user")
	if !ok {
		s.renderAdminTeams(w, r, http.StatusBadRequest, "Choose a person.")
		return
	}
	role := authz.TeamRole(r.FormValue("role"))
	by := actorOf(r)
	err := s.gt.Accounts.SetTeamMember(r.Context(), by, team, user, role)
	if err == nil {
		s.logger.Printf("admin: user %d is %s of team %d (by user %d)", user, role, team, by.UserID)
		s.teamRoleSet(team, user, role)
	}
	s.teamDone(w, r, "set team member", err)
}

func (s *Server) handleAdminTeamMemberRemove(w http.ResponseWriter, r *http.Request) {
	team, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil || user <= 0 {
		http.NotFound(w, r)
		return
	}
	by := actorOf(r)
	err = s.gt.Accounts.RemoveTeamMember(r.Context(), by, team, user)
	if err == nil {
		s.logger.Printf("admin: user %d removed from team %d (by user %d)", user, team, by.UserID)
		s.teamMembershipEnded(team, user)
	}
	s.teamDone(w, r, "remove team member", err)
}
