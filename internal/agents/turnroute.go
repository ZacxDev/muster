package agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// A TURN'S ROUTE INSIDE THE POD: which CLI, and which model, one turn goes to
// when an agent's ccd runs more than one (session_runtime.go).
//
// 🔴 THE ADDRESS DOES NOT CHANGE, ONLY THESE HEADERS DO. Every turn still goes to
// the agent's one endpoint, ccd's port: ccd is the only thing the pod's network
// policy lets muster reach. So a session's route is a property of the REQUEST, set
// by the one place that knows the session (internal/api's sessionRoute) and
// read by the two request builders here, rather than a parameter threaded
// through every turn function between them.
//
// The header names are a wire contract with cmd/ccd, which spells them itself;
// cmd/ccd's tests pin the two (TestTheLaneHeadersAreMustersHeaders,
// TestTheWorkdirHeaderIsMusters).
const (
	// HeaderCCSession is the session key of an EXPLICIT Claude Code session:
	// ccd runs it in a CLI of its own. Absent: ccd's original CLI, the one
	// every legacy session has always shared.
	HeaderCCSession = "X-Muster-CC-Session"
	// HeaderCCModel is the model that CLI is started with (`--model`).
	HeaderCCModel = "X-Muster-CC-Model"
	// HeaderCCWorkdir is the working directory that CLI is started in: a job's
	// own worktree ([JobWorktreeDir]). Absent: the agent's working directory,
	// as for every session before jobs had worktrees. ccd accepts only a
	// directory [IsJobWorktreeDir] admits, and only on a turn that also names
	// an explicit session (HeaderCCSession), whose CLI it then fixes in that
	// directory for the session's life.
	HeaderCCWorkdir = "X-Muster-CC-Workdir"
	// HeaderCCWorkdirUsed is ccd's ANSWER on an explicit session's turn: the
	// directory that session's CLI actually works in. A sha job's runner
	// compares it with the HeaderCCWorkdir it sent and refuses the turn's result
	// when it is missing or different — a ccd from before job worktrees ignores
	// HeaderCCWorkdir and would run the job in the agent's own checkout.
	HeaderCCWorkdirUsed = "X-Muster-CC-Workdir-Used"
	// HeaderCCEphemeral, "1", marks an explicit session (HeaderCCSession) as a
	// THROWAWAY check: ccd stops that session's CLI as soon as its turn is over,
	// and at its cap (CCD_MAX_SESSIONS) refuses it (FailureCapacity) rather
	// than stop another session's CLI to make room. Set only on a chief's stall
	// check (internal/api nudgeRoute). A ccd that predates it ignores it and
	// keeps the CLI like any other session's.
	HeaderCCEphemeral = "X-Muster-CC-Ephemeral"
	// HeaderCCToolless, "1", marks an explicit session (HeaderCCSession) as
	// TOOL-LESS: ccd starts that session's CLI with no built-in tool and no MCP
	// server (`--tools= --strict-mcp-config`), so its only possible effect is
	// its text reply. Set only on a chief's stall check, alongside
	// HeaderCCEphemeral (internal/api withRoute): the check's prompt quotes other
	// sessions' transcripts, and a CLI with tools could be steered by them. Fixed
	// for the session's life. A ccd that predates it IGNORES it and starts the
	// CLI with the chief's full tool set (docs/chief-nudges.md, "Rolling out").
	HeaderCCToolless = "X-Muster-CC-Toolless"
	// HeaderCCMCPSession, "1", asks ccd to start an explicit session's CLI
	// with an MCP config of its own, in which muster's server carries
	// HeaderCCSession (the session's key) on every request: so muster knows
	// which session an MCP call comes from. Set on an officer's CHAT lane only
	// (internal/api officerChatTurnContext); fixed for the session's life. A
	// ccd that predates it ignores it, and the lane's calls then read as the
	// primary's (no session).
	HeaderCCMCPSession = "X-Muster-CC-MCP-Session"
	// HeaderCCLaneSecret is a chat lane's secret, minted by muster and kept
	// with its session row: muster sends it to ccd on the lane's turns
	// (beside HeaderCCMCPSession), ccd puts it in the lane's MCP config, and
	// the lane's MCP calls carry it back — so a call naming a chat session
	// (HeaderCCSession) is that lane's only with it. Fixed for the lane's
	// life.
	HeaderCCLaneSecret = "X-Muster-CC-Lane-Secret"
	// HeaderCCPinned, "1", marks an explicit session (HeaderCCSession) as the
	// chief's PRIMARY chat (internal/api withRoute, chief_chats.go): ccd never
	// stops its CLI to make room for another session, and does not count it
	// against CCD_MAX_SESSIONS, so a delivery to the primary always gets its
	// CLI. One lane is pinned at a time: the latest turn that says so pins its
	// lane and unpins any other (a fresh primary's old one). A ccd that predates
	// it ignores it and treats the lane like any other.
	HeaderCCPinned = "X-Muster-CC-Pinned"
	// HeaderCCNewConversation, "1", on a turn of the agent's ORIGINAL CLI (no
	// HeaderCCSession): ccd starts a NEW conversation in that CLI before it
	// pastes the turn — the CLI's /clear, whose new session ccd records as the
	// one every later start resumes — when the current conversation has a
	// transcript; the old transcript is kept. Set only by the officer runtime,
	// on every turn of an instance of a fresh_session_per_item pool (internal/
	// api OfficerTurnContext). ccd ignores it on an explicit session's turn,
	// and a ccd that predates it ignores it everywhere: the turn then runs in
	// the conversation the CLI already has, and its answer carries no
	// HeaderCCNewConversationSession, which is how muster notices
	// (docs/officers.md, "One conversation per item").
	HeaderCCNewConversation = "X-Muster-CC-New-Conversation"
	// HeaderCCNewConversationSession is ccd's ANSWER to a turn carrying
	// HeaderCCNewConversation: the session id of the new conversation the turn
	// was typed into, on the 103 that says the CLI accepted it (turnaccepted.go
	// TurnAcceptance.NewConversation) and on the final answer. A ccd that
	// predates HeaderCCNewConversation never sends it, which is how muster
	// tells a stale image from a current one (internal/api
	// OfficerTurnContext).
	HeaderCCNewConversationSession = "X-Muster-CC-New-Conversation-Session"
	// HeaderOCSession is the session key of an OPENCODE session
	// (SessionRuntimeOpencode): ccd runs it in an opencode server of its own.
	// It is sent only on OpencodeResponsesPath, never on /v1/responses, so a
	// ccd that predates opencode lanes answers 404 and runs nothing
	// (ErrOpencodeUnsupported) — it never reaches a Claude CLI.
	HeaderOCSession = "X-Muster-OC-Session"
	// HeaderOCModel is that session's stored model ("openrouter/<id>"),
	// fixed for its life like HeaderCCModel.
	HeaderOCModel = "X-Muster-OC-Model"
)

