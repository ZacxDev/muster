package agents

import (
	"context"
	"net/http"
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
	// CLI with the chief's full tool set — restart the chief on a current image.
	HeaderCCToolless = "X-Muster-CC-Toolless"
)

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
	for _, name := range []string{HeaderCCSession, HeaderCCModel, HeaderCCWorkdir, HeaderCCEphemeral, HeaderCCToolless} {
		if v := r.Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
}
