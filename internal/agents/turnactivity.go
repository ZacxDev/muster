package agents

import (
	"context"
	"io"
)

// A TURN'S BYTES ARE EVIDENCE THAT THE AGENT IS ALIVE.
//
// The outcome watchdog (internal/agentoutcome) must never call a working agent
// stuck. The only thing muster can SEE of a long turn in flight is its stream —
// text, thinking, tool calls and, from a claude-code runtime, every tool the CLI
// runs — and the turn's caller does not always read it: the kickoff passes no
// stream callback at all. So the transport reports activity itself, for every
// turn whose context asks: each read of the runtime's stream that returns bytes
// runs the callback WithTurnActivity put on ctx. The callback throttles (it runs
// per read).

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
