package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// TEAM AGENTS, VISIBLE TO THE TEAM (team sharing phase 4, slice 4.4;
// docs/design/team-sharing.md). Since migration 0131 an agent a team owns is
// readable by the team's members through a door where a person is present
// (authz.Principal.TeamScope: the browser session, a paired phone). Until then
// every agent a person could see was their own, and the agent routes acted on
// "visible" as if it meant "mine". This file is what makes that no longer
// true safe:
//
//   - teamAgentRoutes: what a member may do on a team agent's routes, decided
//     once, in admit, for every route that names an agent;
//   - turnActingContext: a turn a member starts on a team agent runs as the
//     TEAM ACTING PRINCIPAL (its secrets, its tools' rows, its usage), after a
//     FRESH read of the member's role — so a removed member's queued message
//     is dropped, not sent;
//   - teamGatewayToolless: a team-owned gateway agent's turn carries no tools,
//     from every chat door (the answer-delivery door included);
//   - teamTurnHeader: the in-memory "typed by <name> (<role>)" line, so the
//     team chief knows who is speaking in its ONE SHARED ROOM.
// ---------------------------------------------------------------------------

// teamAgentRule is what a team member may do through one agent route, on an
// agent their team owns.
type teamAgentRule int

const (
	// teamAgentRead: any member (viewer and up) — row-level security decides
	// who sees the agent at all. Also every machine door (an API or hook
	// token, an agent's own), which carries no team memberships and so never
	// sees a team agent.
	teamAgentRead teamAgentRule = iota
	// teamAgentChat: a message to the agent — operator and up (plan §6: a
	// viewer reads the chat, tool output included, and cannot type).
	teamAgentChat
	// teamAgentRefused: no member, at any role, in slice 4.4. Lifecycle and
	// settings (start, stop, delete, model, name) are a manager's door that
	// acts AS the team (slice 4.5's ViaTeamAdmin): run under a member's
	// principal they would pick the member's credentials. New threads are
	// refused because a team chief has ONE shared room (operator decision 4).
	// The terminal is a shell in a pod holding the team's credentials and
	// needs a fresh step-up (slice 4.7).
	teamAgentRefused
)

// teamAgentRoutes is EVERY route whose pattern names an agent ("agents/{"),
// with its rule. TestEveryAgentRouteHasATeamRule fails when routes.golden
// gains or loses one that is not here, so a new agent route is a decision,
// not a default.
var teamAgentRoutes = map[string]teamAgentRule{
	"GET /agents/{name}":                         teamAgentRead,
	"GET /agents/{name}/logs/stream":             teamAgentRead,
	"GET /agents/{name}/send":                    teamAgentRead, // the redirect back after a send
	"GET /agents/{name}/terminal":                teamAgentRead, // the page; its shell is the /ws below
	"GET /ui/agents/{id}/header":                 teamAgentRead,
	"GET /ui/agents/{id}/status":                 teamAgentRead,
	"GET /ui/agents/{name}/chat-context":         teamAgentRead,
	"GET /ui/agents/{name}/chat-log":             teamAgentRead,
	"GET /ui/agents/{name}/task":                 teamAgentRead,
	"GET /api/agents/{name}/account-failovers":   teamAgentRead, // machine door
	"GET /api/agents/{name}/messages":            teamAgentRead, // machine door
	"POST /api/agents/{name}/messages":           teamAgentRead, // machine door
	"DELETE /api/agents/{name}":                  teamAgentRead, // machine door
	"GET /agents/{name}/ws":                      teamAgentChat,
	"POST /agents/{name}/send":                   teamAgentChat,
	"GET /agents/{name}/terminal/ws":             teamAgentRefused,
	"POST /agents/{id}/start":                    teamAgentRefused,
	"POST /agents/{id}/stop":                     teamAgentRefused,
	"DELETE /agents/{id}":                        teamAgentRefused,
	"POST /agents/{id}/model":                    teamAgentRefused,
	"POST /agents/{id}/name":                     teamAgentRefused,
	"POST /agents/{id}/sessions":                 teamAgentRefused,
	"POST /agents/{id}/sessions/side":            teamAgentRefused,
	"POST /agents/{id}/sessions/fresh-primary":   teamAgentRefused,
	"POST /agents/{id}/sessions/{sid}/default":   teamAgentRefused,
	"POST /agents/{id}/sessions/{sid}/archive":   teamAgentRefused,
	"POST /agents/{id}/sessions/{sid}/unarchive": teamAgentRefused,
}

