package api

import (
	"errors"
	"html"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// --- /teams/{id}: a team's MANAGER manages its members ------------------------
// (docs/design/team-sharing.md, phase 2a)
//
// A manager sees the team's members with their roles, adds a person by their
// email, re-roles and removes members, and reads the team's audit rows. Every
// change is an audit_log row naming the acting manager and owned by the team
// (internal/users teams.go), and a change that would leave the team with no
// manager is refused (users.ErrLastManager; an admin can still make it on
// /admin/teams).
//
// 🔴 THE GATE IS THE REQUEST PRINCIPAL'S OWN MEMBERSHIP: Principal.Teams, read
// with the role on every request (users.Account), must say manager of {id}.
// Anyone else — a viewer, an operator, a non-member, an admin who is not a
// manager — gets the same 404 a team that does not exist gets, so the page
// is no oracle for which teams exist. The writes are requireOperatorWrite:
// the session tier, same-origin (the CSRF rule every form here follows). No
// step-up: membership is account management, like /admin/users, not typing
// into a session (docs/sending-and-step-up.md).
//
// The member list's NAMES are read as the manager through
// muster_team_people (it answers only a member); roles and emails come from
// a System read after the manager check, as the admin pages read users.

func (s *Server) registerTeamPages(mux Mux) {
	mux.HandleFunc("GET /teams/{id}", s.requireSession(s.handleTeamPage))
	mux.HandleFunc("POST /teams/{id}/members", s.requireOperatorWrite(s.handleTeamMemberSet))
	mux.HandleFunc("POST /teams/{id}/members/{user}/remove", s.requireOperatorWrite(s.handleTeamMemberRemove))
}

// managedTeam is {id} when the request's principal manages it; otherwise it
// answers 404 and reports false.
func (s *Server) managedTeam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	team, ok := pathID(r)
	if ok && s.gt != nil && s.gt.Accounts != nil {
		if role, in := authz.MustFrom(r.Context()).Teams.RoleIn(team); in && role == authz.TeamManager {
			return team, true
		}
	}
	if isDocumentNavigation(r) {
		s.renderNotice(w, r, http.StatusNotFound, "No such team", "There is no team here that you manage.")
	} else {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such team"})
	}
	return 0, false
}

func (s *Server) handleTeamPage(w http.ResponseWriter, r *http.Request) {
	team, ok := s.managedTeam(w, r)
	if !ok {
		return
	}
	s.renderTeamPage(w, r, team, http.StatusOK, "")
}