// OpencodeResponsesPath is ccd's turn path for an opencode session. A
// DIFFERENT PATH, not only different headers: a ccd from before opencode
// lanes ignores headers it does not know and would run the turn in its
// primary Claude CLI (the pattern every X-Muster-CC-* header above
// documents); an unknown path is a 404 instead, which fails closed.
const OpencodeResponsesPath = "/v1/opencode/responses"

// ErrOpencodeUnsupported is the 404 a ccd that predates opencode lanes
// answers OpencodeResponsesPath with. 🔴 IT IS NOT ErrResponsesUnsupported:
// that one makes its callers retry the turn toolless over
// /v1/chat/completions (runTurnWithTools, agentkickoff), which on this path
// would hand an OpenRouter session's turn to the chief's Claude CLI.
var ErrOpencodeUnsupported = errors.New("the chief's image runs no opencode sessions (its ccd answered 404 to " +
	OpencodeResponsesPath + "): restart the chief on an image that has them; nothing was sent")

// opencodeRoute reports whether ctx's turn is an opencode session's.
func opencodeRoute(ctx context.Context) bool {
	r, ok := TurnRouteFrom(ctx)
	return ok && r.Runtime == SessionRuntimeOpencode
}

// IsOpencodeTurn reports whether ctx's turn is an opencode session's: a
// failure of such a turn says nothing about the chief's Claude account
// (internal/agentgateway noteFailure marks none).
func IsOpencodeTurn(ctx context.Context) bool { return opencodeRoute(ctx) }

