package api

import (
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/ui"
)

// --- /teams/{id}/officers: a team's officers (docs/design/team-sharing.md,
// phase 3) -------------------------------------------------------------------
//
// A TEAM OFFICER is a builtin officer whose MUSTER_OFFICERS owner is the team:
// its type (name, charter) and its definition (enabled, mode, escalate_to)
// are officer_types / officer_defs rows the team owns. Every member reads
// them here (the managed class reads at viewer); only a MANAGER changes them.
//
// 🔴 MANAGER-ONLY TWICE: the handler refuses anyone below manager, and
// Postgres refuses them again — officer_types and officer_defs are MANAGED
// (0127: write at manager), so an operator's write through the same store
// call is 42501 (TestAnOperatorCannotEditATeamCharter pins both).
//
// 🔴 WHY A PAGE AND NOT THE JSON ADMIN API. /api/officers/* is the hook tier:
// a personal API token or the shared hook token. A personal token carries NO
// team access (team sharing 2a, operator decision), so those routes stay the
// caller's own officers and never name a team. A manager edits a team's
// charter here, signed in, where the person is present and their memberships
// reach the database (Principal.TeamScope).
//
// The writes are requireOperatorWrite (session tier, same-origin), with no
// step-up: a charter is configuration, as the JSON admin API's is.

func (s *Server) registerTeamOfficerPages(mux Mux) {
	mux.HandleFunc("GET /teams/{id}/officers", s.requireSession(s.handleTeamOfficersPage))
	mux.HandleFunc("POST /teams/{id}/officers/types/{key}", s.requireOperatorWrite(s.handleTeamOfficerTypePut))
	mux.HandleFunc("POST /teams/{id}/officers/defs/{officer}", s.requireOperatorWrite(s.handleTeamOfficerDefPut))
}

// teamOfficerRole is the request principal's role in team {id}, read from the
// memberships its door carries (TeamScope: a person's own door). It answers
// the 404 a missing team gets to a non-member, and to everyone while the
// officer store is off.
func (s *Server) teamOfficerRole(w http.ResponseWriter, r *http.Request) (int64, authz.TeamRole, bool) {
	team, ok := pathID(r)
	if ok && s.officerStore != nil {
		if role, in := authz.MustFrom(r.Context()).TeamScope().RoleIn(team); in {
			return team, role, true
		}
	}
	if isDocumentNavigation(r) {
		s.renderNotice(w, r, http.StatusNotFound, "No such team", "There is no team here that you belong to, or this muster keeps no officers.")
	} else {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such team"})
	}
	return 0, "", false
}

// TeamOfficerManagerOnly is what a member below manager is told on a write.
const TeamOfficerManagerOnly = "Only a manager of the team changes its officers."

func (s *Server) handleTeamOfficersPage(w http.ResponseWriter, r *http.Request) {
	team, role, ok := s.teamOfficerRole(w, r)
	if !ok {
		return
	}
	s.renderTeamOfficers(w, r, team, role, http.StatusOK, "")
}

