package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// --- Account → Teams: a member's own notify switch, per team -----------------
// (docs/design/team-sharing.md, phase 2b, part C; operator decision G6)
//
// A list of the signed-in person's teams, their role in each, and ONE switch
// per team: whether that team's pushes reach them (team_members.notify,
// migration 0133). It is the member's own setting: the write updates only
// the caller's own row, only while they are a member (users.PGStore.SetTeamNotify),
// and a manager has no form that sets it for anyone else. Off means no push
// from that team at all (goNotify's fan-out reads it); the team's items are
// still on Attention. A withdrawal still reaches a member with it off, so a
// notification shown before the switch went off is still closed.
//
// The page is the session tier; the write is requireOperatorWrite (same
// origin). A team the caller is not a member of — or that does not exist — is
// one 404, so the route is no oracle for which teams exist.

// accountTeamsPath is the page.
const accountTeamsPath = "/account/teams"

// The page's words.
const (
	accountTeamsTitle   = "Teams"
	accountTeamsNone    = "You are not in any team."
	accountTeamsHelp    = "Operators and managers get a team's notifications (its asks, drafts, task comments and the rest). Turn a team's off here: its items stay on Attention."
	accountTeamsViewer  = "Viewers get no notifications."
	accountTeamsOnLabel = "Notifications from this team"
)

// accountTeamsStore is the account store's two reads and one write for this
// page (users.PGStore), asserted so a store without them serves the 404.
type accountTeamsStore interface {
	MyTeams(ctx context.Context, user int64) ([]users.MyTeam, error)
	SetTeamNotify(ctx context.Context, user, team int64, on bool) error
}

func (s *Server) registerAccountTeams(mux Mux) {
	mux.HandleFunc("GET "+accountTeamsPath, s.requireSession(s.handleAccountTeams))
	mux.HandleFunc("POST "+accountTeamsPath+"/{id}/notify", s.requireOperatorWrite(s.handleAccountTeamNotify))
}

// accountTeams is the account store, or nil when it cannot serve this page.
func (s *Server) accountTeams() accountTeamsStore {
	if s.gt == nil || s.gt.Accounts == nil {
		return nil
	}
	st, _ := s.gt.Accounts.(accountTeamsStore)
	return st
}

func (s *Server) handleAccountTeams(w http.ResponseWriter, r *http.Request) {
	st := s.accountTeams()
	if st == nil {
		s.renderNotice(w, r, http.StatusNotFound, "No teams here", "This server has no team accounts.")
		return
	}
	ctx := r.Context()
	teams, err := st.MyTeams(ctx, authz.MustFrom(ctx).UserID)
	if err != nil {
		s.logger.Printf("account: read user %d's teams: %v", authz.MustFrom(ctx).UserID, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load your teams", "Try again in a moment.")
		return
	}
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">` + accountTeamsTitle + `</h1>`)
	if r.URL.Query().Get("saved") == "1" {
		b.WriteString(`<p role="status" class="text-sm font-semibold text-fg">Saved.</p>`)
	}
	if len(teams) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" id="account-teams-none">` + accountTeamsNone + `</p></section>`)
	} else {
		b.WriteString(`<p class="text-sm text-fg2">` + html.EscapeString(accountTeamsHelp) + `</p><ul class="flex flex-col gap-3" id="account-teams">`)
		for _, t := range teams {
			id := strconv.FormatInt(t.ID, 10)
			on, next, label := "off", "1", "Turn on"
			if t.Notify {
				on, next, label = "on", "0", "Turn off"
			}
			b.WriteString(`<li class="flex flex-col gap-1" data-team-id="` + id + `" data-team-role="` + string(t.Role) + `" data-team-notify="` + on + `">` +
				`<span class="text-sm font-semibold text-fg">` + html.EscapeString(t.Name) + `</span>` +
				`<span class="text-sm text-fg2">Your role: ` + string(t.Role) + `</span>`)
			if !t.Role.AtLeast(authz.TeamOperator) {
				b.WriteString(`<span class="text-sm text-fg2">` + accountTeamsViewer + `</span>`)
			}
			b.WriteString(`<form method="POST" action="` + accountTeamsPath + `/` + id + `/notify" class="flex flex-wrap items-center gap-2">` +
				`<input type="hidden" name="on" value="` + next + `">` +
				`<span class="text-sm text-fg">` + accountTeamsOnLabel + `: <span class="font-semibold">` + on + `</span></span>` +
				`<button type="submit" class="` + adminQuiet + `" id="team-notify-` + id + `">` + label + `</button></form></li>`)
		}
		b.WriteString(`</ul></section>`)
	}
	s.writeNavDocument(w, r, http.StatusOK, accountTeamsTitle, ui.NavAccount, false,
		`<div class="flex flex-col gap-4">`+b.String()+`<a href="/account" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to Account</a></div>`)
}

func (s *Server) handleAccountTeamNotify(w http.ResponseWriter, r *http.Request) {
	st := s.accountTeams()
	team, ok := pathID(r)
	if st == nil || !ok {
		http.NotFound(w, r)
		return
	}
	on := r.FormValue("on")
	if on != "1" && on != "0" {
		http.Error(w, "on is 1 or 0", http.StatusBadRequest)
		return
	}
	me := authz.MustFrom(r.Context()).UserID
	if err := st.SetTeamNotify(r.Context(), me, team, on == "1"); err != nil {
		if errors.Is(err, users.ErrNotAMember) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("account: user %d's notify switch for team %d: %v", me, team, err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, accountTeamsPath+"?saved=1", http.StatusSeeOther)
}