// routedResponsesURL is url (an agent's /v1/responses) as ctx's turn sends
// it: OpencodeResponsesPath for an opencode session's turn, url otherwise.
// An opencode turn on a URL that is not a /v1/responses one is refused: no
// path it could take there is known to be opencode's.
func routedResponsesURL(ctx context.Context, url string) (string, error) {
	if !opencodeRoute(ctx) {
		return url, nil
	}
	if base, ok := strings.CutSuffix(url, "/v1/responses"); ok {
		return base + OpencodeResponsesPath, nil
	}
	return "", fmt.Errorf("an opencode session's turn needs the agent's /v1/responses URL, not %q; nothing was sent", url)
}

// laneOf is the ccd lane a request runs in: its explicit Claude Code
// session (HeaderCCSession), else its opencode session (HeaderOCSession),
// "" for ccd's primary CLI. ccd's /v1/clis names an opencode lane by the
// same key (agents.CLIState.Lane).
func laneOf(h http.Header) string {
	if k := h.Get(HeaderCCSession); k != "" {
		return k
	}
	return h.Get(HeaderOCSession)
}

// TurnLane is laneOf for a turn's route: the lane every call of the turn runs in.
func TurnLane(ctx context.Context) string {
	if r, ok := TurnRouteFrom(ctx); ok && r.Header != nil {
		return laneOf(r.Header)
	}
	return ""
}

// FailureCapacity is the `error.type` of ccd's typed 503 for a turn that names
// an explicit session (HeaderCCSession) whose CLI is not running while every
// CLI ccd may run at once (CCD_MAX_SESSIONS) is held by a turn. ccd answers it
// from lane selection, BEFORE it starts a CLI or pastes anything. cmd/ccd spells
// it itself (failCapacity); TestTheCapacityFailureIsMusters pins the two.
const FailureCapacity = "capacity"

// TurnRoute is what a turn carries about where in the pod it goes.
type TurnRoute struct {
	// Runtime is the session's runtime (SessionRuntime*); "" is the agent's own.
	Runtime string
	// Header is set on every request of the turn, after the transport's own.
	Header http.Header
	// OnResponse, when set, is handed the response headers of every request of
	// the turn that got an answer, whatever its status.
	OnResponse func(http.Header)
}

// observeTurnRoute hands resp's headers to the route's OnResponse, if any.
func observeTurnRoute(req *http.Request, resp *http.Response) {
	if r, ok := TurnRouteFrom(req.Context()); ok && r.OnResponse != nil && resp != nil {
		r.OnResponse(resp.Header.Clone())
	}
}

type turnRouteKey struct{}

// WithTurnRoute returns ctx carrying r for the turn run under it.
func WithTurnRoute(ctx context.Context, r TurnRoute) context.Context {
	return context.WithValue(ctx, turnRouteKey{}, r)
}

// TurnRouteFrom is the route ctx carries, if any.
func TurnRouteFrom(ctx context.Context) (TurnRoute, bool) {
	r, ok := ctx.Value(turnRouteKey{}).(TurnRoute)
	return r, ok
}

// applyTurnRoute sets the route's headers on req. Called by both request
// builders AFTER their own headers, and the route can set only its own names:
// a route is never a way to replace the bearer or the session key.
func applyTurnRoute(req *http.Request) {
	r, ok := TurnRouteFrom(req.Context())
	if !ok {
		return
	}
	for _, name := range []string{HeaderCCSession, HeaderCCModel, HeaderCCWorkdir, HeaderCCEphemeral, HeaderCCToolless, HeaderCCPinned, HeaderCCNewConversation,
		HeaderOCSession, HeaderOCModel} {
		if v := r.Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
}