func (s *Server) renderTeamOfficers(w http.ResponseWriter, r *http.Request, team int64, role authz.TeamRole, code int, problem string) {
	ctx := r.Context()
	types, err := s.officerStore.ListTypes(ctx, team)
	if err == nil {
		for i := range types {
			if full, gerr := s.officerStore.GetType(ctx, team, types[i].Key); gerr == nil {
				types[i].Charter = full.Charter
			}
		}
	}
	var defs []builtin.DefInfo
	if err == nil {
		defs, err = s.officerStore.ListDefs(ctx, team)
	}
	if err != nil {
		s.logger.Printf("teams: read team %d's officers: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the team's officers", "Try again in a moment.")
		return
	}
	manager := role == authz.TeamManager
	id := strconv.FormatInt(team, 10)
	var b strings.Builder
	b.WriteString(problemNode(problem))
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Team officers</h1>
<p class="text-sm text-fg2">Officers this team owns: every member sees them, a manager changes them. An officer runs as the team when its owner in MUSTER_OFFICERS is this team.</p>
<ul class="flex flex-col gap-3" id="team-officer-types">`)
	for _, t := range types {
		b.WriteString(`<li class="flex flex-col gap-2" data-type-key="` + html.EscapeString(t.Key) + `" data-charter-version="` + strconv.Itoa(t.CharterVersion) + `">
<span class="text-sm text-fg">` + html.EscapeString(t.Name) + ` (` + html.EscapeString(t.Key) + `) · charter v` + strconv.Itoa(t.CharterVersion) + `</span>`)
		if manager {
			b.WriteString(s.teamTypeForm(id, t.Key, t.Name, t.Charter))
		} else {
			b.WriteString(`<pre class="whitespace-pre-wrap text-xs text-fg2">` + html.EscapeString(t.Charter) + `</pre>`)
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></section><section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">Officers</h2><ul class="flex flex-col gap-3" id="team-officer-defs">`)
	for _, d := range defs {
		esc := make([]string, 0, len(d.EscalateTo))
		for _, u := range d.EscalateTo {
			esc = append(esc, strconv.FormatInt(u, 10))
		}
		b.WriteString(`<li class="flex flex-col gap-2" data-officer="` + html.EscapeString(d.Officer) + `"><span class="text-sm text-fg">` +
			html.EscapeString(d.Officer) + ` · ` + html.EscapeString(d.Mode) + ` · enabled ` + strconv.FormatBool(d.Enabled) + `</span>`)
		if manager {
			checked := ""
			if d.Enabled {
				checked = " checked"
			}
			b.WriteString(`<form method="POST" action="/teams/` + id + `/officers/defs/` + html.EscapeString(d.Officer) + `" class="flex flex-wrap items-center gap-2">
<label class="text-sm text-fg"><input type="checkbox" name="enabled" value="1"` + checked + `> Enabled</label>
<label class="sr-only" for="mode-` + html.EscapeString(d.Officer) + `">Mode</label><select id="mode-` + html.EscapeString(d.Officer) + `" name="mode" class="` + adminInput + `">`)
			for _, m := range []string{"live", "shadow"} {
				sel := ""
				if m == d.Mode {
					sel = " selected"
				}
				b.WriteString(`<option value="` + m + `"` + sel + `>` + m + `</option>`)
			}
			b.WriteString(`</select><label class="sr-only" for="esc-` + html.EscapeString(d.Officer) + `">Escalate to (user or team ids)</label>
<input id="esc-` + html.EscapeString(d.Officer) + `" name="escalate_to" value="` + strings.Join(esc, ",") + `" class="` + adminInput + `" placeholder="escalate to: ids, comma-separated">
<button type="submit" class="` + adminQuiet + `">Save</button></form>`)
		} else if len(esc) > 0 {
			b.WriteString(`<span class="text-xs text-fg2">escalates to ` + strings.Join(esc, ", ") + `</span>`)
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></section>`)
	if manager {
		b.WriteString(`<section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">New officer type</h2>` +
			s.teamTypeForm(id, "", "", "") + `</section>`)
	}
	s.writeNavDocument(w, r, code, "Team officers", ui.NavAccount, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// teamTypeForm is a manager's form for type key ("" for a new one).
func (s *Server) teamTypeForm(team, key, name, charter string) string {
	action := `/teams/` + team + `/officers/types/` + html.EscapeString(key)
	keyField := ""
	if key == "" {
		action = `/teams/` + team + `/officers/types/new`
		keyField = `<label for="type-key-new" class="text-sm font-medium text-fg">Key</label><input id="type-key-new" name="key" required class="` + adminInput + `">`
	}
	sfx := key
	if sfx == "" {
		sfx = "new"
	}
	return `<form method="POST" action="` + action + `" class="flex flex-col gap-2">` + keyField +
		`<label for="type-name-` + html.EscapeString(sfx) + `" class="text-sm font-medium text-fg">Name</label><input id="type-name-` + html.EscapeString(sfx) +
		`" name="name" required value="` + html.EscapeString(name) + `" class="` + adminInput + `">` +
		`<label for="type-charter-` + html.EscapeString(sfx) + `" class="text-sm font-medium text-fg">Charter</label><textarea id="type-charter-` + html.EscapeString(sfx) +
		`" name="charter" required rows="6" class="` + adminInput + `">` + html.EscapeString(charter) + `</textarea>` +
		`<button type="submit" class="` + adminButton + `">Save charter</button></form>`
}

// teamOfficerWrite is a write's gate: team {id}, and the principal manages it.
func (s *Server) teamOfficerWrite(w http.ResponseWriter, r *http.Request) (int64, bool) {
	team, role, ok := s.teamOfficerRole(w, r)
	if !ok {
		return 0, false
	}
	if role != authz.TeamManager {
		s.renderTeamOfficers(w, r, team, role, http.StatusForbidden, TeamOfficerManagerOnly)
		return 0, false
	}
	return team, true
}

// teamOfficerDone answers a manager's write.
func (s *Server) teamOfficerDone(w http.ResponseWriter, r *http.Request, team int64, what string, err error) {
	if err == nil {
		http.Redirect(w, r, "/teams/"+strconv.FormatInt(team, 10)+"/officers", http.StatusSeeOther)
		return
	}
	code, msg := http.StatusInternalServerError, "That change could not be made. Try again."
	switch {
	case errors.Is(err, builtin.ErrInvalid):
		code, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, builtin.ErrNotFound):
		// The team exists (teamOfficerWrite found it); the form named a type
		// it does not have: a bad form, not a missing page.
		code, msg = http.StatusBadRequest, "Define that officer's type first."
	default:
		s.logger.Printf("teams: %s: %v", what, err)
	}
	s.renderTeamOfficers(w, r, team, authz.TeamManager, code, msg)
}