// TeamAgentViewerRefusal is what a team viewer is answered on a chat route.
const TeamAgentViewerRefusal = "this is a team agent: you can read its chat; writing to it needs a team operator or above"

// TeamAgentRouteRefusal is what any member is answered on a route a team agent
// does not take from a member in this release.
const TeamAgentRouteRefusal = "this is a team agent: a member cannot do that to it here (its lifecycle and settings are its managers', " +
	"its terminal needs a fresh sign-in, and a team chief has one shared conversation)"

// refuseTeamAgentRoute answers 403 and reports true when r names an agent its
// principal's TEAM owns and the route's rule refuses the principal's role in
// it. Called by admit, after the principal is attached: one place for every
// agent route (teamAgentRoutes).
//
// It costs nothing for a principal with no team memberships on its door (no
// read): such a principal cannot see a team agent. An agent the principal
// cannot see, or does not exist, passes to the handler (its own 404, no
// oracle); any other read error is a 500 — the gate cannot decide, so it
// does not let the request through.
func (s *Server) refuseTeamAgentRoute(w http.ResponseWriter, r *http.Request) bool {
	rule, ok := teamAgentRoutes[r.Pattern]
	if !ok || rule == teamAgentRead || s.ext.Agents == nil {
		return false
	}
	ctx := r.Context()
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.TeamScope() == "" {
		return false
	}
	var a agents.Agent
	var err error
	if name := r.PathValue("name"); name != "" {
		a, err = s.ext.Agents.GetByName(ctx, name)
	} else if id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64); perr == nil {
		a, err = s.ext.Agents.Get(ctx, id)
	} else {
		return false
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false
	case err != nil:
		s.logger.Printf("team agents: %s: read the agent: %v", r.Pattern, err)
		http.Error(w, "could not load the agent", http.StatusInternalServerError)
		return true
	}
	if a.OwnerID <= 0 || a.OwnerID == p.UserID {
		return false
	}
	role, in := p.TeamScope().RoleIn(a.OwnerID)
	if !in {
		return false // not a team agent of theirs: row-level security decided
	}
	if rule == teamAgentChat && role.AtLeast(authz.TeamOperator) {
		return false
	}
	msg := TeamAgentRouteRefusal
	if rule == teamAgentChat {
		msg = TeamAgentViewerRefusal
	}
	http.Error(w, msg, http.StatusForbidden)
	return true
}

// ErrNoLongerTeamOperator is a turn a member started on a team agent that is
// not sent because, when it came to be sent, the member was no longer an
// operator (or above) of the team: removed, or downgraded to viewer.
var ErrNoLongerTeamOperator = errors.New("not sent: you are no longer an operator of the team that owns this agent")

