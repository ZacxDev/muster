package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/jackc/pgx/v5"
)

// Team tasks: who may do what is the table in docs/design/team-sharing.md, slice 4.3.

// taskRoleAllows reports whether p may act at min on a task owner owns: its
// own task always; a team's only at min or above in that team. owner 0 is a
// store that does not read the column (a test fake), where there is nothing
// to decide.
func taskRoleAllows(p authz.Principal, owner int64, min authz.TeamRole) bool {
	if owner == 0 || owner == p.UserID {
		return true
	}
	role, in := p.TeamScope().RoleIn(owner)
	return in && role.AtLeast(min)
}

// requireTaskOperator is requireTaskRole at operator: file, comment, change
// status, edit, tag, retract a comment, retry a reference write.
func (s *Server) requireTaskOperator(next http.HandlerFunc) http.HandlerFunc {
	return s.requireTaskRole(authz.TeamOperator, next)
}

// requireTaskManager is requireTaskRole at manager: dismiss.
func (s *Server) requireTaskManager(next http.HandlerFunc) http.HandlerFunc {
	return s.requireTaskRole(authz.TeamManager, next)
}

// requireTaskRole wraps a /tasks/{id}/… write: on a team's task it answers
// 403 unless the caller holds min in that team. A task the caller cannot see
// (or no task: pgx.ErrNoRows) passes to next, which answers its own 404, so
// this is no oracle for which ids exist. Any other read error is a 500: the
// gate cannot decide, so it does not let the write through.
func (s *Server) requireTaskRole(min authz.TeamRole, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil && s.ext.Notes != nil {
			p, hasP := authz.From(r.Context())
			n, err := s.ext.Notes.Get(r.Context(), id)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				s.logger.Printf("notes: team-task role check on %d: %v", id, err)
				http.Error(w, "could not load the task", http.StatusInternalServerError)
				return
			case hasP && !taskRoleAllows(p, n.OwnerID, min):
				http.Error(w, "this is a team task: it needs a team "+string(min)+" or above", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// teamNamer is the users store's team-name read (users.PGStore.TeamNames).
type teamNamer interface {
	TeamNames(ctx context.Context, ids []int64) (map[int64]string, error)
}

// teamNames names teams for display; a team the read cannot name (no users
// store, a failed read) is "Team <id>".
func (s *Server) teamNames(ctx context.Context, ids []int64) map[int64]string {
	names := map[int64]string{}
	if tn, ok := s.ext.Users.(teamNamer); ok && len(ids) > 0 {
		if got, err := tn.TeamNames(ctx, ids); err == nil {
			names = got
		} else {
			s.logger.Printf("teams: name teams %v: %v", ids, err)
		}
	}
	for _, id := range ids {
		if strings.TrimSpace(names[id]) == "" {
			names[id] = "Team " + strconv.FormatInt(id, 10)
		}
	}
	return names
}

// operatorTeams are the teams the request's principal may file a task in or
// move one into: at least an operator, through a door that carries teams.
func (s *Server) operatorTeams(ctx context.Context) []ui.TaskOwnerOption {
	p, ok := authz.From(ctx)
	if !ok {
		return nil
	}
	var ids []int64
	for _, m := range p.TeamScope().List() {
		if m.Role.AtLeast(authz.TeamOperator) {
			ids = append(ids, m.Team)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	names := s.teamNames(ctx, ids)
	out := make([]ui.TaskOwnerOption, 0, len(ids))
	for _, id := range ids {
		out = append(out, ui.TaskOwnerOption{ID: id, Label: names[id]})
	}
	return out
}

// moveTeamsFor is the edit modal's move targets for n: the caller's operator
// teams, when the task is the caller's own (and not a plugin reference,
// which MoveToTeam refuses anyway).
func (s *Server) moveTeamsFor(ctx context.Context, n notes.Note) []ui.TaskOwnerOption {
	p, ok := authz.From(ctx)
	if !ok || n.OwnerID != p.UserID || n.IsReference() {
		return nil
	}
	return s.operatorTeams(ctx)
}

// taskOwnerFromForm is the owner a create form names: 0 (the caller) for none
// or the caller's own id, else a team the caller is at least an operator in.
// ok is false for anything else (a team they only view, a team they are not
// in, a person, junk).
func taskOwnerFromForm(p authz.Principal, raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	if id == p.UserID {
		return 0, true
	}
	role, in := p.TeamScope().RoleIn(id)
	return id, in && role.AtLeast(authz.TeamOperator)
}

// taskOwnerGroups splits the board's cards by owner: the caller's own first
// ("Yours"), then each team's, by name. Nil when every card is the caller's,
// so a board with no team task is the flat list it always was.
func (s *Server) taskOwnerGroups(ctx context.Context, cards []ui.TaskCardView) []ui.TaskOwnerGroup {
	p, ok := authz.From(ctx)
	if !ok {
		return nil
	}
	byOwner := map[int64][]ui.TaskCardView{}
	var teams []int64
	for _, c := range cards {
		o := c.Note.OwnerID
		if o == 0 {
			o = p.UserID
		}
		if _, seen := byOwner[o]; !seen && o != p.UserID {
			teams = append(teams, o)
		}
		byOwner[o] = append(byOwner[o], c)
	}
	if len(teams) == 0 {
		return nil
	}
	names := s.teamNames(ctx, teams)
	sort.Slice(teams, func(i, j int) bool {
		if names[teams[i]] != names[teams[j]] {
			return names[teams[i]] < names[teams[j]]
		}
		return teams[i] < teams[j]
	})
	var out []ui.TaskOwnerGroup
	if mine := byOwner[p.UserID]; len(mine) > 0 {
		out = append(out, ui.TaskOwnerGroup{Owner: p.UserID, Label: "Yours", Cards: mine})
	}
	for _, t := range teams {
		out = append(out, ui.TaskOwnerGroup{Owner: t, Team: true, Label: names[t], Cards: byOwner[t]})
	}
	return out
}

// handleTaskMove handles POST /tasks/{id}/move (form: team): move one of the
// caller's own tasks into a team they are at least an operator in — the task
// and its comments, attachments and session links, in one system-scope
// transaction (notes.PGStore.MoveToTeam). Refused, moving nothing: a team the
// caller does not operate in (403), a task that is not the caller's own (403;
// 404 when they cannot see it), and a task a personal link points at (409,
// naming each link).
func (s *Server) handleTaskMove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if s.ext.Notes == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	p := authz.MustFrom(ctx)
	team, ok := taskOwnerFromForm(p, r.FormValue("team"))
	if !ok || team == 0 {
		http.Error(w, "you can move a task only into a team you are an operator or manager of", http.StatusForbidden)
		return
	}
	n, err := s.ext.Notes.Get(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if n.OwnerID != p.UserID {
		http.Error(w, "only the task's owner can move it into a team", http.StatusForbidden)
		return
	}
	var blocked *notes.MoveBlockedError
	// The move itself is a SYSTEM write (owner_id changes nowhere else,
	// migration 0127's owner_fixed), made only after the two checks above;
	// the store re-checks the owner under the row lock.
	switch err := s.ext.Notes.MoveToTeam(authz.System(ctx), id, p.UserID, team); {
	case errors.As(err, &blocked):
		http.Error(w, "this task cannot move into the team: "+strings.Join(blocked.Links, "; "), http.StatusConflict)
		return
	case errors.Is(err, notes.ErrMoveNotYours):
		http.NotFound(w, r)
		return
	case err != nil:
		s.logger.Printf("notes: move %d to team %d: %v", id, team, err)
		http.Error(w, "could not move the task", http.StatusInternalServerError)
		return
	}
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(id, 10))
	moved, err := s.ext.Notes.Get(ctx, id)
	if err != nil {
		s.logger.Printf("notes: get %d after move: %v", id, err)
		http.Error(w, "moved; could not load the task", http.StatusInternalServerError)
		return
	}
	s.renderNoteCard(w, r, moved)
}
