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
//   - mirrorTeamOperate: typing (team sharing 2b.4) — a send, a queued send
//     and its queue, an answer typed into the pane, the composer's catalog.
//     A member at OPERATOR and above, aimed at the team and registered for
//     the sweep at operator, so a removal or a drop to viewer ends it; the
//     handler's own step-up gate (stepup.Fresh, the member's own sign-in)
//     and the plugin's access (operator) decide again. A viewer is answered
//     403 (MirrorTeamViewerRefusal).
//   - mirrorTeamViewOrKeys: the terminal's socket — a view at viewer, and
//     live keys (mode=interactive) at operator, as mirrorTeamOperate.
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
	mirrorTeamOperate mirrorTeamRule = iota
	mirrorTeamView
	mirrorTeamViewOrKeys
	mirrorTeamRefused
	mirrorTeamHostDoor
)

// mirrorTeamRoutes is every mirror route, with its rule.
var mirrorTeamRoutes = map[string]mirrorTeamRule{
	"GET /mirror/connect": mirrorTeamHostDoor,
	"GET /mirror/host":    mirrorTeamHostDoor,
	"POST /mirror/ask":    mirrorTeamHostDoor,
	"POST /mirror/mcp":    mirrorTeamHostDoor,
	// An attention_ask's file, sent by the host's MCP proxy (ask_media.go).
	"POST /mirror/ask-attachments": mirrorTeamHostDoor,
	// …and its take-back when the ask was not filed (ask_media.go).
	"DELETE /mirror/ask-attachments/{id}": mirrorTeamHostDoor,
	// A page's image or video, sent by the host's MCP proxy (page_assets.go).
	"POST /mirror/pages/{id}/assets": mirrorTeamHostDoor,

	"GET /mirror/terminal":         mirrorTeamView,
	"GET /mirror/terminal/ws":      mirrorTeamViewOrKeys, // read-only at viewer; live keys (mode=interactive) at operator
	"POST /mirror/terminal/scroll": mirrorTeamView,       // moves copy-mode for every watcher (D-1113 #7)
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

	"GET /ui/mirror/preview-button":           mirrorTeamView,    // on the TEAM's viewer plugins (2b.3b), never the viewer's
	"GET /ui/mirror/session-pages":            mirrorTeamView,    // a team window lists no pages (windowPagesRef)
	"GET /ui/mirror/session-asks":             mirrorTeamView,    // nor asks (windowAsksQuery)
	"GET /mirror/view/complete":               mirrorTeamOperate, // the composer's catalog
	"GET /mirror/terminal/send":               mirrorTeamOperate,
	"POST /mirror/terminal/send":              mirrorTeamOperate,
	"GET /mirror/view/send":                   mirrorTeamOperate,
	"POST /mirror/view/send":                  mirrorTeamOperate,
	"POST /mirror/confirm-send":               mirrorTeamOperate,
	"POST /mirror/view/ask":                   mirrorTeamOperate,
	"POST /mirror/view/ask/cancel":            mirrorTeamOperate,
	"POST /mirror/view/ask/message":           mirrorTeamOperate,
	"GET /send-queue":                         mirrorTeamOperate,
	"POST /send-queue":                        mirrorTeamOperate,
	"POST /send-queue/confirm":                mirrorTeamOperate,
	"POST /send-queue/{id}/cancel":            mirrorTeamOperate,
	"POST /send-queue/{id}/delete":            mirrorTeamOperate,
	"POST /send-queue/{id}/edit":              mirrorTeamOperate,
	"POST /send-queue/{id}/edit/cancel":       mirrorTeamOperate,
	"POST /send-queue/{id}/edit/confirm":      mirrorTeamOperate,
	"POST /send-queue/{id}/edit/save":         mirrorTeamOperate,
	"POST /send-queue/{id}/edit/save/confirm": mirrorTeamOperate,
	"POST /send-queue/{id}/retry":             mirrorTeamOperate,
	"POST /send-queue/{id}/retry/confirm":     mirrorTeamOperate,
	"POST /send-queue/{id}/send-now":          mirrorTeamOperate,
	"POST /send-queue/{id}/send-now/confirm":  mirrorTeamOperate,
}

