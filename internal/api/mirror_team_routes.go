package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// TEAM HOSTS ON THE MIRROR ROUTES (team sharing 2b.3, "viewers";
// docs/design/team-sharing.md A, "Who sees and does what" and "The
// chokepoint"). A team host's sessions are listed to its members beside their
// own (mirror.Service.VisibleHosts), and a page of one names its owner:
// `owner=<team id>` beside `host=&session=&window=`. A missing owner, or the
// principal's own id, is the principal's own host: every link from before
// teams keeps working.
//
// mirrorTeamRoutes decides, ONCE, in admit (mirrorTeamGate), what each mirror
// route does with an owner that is not the principal's:
//
//   - mirrorTeamView: a member at viewer and above reads — the pages, the
//     read-only terminal, the Rendered stream, a subagent's steps, the
//     window's question cards, a scroll. The request is aimed at the team's
//     hub entries (mirror.ForOwner; the plugin's access decides again) and
//     registered for the membership sweep (team_live.go), so it ends when the
//     member leaves.
//   - mirrorTeamRefused: the model-backed extras — the chief panel and its
//     log, proposals, succession and suggested replies. Each sends the
//     session's transcript to a model on a PERSON's credentials or a
//     person's chief, which D-1113 #10 and G4 forbid for a team's. 403 at
//     every role.
//   - mirrorTeamOwnOnly: own sessions only in this release — typing (a send,
//     a queued send, an answer typed into the pane, the composer's catalog)
//     is slice 2b.4's, with its step-up and its queue checks; the preview
//     button reads the viewer's PERSONAL viewer plugins. A member is answered
//     403; anyone else 404.
//   - mirrorTeamHostDoor: a host's own door (its mirror token): the owner is
//     the token's, and no owner= is read.
//
// A non-member naming a team is answered 404 on every route: the same answer
// as a host that does not exist, so it is no oracle. TestEveryMirrorRouteHasATeamRule
// pins this table to every /mirror/, /ui/mirror/ and /send-queue route in
// routes.golden, so a new one is a decision, not a default.
// ---------------------------------------------------------------------------

// mirrorTeamRule is what a mirror route does with a team owner.
type mirrorTeamRule int

const (
	mirrorTeamOwnOnly mirrorTeamRule = iota
	mirrorTeamView
	mirrorTeamRefused
	mirrorTeamHostDoor
)

// mirrorTeamRoutes is every mirror route, with its rule.
var mirrorTeamRoutes = map[string]mirrorTeamRule{
	"GET /mirror/connect": mirrorTeamHostDoor,
	"GET /mirror/host":    mirrorTeamHostDoor,
	"POST /mirror/ask":    mirrorTeamHostDoor,
	"POST /mirror/mcp":    mirrorTeamHostDoor,

	"GET /mirror/terminal":         mirrorTeamView,
	"GET /mirror/terminal/ws":      mirrorTeamView, // read-only: live keys need operator, which access refuses on a team host until 2b.4
	"POST /mirror/terminal/scroll": mirrorTeamView, // moves copy-mode for every watcher (D-1113 #7)
	"GET /mirror/view":             mirrorTeamView,
	"GET /mirror/view/ws":          mirrorTeamView,
	"GET /mirror/view/subagent":    mirrorTeamView,
	"GET /mirror/view/asks":        mirrorTeamView, // the window's question cards, drawn unanswerable for a team

	"GET /mirror/view/chief":          mirrorTeamRefused,
	"POST /mirror/view/chief":         mirrorTeamRefused,
	"GET /mirror/view/chief/log":      mirrorTeamRefused,
	"GET /mirror/view/proposal":       mirrorTeamRefused,
	"POST /mirror/view/proposal":      mirrorTeamRefused,
	"GET /mirror/view/successor":      mirrorTeamRefused,
	"GET /mirror/view/suggest":        mirrorTeamRefused,
	"POST /mirror/view/suggest/event": mirrorTeamRefused,

	"GET /ui/mirror/preview-button":           mirrorTeamOwnOnly, // the viewer's personal viewer plugins
	"GET /mirror/view/complete":               mirrorTeamOwnOnly, // the composer's catalog: operator (2b.4)
	"GET /mirror/terminal/send":               mirrorTeamOwnOnly,
	"POST /mirror/terminal/send":              mirrorTeamOwnOnly,
	"GET /mirror/view/send":                   mirrorTeamOwnOnly,
	"POST /mirror/view/send":                  mirrorTeamOwnOnly,
	"POST /mirror/confirm-send":               mirrorTeamOwnOnly,
	"POST /mirror/view/ask":                   mirrorTeamOwnOnly,
	"POST /mirror/view/ask/cancel":            mirrorTeamOwnOnly,
	"POST /mirror/view/ask/message":           mirrorTeamOwnOnly,
	"GET /send-queue":                         mirrorTeamOwnOnly,
	"POST /send-queue":                        mirrorTeamOwnOnly,
	"POST /send-queue/confirm":                mirrorTeamOwnOnly,
	"POST /send-queue/{id}/cancel":            mirrorTeamOwnOnly,
	"POST /send-queue/{id}/delete":            mirrorTeamOwnOnly,
	"POST /send-queue/{id}/edit":              mirrorTeamOwnOnly,
	"POST /send-queue/{id}/edit/cancel":       mirrorTeamOwnOnly,
	"POST /send-queue/{id}/edit/confirm":      mirrorTeamOwnOnly,
	"POST /send-queue/{id}/edit/save":         mirrorTeamOwnOnly,
	"POST /send-queue/{id}/edit/save/confirm": mirrorTeamOwnOnly,
	"POST /send-queue/{id}/retry":             mirrorTeamOwnOnly,
	"POST /send-queue/{id}/retry/confirm":     mirrorTeamOwnOnly,
	"POST /send-queue/{id}/send-now":          mirrorTeamOwnOnly,
	"POST /send-queue/{id}/send-now/confirm":  mirrorTeamOwnOnly,
}

