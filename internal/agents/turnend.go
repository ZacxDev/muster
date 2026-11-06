package agents

import (
	"context"
	"sync/atomic"
)

// A TOOL THAT ENDS THE TURN IT IS CALLED IN.
//
// The chief's ask_owner (internal/api owner_ask.go) puts a question to its owner
// as a card in the chat; the owner's answer arrives as their NEXT message. A turn
// that went on after asking would answer its own question, or start the very
// work it was asking about. So the tool marks the turn as ended, and
// [RunToolLoop] honours the mark STRUCTURALLY: after the round of calls that set
// it, the outputs go back to the runtime once (its session must hold an output
// for every call it made) with tool_choice "none", and whatever that answer asks
// for, no further tool is run — the loop returns.
//
// ⚠ IT BINDS ONLY THE IN-PROCESS LOOP. A claude-code agent calls its tools over
// MCP from its own CLI, which muster does not drive call by call, so there the
// tool's result asks the model to stop — prose, not structure.

type turnEndKey struct{}

// WithTurnEnd returns ctx carrying a fresh, unset turn-end mark. Each turn
// starts its own: a mark never leaks from one turn to the next.
func WithTurnEnd(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnEndKey{}, new(atomic.Bool))
}

// EndTurn sets ctx's turn-end mark; a no-op for a ctx without one (an MCP call).
// It reports whether there was a mark to set.
func EndTurn(ctx context.Context) bool {
	if m, ok := ctx.Value(turnEndKey{}).(*atomic.Bool); ok {
		m.Store(true)
		return true
	}
	return false
}

// TurnEnded reports whether ctx's turn-end mark is set.
func TurnEnded(ctx context.Context) bool {
	m, ok := ctx.Value(turnEndKey{}).(*atomic.Bool)
	return ok && m.Load()
}