func (s *Server) handleTeamOfficerTypePut(w http.ResponseWriter, r *http.Request) {
	team, ok := s.teamOfficerWrite(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if key == "new" {
		key = strings.TrimSpace(r.FormValue("key"))
	}
	t, err := s.officerStore.PutTypeReturning(r.Context(), team, builtin.Type{Key: key, Name: strings.TrimSpace(r.FormValue("name")), Charter: r.FormValue("charter")})
	if err == nil {
		s.logger.Printf("teams: manager %d put team %d's officer type %s (charter version %d)", authz.MustFrom(r.Context()).UserID, team, t.Key, t.CharterVersion)
	}
	s.teamOfficerDone(w, r, team, "put team officer type", err)
}

func (s *Server) handleTeamOfficerDefPut(w http.ResponseWriter, r *http.Request) {
	team, ok := s.teamOfficerWrite(w, r)
	if !ok {
		return
	}
	// 🔴 escalate_to MAY NAME A TEAM HERE, and only here: this team, or a team
	// the manager operates (read from their own memberships). An escalation
	// to a team is ONE ask the team owns, every member sees it and one answers
	// it (design §4). Every other id must be a person (escalationRecipientsMissing,
	// the JSON admin API's own check — which never admits a team: a personal
	// API token carries no team access).
	var esc, persons []int64
	scope := authz.MustFrom(r.Context()).TeamScope()
	for _, f := range strings.FieldsFunc(r.FormValue("escalate_to"), func(c rune) bool { return c == ',' || c == ' ' }) {
		u, err := strconv.ParseInt(f, 10, 64)
		if err != nil || u <= 0 {
			s.renderTeamOfficers(w, r, team, authz.TeamManager, http.StatusBadRequest, "Escalate to holds user or team ids (positive numbers).")
			return
		}
		esc = append(esc, u)
		if role, in := scope.RoleIn(u); u != team && !(in && (role == authz.TeamOperator || role == authz.TeamManager)) {
			persons = append(persons, u)
		}
	}
	if missing, err := s.escalationRecipientsMissing(r.Context(), persons); err != nil || len(missing) > 0 {
		s.renderTeamOfficers(w, r, team, authz.TeamManager, http.StatusBadRequest, "Escalate to names someone who is not a person here, or a team you do not operate.")
		return
	}
	d, err := s.officerStore.PutDefReturning(r.Context(), team, builtin.Def{Officer: r.PathValue("officer"), Enabled: r.FormValue("enabled") != "",
		Mode: r.FormValue("mode"), EscalateTo: esc})
	if err == nil {
		s.logger.Printf("teams: manager %d put team %d's officer %s (enabled %v, mode %s)", authz.MustFrom(r.Context()).UserID, team, d.Officer, d.Enabled, d.Mode)
	}
	s.teamOfficerDone(w, r, team, "put team officer", err)
}