// What a member is answered on a team session's route that does not take one.
const (
	// MirrorTeamExtraRefusal: a model-backed extra (mirrorTeamRefused).
	MirrorTeamExtraRefusal = "this is a team session: the chief panel, proposals, succession and suggested replies are off on team sessions " +
		"(they would send its transcript to a person's model or chief)"
	// MirrorTeamOwnOnlyRefusal: typing, and the preview (mirrorTeamOwnOnly).
	MirrorTeamOwnOnlyRefusal = "this is a team session: you can watch it here; typing into it, and its preview, are not available yet"
)

// mirrorOwnerParam is the request's owner= (the query; for a POST, its form
// too), "" for none.
func mirrorOwnerParam(r *http.Request) string {
	if v := r.URL.Query().Get("owner"); v != "" {
		return v
	}
	if r.Method == http.MethodPost {
		return r.PostFormValue("owner")
	}
	return ""
}

// mirrorTeamGate is admit's half of mirrorTeamRoutes: it answers a refusal
// itself (ok false), or returns the request to serve — aimed at the team's
// hub entries and registered for the membership sweep when a member's
// mirrorTeamView request names their team (done unregisters it; nil when
// nothing was registered). Every request whose owner= is absent or the
// principal's own passes unchanged, and so does every route not in
// mirrorTeamRoutes (mirrorEntry refuses an owner= there).
//
// 🔴 THE ONE PLACE A MIRROR CALL IS AIMED AT ANOTHER OWNER (mirror.ForOwner;
// TestOnlyTheMirrorTeamGateAimsAMirrorCall). It grants nothing by itself: the
// plugin's access re-reads the role from the same principal.
func (s *Server) mirrorTeamGate(w http.ResponseWriter, r *http.Request) (*http.Request, func(), bool) {
	rule, ok := mirrorTeamRoutes[r.Pattern]
	if !ok || rule == mirrorTeamHostDoor {
		return r, nil, true
	}
	// 🔴 A POST'S BODY IS READ HERE ONLY ON A mirrorTeamView ROUTE (the
	// scroll): parsing it ahead of every handler would spend a body a
	// handler bounds itself (its MaxBytesReader, and the "too-large" reason
	// its answer path records) before that handler ran. On any other route an
	// owner= in the body is the handler's mirrorEntry's to refuse (404).
	raw := r.URL.Query().Get("owner")
	if raw == "" && rule == mirrorTeamView && r.Method == http.MethodPost {
		// Under the scroll handler's own bound (#690 round 1, 🟢2): its body
		// is parsed here first, so the limit and its 413 are applied here.
		r.Body = http.MaxBytesReader(w, r.Body, mirrorScrollBodyMax)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
			return nil, nil, false
		}
		raw = r.PostFormValue("owner")
	}
	if raw == "" {
		return r, nil, true
	}
	p, pok := authz.From(r.Context())
	o, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || o <= 0 || !pok || p.All() {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if o == p.UserID {
		return r, nil, true
	}
	role, in := p.TeamScope().RoleIn(o)
	if !in || !role.AtLeast(authz.TeamViewer) {
		http.NotFound(w, r) // no oracle: as a host that does not exist
		return nil, nil, false
	}
	switch rule {
	case mirrorTeamView:
		ctx, done := s.teamLiveOpen(mirror.ForOwner(r.Context(), o), o, p.UserID, authz.TeamViewer, errTeamStreamClosed)
		// Read the role AGAIN, after registering (#690 round 1, 🟢1; as
		// teamTerminalOpen does): a removal that landed between the
		// principal's read and the registration ran its close before this
		// request was there to be closed. Not a member now: the same 404.
		if !s.teamMemberNow(ctx, p.UserID, o, authz.TeamViewer) {
			done()
			http.NotFound(w, r)
			return nil, nil, false
		}
		return r.WithContext(ctx), done, true
	case mirrorTeamRefused:
		http.Error(w, MirrorTeamExtraRefusal, http.StatusForbidden)
	default:
		http.Error(w, MirrorTeamOwnOnlyRefusal, http.StatusForbidden)
	}
	return nil, nil, false
}

// mirrorScrollBodyMax is the scroll request's body bound (handleMirrorTerminalScroll).
const mirrorScrollBodyMax = 4 << 10

// teamMemberNow reads person's role in team fresh (accountOfChecked) and
// reports whether it reaches need. A failed read is false: fail closed.
func (s *Server) teamMemberNow(ctx context.Context, person, team int64, need authz.TeamRole) bool {
	a, err := s.accountOfChecked(ctx, person)
	if err != nil {
		s.logger.Printf("team streams: read user %d's role in team %d: %v (not a member)", person, team, err)
		return false
	}
	role, in := a.Teams.RoleIn(team)
	return !a.Team && in && role.AtLeast(need)
}

// mirrorTarget is the owner a request's mirror calls are about: its team
// (mirrorTeamGate aimed it there) or the principal's own.
func mirrorTarget(r *http.Request) int64 {
	if t := mirror.TargetOwner(r.Context()); t > 0 {
		return t
	}
	return requestOwner(r.Context())
}

// onTeamHost reports whether r is a member's request on a TEAM host's entry
// (mirrorTeamGate aimed it there): the pages then leave out every extra that
// is the viewer's own — their recents, tags, pins, names, drafts, chief,
// proposals, suggestions and queue — and draw the composer disabled.
func onTeamHost(r *http.Request) bool {
	t := mirror.TargetOwner(r.Context())
	return t > 0 && t != requestOwner(r.Context())
}