// What a member is answered on a team session's route that does not take one.
const (
	// MirrorTeamExtraRefusal: a model-backed extra (mirrorTeamRefused).
	MirrorTeamExtraRefusal = "this is a team session: the chief panel, proposals, succession and suggested replies are off on team sessions " +
		"(they would send its transcript to a person's model or chief)"
	// MirrorTeamViewerRefusal: typing (mirrorTeamOperate, and live keys on
	// mirrorTeamViewOrKeys) by a member below operator.
	MirrorTeamViewerRefusal = "this is a team session and you are a viewer of its team: you can watch it; an operator or a manager types into it"
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
// hub entries and registered for the membership sweep, at the role the
// route needs (mirrorTeamNeed), when a member's request names their team
// (done unregisters it; nil when nothing was registered). Every request whose owner= is absent or the
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
	if rule == mirrorTeamRefused {
		http.Error(w, MirrorTeamExtraRefusal, http.StatusForbidden)
		return nil, nil, false
	}
	need := mirrorTeamNeed(rule, r)
	if !role.AtLeast(need) {
		http.Error(w, MirrorTeamViewerRefusal, http.StatusForbidden)
		return nil, nil, false
	}
	ctx, done := s.teamLiveOpen(mirror.ForOwner(r.Context(), o), o, p.UserID, need, errTeamStreamClosed)
	// Read the role AGAIN, after registering (#690 round 1, 🟢1; as
	// teamAgentSocketOpen does): a removal or a drop that landed between the
	// principal's read and the registration ran its close before this
	// request was there to be closed. Not a member now: the same 404; a
	// member below the need now: the viewer's 403.
	if !s.teamMemberNow(ctx, p.UserID, o, need) {
		done()
		if need != authz.TeamViewer && s.teamMemberNow(ctx, p.UserID, o, authz.TeamViewer) {
			http.Error(w, MirrorTeamViewerRefusal, http.StatusForbidden)
		} else {
			http.NotFound(w, r)
		}
		return nil, nil, false
	}
	return r.WithContext(ctx), done, true
}

// mirrorTeamNeed is the role a member's request on a team session needs under
// rule: operator to type (mirrorTeamOperate, live keys), viewer to watch.
func mirrorTeamNeed(rule mirrorTeamRule, r *http.Request) authz.TeamRole {
	switch {
	case rule == mirrorTeamOperate:
		return authz.TeamOperator
	case rule == mirrorTeamViewOrKeys && r.URL.Query().Get("mode") == terminalModeInteractive:
		return authz.TeamOperator
	}
	return authz.TeamViewer
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

// windowOwner is whose WINDOWS ctx's mirror calls touch: the team the route
// gate aimed it at (mirror.TargetOwner), else the principal's own — the team
// acting principal's own being the team (the send queue's dispatcher on a
// team's row). Everything keyed by a window's address beside its owner — the
// send hold, the nonce claim, the queue's turn stamps — is keyed by this, so
// a member's Send and the dispatcher's delivery into one team window meet in
// one place, and a member's own build-01 never meets the team's.
func windowOwner(ctx context.Context) int64 {
	if t := mirror.TargetOwner(ctx); t > 0 {
		return t
	}
	return requestOwner(ctx)
}

// windowHosts is the hosts of windowOwner(ctx): the principal's own (Hosts),
// or the aimed-at team's (teamHostsOf). Pick an entry from it by host name:
// it holds one owner's hosts.
func (s *Server) windowHosts(ctx context.Context) []mirror.HostView {
	if o := windowOwner(ctx); o != requestOwner(ctx) {
		return s.teamHostsOf(ctx, o)
	}
	return s.ext.Mirror.Hosts(ctx)
}

// teamOperatorOn reports whether r is a member's request on a TEAM host's
// entry made by an operator or a manager of that team (the request's own
// memberships): a page then draws the composer, the queue and the question
// cards' answers for typing (team sharing 2b.4). Drawing only — every typing
// route is gated again (mirrorTeamOperate), on a fresh read.
func teamOperatorOn(r *http.Request) bool {
	return onTeamHost(r) && !teamWatcherOf(r.Context(), mirror.TargetOwner(r.Context()))
}

// teamWatcherOf is THE ONE RULE for whether ctx's principal reads something
// owner owns — a team host session's question cards (handleMirrorAsks), a
// team's ask on the Asks list (withAskBoxes) — as a WATCHER, drawn with no
// answer: owner is not the principal's own, and the principal is not an
// operator or a manager of it (by the request's own memberships; a viewer,
// or — failing closed — no member at all). Drawing only: every write is
// gated again on its own route.
func teamWatcherOf(ctx context.Context, owner int64) bool {
	if owner <= 0 || owner == requestOwner(ctx) {
		return false
	}
	p, ok := authz.From(ctx)
	if !ok {
		return true
	}
	role, in := p.TeamScope().RoleIn(owner)
	return !in || !role.AtLeast(authz.TeamOperator)
}

// TeamAskViewerNote is what a team's ask on the Asks list says, in place of
// its answer controls, to a VIEWER of the team (withAskBoxes).
const TeamAskViewerNote = "This is your team's question and you are a viewer of the team: an operator or a manager answers it."

// onTeamHost reports whether r is a member's request on a TEAM host's entry
// (mirrorTeamGate aimed it there): the pages then leave out every extra that
// is the viewer's own — their recents, tags, pins, names, drafts, chief,
// proposals, suggestions and queue — and draw the composer disabled.
func onTeamHost(r *http.Request) bool {
	t := mirror.TargetOwner(r.Context())
	return t > 0 && t != requestOwner(r.Context())
}
