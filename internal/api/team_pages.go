package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
)

// ---------------------------------------------------------------------------
// A TEAM'S PAGES, SEEN BY THE TEAM (migration 0165; docs/pages.md "A team's
// pages"). A page a team-owned agent makes belongs to the team; every member
// of the team (viewer and up) reads it through a door where a person is
// present (the browser session, a paired phone: authz.Principal.TeamScope),
// row-level security deciding which. What a member may then DO is decided
// here, once, in admit, for every route that names a page (teamPageRoutes):
//
//   - read, respond (the owner view's form, recorded as the member:
//     page_responses.responder_user) and Share (the page's one link) — any
//     member;
//   - delete, "Show this version" — only the page's CREATOR, the person who
//     started the agent that made it (pages.MayManage; with no creator
//     recorded, or one who has left the team, any manager of it);
//   - revoke a link, ⋯ Revoke link, ⋯ New link — the creator, and ANY manager
//     of the team, even while the creator is present (pages.MayRevoke: a
//     mistaken Share is undone at once; New link is a revoke and a Share,
//     both a manager's, so the database lets it too);
//   - send a FILE from the owner view — no member in this release (a file is
//     stored as the person who sends it, and a team's page keeps only files
//     the team owns: its quota and its delete read them as the team).
//
// The database holds every statement to the same rule (0165's
// muster_page_manager and guards); this gate is what answers with words, and
// what the screen's controls follow (renderPageDetail draws none of them for
// a member who may not use them).
// ---------------------------------------------------------------------------

// teamPageRule is what a team member may do through one page route, on a page
// their team owns.
type teamPageRule int

const (
	// teamPageRead: any member — row-level security decides who sees the page.
	teamPageRead teamPageRule = iota
	// teamPageRespond: any member — their response is recorded as theirs.
	teamPageRespond
	// teamPageShare: any member — the page's link goes public on a person's tap.
	teamPageShare
	// teamPageCreator: the page's creator only (pages.MayManage).
	teamPageCreator
	// teamPageRevoke: the creator, or any manager of the team
	// (pages.MayRevoke).
	teamPageRevoke
	// teamPageNoFile: no member (a file from the owner view).
	teamPageNoFile
)

// teamPageRoutes is EVERY route whose pattern names a page ("pages/{id}"),
// with its rule. TestEveryPageRouteHasATeamRule fails when routes.golden gains
// or loses one that is not here, so a new page route is a decision, not a
// default.
var teamPageRoutes = map[string]teamPageRule{
	"GET /pages/{id}":                      teamPageRead,
	"GET /pages/{id}/card":                 teamPageRead,
	"GET /pages/{id}/preview":              teamPageRead,
	"GET /pages/{id}/responses/{rid}/raw":  teamPageRead,
	"GET /pages/{id}/versions/{v}/content": teamPageRead,
	// 0164's author assets follow the page (0165): any member reads the
	// images and video the version they view shows.
	"GET /pages/{id}/versions/{v}/assets/{asset}": teamPageRead,
	"GET /pages/{id}/view":                        teamPageRead,
	"POST /pages/{id}/versions/{v}/submit":        teamPageRespond,
	"POST /pages/{id}/versions/{v}/feedback":      teamPageRespond,
	"POST /pages/{id}/share":                      teamPageShare,
	"POST /pages/{id}/delete":                     teamPageCreator,
	"POST /pages/{id}/rollback":                   teamPageCreator,
	"POST /pages/{id}/links/{link}/revoke":        teamPageRevoke,
	"POST /pages/{id}/share/rotate":               teamPageRevoke,
	"POST /pages/{id}/share/revoke":               teamPageRevoke,
	"POST /pages/{id}/versions/{v}/upload":        teamPageNoFile,
}

// The refusals a member reads.
const (
	// TeamPageCreatorOnly: a creator-only route, to any other member.
	TeamPageCreatorOnly = "this is a team's page: only the person who started the agent that made it can delete it " +
		"or change its version (a manager of the team can when that person has left the team)"
	// TeamPageRevokeOnly: a revoke, to a member who is neither the creator
	// nor a manager of the team.
	TeamPageRevokeOnly = "this is a team's page: only the person who started the agent that made it, or a manager of " +
		"the team, can revoke its links or make a new one"
	// TeamPageNoFile: a file from a team page's owner view, to any member.
	TeamPageNoFile = "this is a team's page: files can't be sent from a team page yet"
)

// teamPageGate answers 403 and reports true when r names a page its
// principal's TEAM owns and the route's rule refuses the principal. Called by
// admit, after the principal is attached.
//
// It costs nothing for a principal with no team memberships on its door (no
// read): such a principal cannot see a team's page. A page the principal
// cannot see, or that does not exist, passes to the handler (its own 404, no
// oracle); any other read error is a 500 — the gate cannot decide, so it does
// not let the request through.
func (s *Server) teamPageGate(w http.ResponseWriter, r *http.Request) bool {
	rule, ok := teamPageRoutes[r.Pattern]
	if !ok || (rule != teamPageCreator && rule != teamPageRevoke && rule != teamPageNoFile) || s.ext.Pages == nil {
		return false
	}
	ctx := r.Context()
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.TeamScope() == "" {
		return false
	}
	id, ok := pageIDParam(r)
	if !ok {
		return false
	}
	pg, err := s.ext.Pages.Page(ctx, id)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return false
	case err != nil:
		s.logger.Printf("team pages: %s: read page %d: %v", r.Pattern, id, err)
		http.Error(w, "could not load the page", http.StatusInternalServerError)
		return true
	}
	if !memberOnTeamPage(ctx, pg) {
		return false
	}
	switch rule {
	case teamPageNoFile:
		s.pageWriteError(w, http.StatusForbidden, TeamPageNoFile)
		return true
	case teamPageCreator, teamPageRevoke:
		msg := TeamPageCreatorOnly
		if rule == teamPageRevoke {
			if pages.MayRevoke(pg, p) {
				return false
			}
			msg = TeamPageRevokeOnly
		} else if pages.MayManage(pg, p) {
			return false
		}
		if wantsJSON(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": msg})
		} else {
			http.Error(w, msg, http.StatusForbidden)
		}
		return true
	}
	return false
}

// memberOnTeamPage reports whether ctx is a PERSON reading page pg, which a
// team of theirs owns (not their own, not System, not the team acting
// principal).
func memberOnTeamPage(ctx context.Context, pg pages.Page) bool {
	p, ok := authz.From(ctx)
	if !ok || p.All() || pg.OwnerID <= 0 || pg.OwnerID == p.UserID {
		return false
	}
	_, in := p.TeamScope().RoleIn(pg.OwnerID)
	return in
}

// pageManageable reports whether ctx may manage page pg (pages.MayManage):
// what the page's screen draws its Delete, Show this version and ⋯ New link
// for. Without a principal (a test's bare context) the page is the caller's
// own, as every page was before 0165.
func pageManageable(ctx context.Context, pg pages.Page) bool {
	p, ok := authz.From(ctx)
	if !ok {
		return true
	}
	return pages.MayManage(pg, p)
}

// pageRevocable reports whether ctx may revoke page pg's links
// (pages.MayRevoke): what the screen draws each link's Revoke and ⋯ Revoke
// link for.
func pageRevocable(ctx context.Context, pg pages.Page) bool {
	p, ok := authz.From(ctx)
	if !ok {
		return true
	}
	return pages.MayRevoke(pg, p)
}
