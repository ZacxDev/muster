package agents

import (
	"context"
	"sync"
)

// A SEND FENCE: A CHECK THAT MUST STILL HOLD WHEN THE TEXT REACHES THE RUNTIME.
//
// Some turns may be sent only while a fact stays true — a team member's message
// to the team's agent only while they are still an operator of the team
// (internal/api team_agents.go turnActingContext). Checking that fact before the
// turn and then sending is a check-then-act: the fact can change in between, and
// the text arrives anyway. A fence closes the gap. Its function runs
// IMMEDIATELY before each request of the turn opens — after every other piece
// of the turn's preparation — and HOLDS what it checked (for the team check, a
// row lock every membership change must wait for) until the runtime's verdict
// on that request:
//
//   - ccd's 103 (the text was accepted; traceTurnAccepted releases it FIRST,
//     before any acceptance callback runs, so those callbacks' own database
//     writes never wait behind it), or
//   - the response's status line (a refusal, a 404, an answer), or
//   - the request failing.
//
// The text therefore reaches the runtime only inside the fence's hold, and a
// change the fence's lock serialises against is ordered either wholly before
// the check (and the request is refused) or wholly after the verdict.
//
// ONCE ACCEPTED, DISARMED: a turn whose first request was accepted (103 or a
// 200) runs each later round of its tool loop unfenced — those carry tool results,
// not the fenced text. A 404 (ErrResponsesUnsupported, before the chat
// completions fallback) or a refusal leaves it armed, so a retried or fallback
// request is checked again.
//
// ⚠ NOTHING HAPPENS WHILE IT HOLDS BUT THE REQUEST ITSELF: the hold is taken
// after the turn's own reads (its credential, its tools), so a hold that holds
// a database connection never waits for a second one.

// SendFence checks, immediately before a request of a turn opens, that the turn
// may still be sent. It returns a non-nil error to refuse (nothing is sent; the
// error is the turn's), or release, which the transport calls exactly once when
// the request has its verdict.
type SendFence func(ctx context.Context) (release func(), err error)

type sendFenceKey struct{}

// sendFenceState is one turn's fence: armed until a request is accepted.
type sendFenceState struct {
	fn SendFence

	mu       sync.Mutex
	disarmed bool
	open     func(accepted bool) // the current request's release, nil when none holds
}

// WithSendFence returns ctx whose turn has each request fenced by fn
// (until one is accepted).
func WithSendFence(ctx context.Context, fn SendFence) context.Context {
	return context.WithValue(ctx, sendFenceKey{}, &sendFenceState{fn: fn})
}

// fenceRequest runs ctx's fence, if any, for one request about to open. It
// returns done, which the caller calls when the request has its verdict
// (accepted: a 200 or ccd's 103) — idempotent, and a no-op for a ctx without a
// fence or with a disarmed one — or the fence's refusal.
func fenceRequest(ctx context.Context) (done func(accepted bool), err error) {
	st, ok := ctx.Value(sendFenceKey{}).(*sendFenceState)
	if !ok {
		return func(bool) {}, nil
	}
	st.mu.Lock()
	disarmed := st.disarmed
	st.mu.Unlock()
	if disarmed {
		return func(bool) {}, nil
	}
	release, err := st.fn(ctx)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	done = func(accepted bool) {
		once.Do(func() {
			st.mu.Lock()
			if accepted {
				st.disarmed = true
			}
			st.open = nil
			st.mu.Unlock()
			if release != nil {
				release()
			}
		})
	}
	st.mu.Lock()
	st.open = done
	st.mu.Unlock()
	return done, nil
}

// fenceAccepted releases ctx's fence's current hold as accepted (ccd's 103).
func fenceAccepted(ctx context.Context) {
	st, ok := ctx.Value(sendFenceKey{}).(*sendFenceState)
	if !ok {
		return
	}
	st.mu.Lock()
	done := st.open
	st.mu.Unlock()
	if done != nil {
		done(true)
	}
}