// turnActingContext is ctx for one turn of agent a. A turn a PERSON starts on
// an agent they do not own — under row-level security that is only a team's
// agent — runs as the TEAM ACTING PRINCIPAL (agentPrincipal through
// authz.ViaAgent: the agent acting as its owner), so its credentials
// (agents.Store.Secrets, owner-only), its usage row, its tools' rows and its
// stored reply are the team's, never the member's. Every other ctx (the
// owner's own, the team acting principal of a background door, System) is
// returned as is.
//
// 🔴 THE MEMBER'S ROLE IS READ AGAIN HERE, FRESH, ON EVERY ATTEMPT. chatTurn is
// called once per send attempt (chatTurnWhenFree), so a message queued behind
// a busy turn is re-checked when its turn comes: a member removed from the
// team — or made a viewer — while it waited gets ErrNoLongerTeamOperator and
// the message is not sent (plan §6). An unreadable account fails closed.
func (s *Server) turnActingContext(ctx context.Context, a agents.Agent) (context.Context, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.Team || a.OwnerID <= 0 || p.UserID == a.OwnerID {
		return ctx, nil
	}
	acct, err := s.accountOfChecked(ctx, p.UserID)
	if err != nil {
		return ctx, fmt.Errorf("not sent: your team role could not be read just now: %w", err)
	}
	role, in := acct.Teams.RoleIn(a.OwnerID)
	if !in || !role.AtLeast(authz.TeamOperator) {
		return ctx, ErrNoLongerTeamOperator
	}
	tp, ok := s.agentPrincipal(ctx, a, authz.ViaAgent)
	if !ok || !tp.Team || tp.RoleUncertain {
		return ctx, fmt.Errorf("not sent: agent %q is not a team's (owner %d)", a.Name, a.OwnerID)
	}
	return authz.WithPrincipal(ctx, tp), nil
}

// teamGatewayToolless reports whether a's turn must go out WITHOUT tools: a
// GATEWAY-kind agent a team owns (plan §3.1: gateway kind is not a team agent
// kind in phase 4). Its in-process tools would run under whatever principal
// the door brought — the team acting principal through the attention door
// (deliverAnswer → runDeliveredTurn → chatTurn), which turnPrincipal's team
// refusal never sees. An owner that cannot be read is treated as a team: a
// toolless turn is the safe side.
func (s *Server) teamGatewayToolless(ctx context.Context, a agents.Agent) bool {
	if agents.ResolveKind(a.Kind) != agents.KindGateway || a.OwnerID <= 0 {
		return false
	}
	if p, ok := authz.From(ctx); ok && !p.All() && p.UserID == a.OwnerID {
		return p.Team
	}
	acct, err := s.accountOfChecked(ctx, a.OwnerID)
	if err != nil {
		s.logger.Printf("agents: %s: read its owner %d: %v (its turn goes out without tools)", a.Name, a.OwnerID, err)
		return true
	}
	return acct.Team
}

// personNamer is the users store's one-row read of a person (users.PGStore.ByID).
type personNamer interface {
	ByID(ctx context.Context, id int64) (users.User, error)
}

// teamTurnHeader is the muster line a team agent's runtime is handed before
// what a member typed — "[muster] typed by <name> (<role>) in team <team>" —
// or "" when the turn is not a member's on a team agent. Rendered in memory
// only, as viewChiefTurnText's header is: the stored row is what was typed.
// The role is the one the member typed with (their request's memberships).
func (s *Server) teamTurnHeader(ctx context.Context, a agents.Agent, typedBy int64) string {
	if typedBy <= 0 || a.OwnerID <= 0 || typedBy == a.OwnerID {
		return ""
	}
	p, ok := authz.From(ctx)
	if !ok {
		return ""
	}
	role, in := p.TeamScope().RoleIn(a.OwnerID)
	if !in {
		return ""
	}
	name := "user " + strconv.FormatInt(typedBy, 10)
	if pn, ok := s.ext.Users.(personNamer); ok {
		if u, err := pn.ByID(ctx, typedBy); err == nil && strings.TrimSpace(u.DisplayName) != "" {
			name = u.DisplayName
		}
	}
	one := func(v string) string { return fence.Sanitize(strings.Join(strings.Fields(v), " ")) }
	team := s.teamNames(ctx, []int64{a.OwnerID})[a.OwnerID]
	return "[muster] typed by " + one(name) + " (" + string(role) + ") in team " + one(team) + "\n\n"
}

// turnPanelOwner is whose notification panel a turn of a is shown in: the
// agent's owner — on a team agent the team, never the member who typed — or,
// for a row that carries no owner (a store that does not read the column),
// the request's.
func turnPanelOwner(ctx context.Context, a agents.Agent) int64 {
	if o := agentOwnerID(a); o > 0 {
		return o
	}
	return requestOwner(ctx)
}
