package agents

import (
	"context"
	"io"
)

// A TURN'S BYTES ARE EVIDENCE THAT THE AGENT IS ALIVE.
//
// The outcome watchdog (internal/agentoutcome) must never call a working agent
// stuck. Where a runtime STREAMS a turn — text, thinking, tool calls — that
// stream is evidence of life, and the turn's caller does not always read it (the
// kickoff passes no stream callback at all). So the transport reports activity
// itself, for every turn whose context asks: each read of the runtime's stream
// that returns bytes runs the callback WithTurnActivity put on ctx. The callback
// throttles (it runs per read).
//
// ⚠ A CLAUDE-CODE RUNTIME DOES NOT STREAM A TURN IN FLIGHT: ccd writes its whole
// answer when the CLI's turn is over (cmd/ccd/server.go), so this hook sees
// nothing until then. That case is covered by the turn being OPEN — the
// watchdog counts an in-flight turn as activity (internal/api agent_watchdog.go)
// — not by this hook.

type turnActivityKey struct{}

// WithTurnActivity returns ctx carrying fn, run on every read of a turn's stream
// that returned bytes. fn may run on the transport's goroutine and must be cheap
// (throttle inside it).
func WithTurnActivity(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, turnActivityKey{}, fn)
}

// MarkTurnActivity runs ctx's WithTurnActivity callback, if any. The transport
// calls it per read; a test's fake runtime calls it to play a stream.
func MarkTurnActivity(ctx context.Context) {
	if fn, ok := ctx.Value(turnActivityKey{}).(func()); ok {
		fn()
	}
}

// activityBody wraps a turn's response body so each read with bytes marks
// activity on ctx; it is body itself when ctx asks for nothing.
func activityBody(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	fn, ok := ctx.Value(turnActivityKey{}).(func())
	if !ok {
		return body
	}
	return activityReader{body, fn}
}

type activityReader struct {
	io.ReadCloser
	fn func()
}

func (r activityReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.fn()
	}
	return n, err
}