func (s *Server) renderTeamPage(w http.ResponseWriter, r *http.Request, team int64, code int, problem string) {
	ctx := r.Context()
	people, err := s.gt.Accounts.TeamPeople(ctx, team)
	if err != nil {
		s.logger.Printf("teams: read team %d's people: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the team", "Try again in a moment.")
		return
	}
	members, err := s.gt.Accounts.TeamMembers(ctx, team)
	if err != nil {
		s.logger.Printf("teams: read team %d's members: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the team", "Try again in a moment.")
		return
	}
	names := map[int64]string{}
	for _, p := range people {
		names[p.UserID] = p.DisplayName
	}
	me := authz.MustFrom(ctx).UserID
	id := strconv.FormatInt(team, 10)
	var b strings.Builder
	b.WriteString(problemNode(problem))
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Team members</h1>
<p class="text-sm text-fg2">Viewers watch, operators type and answer, managers manage the team. A team always keeps at least one manager.</p>
<ul class="flex flex-col gap-2" id="team-members">`)
	for _, m := range members {
		uid := strconv.FormatInt(m.UserID, 10)
		name := names[m.UserID]
		if name == "" {
			name = personLabel(m.Email, m.DisplayName, m.UserID)
		}
		if m.UserID == me {
			name += " (you)"
		}
		b.WriteString(`<li class="flex flex-wrap items-center gap-2" data-member-id="` + uid + `" data-member-role="` + string(m.Role) + `">
<span class="text-sm text-fg">` + html.EscapeString(name) + `</span>
<form method="POST" action="/teams/` + id + `/members" class="flex items-center gap-2"><input type="hidden" name="user" value="` + uid + `">
<label class="sr-only" for="role-` + uid + `">Role</label><select id="role-` + uid + `" name="role" class="` + adminInput + `">`)
		for _, role := range teamRoles {
			sel := ""
			if role == m.Role {
				sel = " selected"
			}
			b.WriteString(`<option value="` + string(role) + `"` + sel + `>` + string(role) + `</option>`)
		}
		b.WriteString(`</select><button type="submit" class="` + adminQuiet + `">Change</button></form>
<form method="POST" action="/teams/` + id + `/members/` + uid + `/remove"><button type="submit" class="` + adminQuiet + `">Remove</button></form></li>`)
	}
	b.WriteString(`</ul></section>
<form method="POST" action="/teams/` + id + `/members" class="` + adminCard + `">
<h2 class="text-base font-semibold text-fg">Add a member</h2>
<label for="member-email" class="text-sm font-medium text-fg">Their email</label>
<input id="member-email" name="email" type="email" required autocomplete="off" class="` + adminInput + `">
<label for="member-role" class="text-sm font-medium text-fg">Role</label>
<select id="member-role" name="role" class="` + adminInput + `">`)
	for _, role := range teamRoles {
		b.WriteString(`<option value="` + string(role) + `">` + string(role) + `</option>`)
	}
	b.WriteString(`</select><button type="submit" class="` + adminButton + `">Add</button></form>`)
	if entries, err := s.gt.Accounts.TeamAudit(ctx, team, 25); err == nil && len(entries) > 0 {
		b.WriteString(`<section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">Recent team activity</h2><ul class="flex flex-col gap-1 text-sm text-fg2" id="team-audit">`)
		for _, e := range entries {
			b.WriteString(`<li><span class="text-fg">` + html.EscapeString(e.Action) + `</span> · by user ` + strconv.FormatInt(e.UserID, 10) +
				` · ` + fmtTime(e.At) + `</li>`)
		}
		b.WriteString(`</ul></section>`)
	}
	s.writeNavDocument(w, r, code, "Team", ui.NavAccount, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// teamPageDone answers a manager's write: back to the page on success, the
// page with its problem on a refusal, a 500 otherwise.
func (s *Server) teamPageDone(w http.ResponseWriter, r *http.Request, team int64, what string, err error) {
	if err == nil {
		http.Redirect(w, r, "/teams/"+strconv.FormatInt(team, 10), http.StatusSeeOther)
		return
	}
	if errors.Is(err, users.ErrLastManager) {
		s.renderTeamPage(w, r, team, http.StatusConflict, "A team keeps at least one manager: make someone else a manager first.")
		return
	}
	if code, msg, ok := teamProblem(err); ok {
		s.renderTeamPage(w, r, team, code, msg)
		return
	}
	s.logger.Printf("teams: %s: %v", what, err)
	s.renderTeamPage(w, r, team, http.StatusInternalServerError, "That change could not be made. Try again.")
}

func (s *Server) handleTeamMemberSet(w http.ResponseWriter, r *http.Request) {
	team, ok := s.managedTeam(w, r)
	if !ok {
		return
	}
	user, ok := formID(r, "user")
	if !ok {
		email := strings.TrimSpace(r.FormValue("email"))
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
			s.renderTeamPage(w, r, team, http.StatusBadRequest, "Enter the email the person signs in with.")
			return
		}
		id, found, err := s.gt.Users.IDByEmail(r.Context(), email)
		if err != nil {
			s.teamPageDone(w, r, team, "find a person by email", err)
			return
		}
		if !found {
			s.renderTeamPage(w, r, team, http.StatusBadRequest, "No one signs in with that email.")
			return
		}
		user = id
	}
	by := actorOf(r)
	err := s.gt.Accounts.SetTeamMemberAsManager(r.Context(), by, team, user, authz.TeamRole(r.FormValue("role")))
	if err == nil {
		s.logger.Printf("teams: user %d is %s of team %d (by manager %d)", user, r.FormValue("role"), team, by.UserID)
	}
	s.teamPageDone(w, r, team, "set team member", err)
}

func (s *Server) handleTeamMemberRemove(w http.ResponseWriter, r *http.Request) {
	team, ok := s.managedTeam(w, r)
	if !ok {
		return
	}
	user, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil || user <= 0 {
		http.NotFound(w, r)
		return
	}
	by := actorOf(r)
	err = s.gt.Accounts.RemoveTeamMemberAsManager(r.Context(), by, team, user)
	if err == nil {
		s.logger.Printf("teams: user %d removed from team %d (by manager %d)", user, team, by.UserID)
	}
	s.teamPageDone(w, r, team, "remove team member", err)
}
