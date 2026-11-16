package agents

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
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
// row lock every membership change must wait for) until the FIRST of:
//
//   - ccd's 103 (the text was accepted; traceTurnAccepted releases it FIRST,
//     before any acceptance callback runs, so those callbacks' own database
//     writes never wait behind it);
//   - the response's status line (a refusal, a 404, an answer);
//   - the request failing;
//   - SendFenceHoldCap after the request (its body included) was fully
//     WRITTEN (httptrace WroteRequest), so a slow connect spends none of it;
//   - SendFenceHoldMax after the hold was TAKEN, whatever the request is
//     doing — the hard bound, which a connect or TLS handshake that never
//     finishes (a blackholed runtime) cannot outlast.
//
// 🔴 THE CAP AND THE HARD BOUND (operator decision, #731 rounds 0 and 1).
// Without them a membership change waited behind any member turn in flight
// and, on a runtime whose 103 does not come (a ccd that predates it; the
// chat-completions fallback, whose status line comes only after the turn) or
// a runtime it cannot reach, failed at db.StatementTimeout. With them the
// change waits at most SendFenceHoldMax — under the statement timeout.
// ⚠ THE ACCEPTED RESIDUAL: on ANY runtime whose 103 does not arrive within
// the cap — one that never sends it, and also a ccd that does but takes
// longer (its submit window runs to 20s, with an extra Enter at 10s) — a
// message transmitted before a removal can still be delivered after it. Only
// a 103 (or a refusal) inside the cap settles the text inside the hold.
//
// ONCE ACCEPTED, DISARMED: a turn whose first request was accepted (103 or a
// 200) runs each later round of its tool loop unfenced — those carry tool
// results, not the fenced text. A 404 (ErrResponsesUnsupported, before the chat
// completions fallback) or a refusal leaves it armed, so a retried or fallback
// request is checked again. A cap release does not disarm by itself: the
// request's own verdict still does.
//
// ⚠ NOTHING HAPPENS WHILE IT HOLDS BUT THE REQUEST ITSELF: the hold is taken
// after the turn's own reads (its credential, its tools), so a hold that holds
// a database connection never waits for a second one.

// SendFenceHoldCap bounds a send fence's hold after the request was written.
const SendFenceHoldCap = 5 * time.Second

// SendFenceHoldMax bounds a send fence's hold from the moment it was taken,
// connect and TLS included. 🔴 UNDER db.StatementTimeout (10s) WITH HEADROOM,
// and at least the cap, so a membership change waiting on a hold finishes
// rather than timing out: TestTheSendFenceBoundsAreUnderTheStatementTimeout
// (internal/api) pins both relations.
const SendFenceHoldMax = 8 * time.Second

// sendFenceHoldCap and sendFenceHoldMax are the bounds in force; a test of
// this package shortens them.
var (
	sendFenceHoldCap = SendFenceHoldCap
	sendFenceHoldMax = SendFenceHoldMax
)

// SendFence checks, immediately before a request of a turn opens, that the turn
// may still be sent. It returns a non-nil error to refuse (nothing is sent; the
// error is the turn's), or release, which the transport calls exactly once
// (the first of the verdicts above, the cap, or the hard bound).
type SendFence func(ctx context.Context) (release func(), err error)

type sendFenceKey struct{}

// sendFenceState is one turn's fence: armed until a request is accepted.
type sendFenceState struct {
	fn SendFence

	mu       sync.Mutex
	disarmed bool
	open     func(accepted bool) // the current request's done, nil when none holds
}

// WithSendFence returns ctx whose turn has each request fenced by fn
// (until one is accepted).
func WithSendFence(ctx context.Context, fn SendFence) context.Context {
	return context.WithValue(ctx, sendFenceKey{}, &sendFenceState{fn: fn})
}

// fenceRequest runs req's context's fence, if any, for req about to open. It
// returns req carrying the cap's trace (WroteRequest starts it) and done,
// which the caller calls when the request has its verdict (accepted: a 200 or
// ccd's 103) — idempotent, it also stops both timers — or the fence's
// refusal. For a context without a fence, or with a disarmed one, req is
// returned as is and done is a no-op.
func fenceRequest(req *http.Request) (*http.Request, func(accepted bool), error) {
	ctx := req.Context()
	st, ok := ctx.Value(sendFenceKey{}).(*sendFenceState)
	if !ok {
		return req, func(bool) {}, nil
	}
	st.mu.Lock()
	disarmed := st.disarmed
	st.mu.Unlock()
	if disarmed {
		return req, func(bool) {}, nil
	}
	release, err := st.fn(ctx)
	if err != nil {
		return nil, nil, err
	}
	h := &fenceHold{release: release}
	h.startMax(sendFenceHoldMax) // from the moment the hold was taken
	done := func(accepted bool) {
		if accepted {
			st.mu.Lock()
			st.disarmed = true
			st.mu.Unlock()
		}
		h.end()
		st.mu.Lock()
		st.open = nil
		st.mu.Unlock()
	}
	st.mu.Lock()
	st.open = done
	st.mu.Unlock()
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { h.startCap(sendFenceHoldCap) },
	}))
	return req, done, nil
}

// fenceHold is one request's hold: released exactly once, by its verdict, its
// cap or its hard bound, whichever is first.
type fenceHold struct {
	mu       sync.Mutex
	ended    bool
	timer    *time.Timer // the cap's, armed at WroteRequest
	maxTimer *time.Timer // the hard bound's, armed when the hold is taken
	release  func()
}

// startCap arms the cap, once, unless the hold already ended.
func (h *fenceHold) startCap(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended || h.timer != nil {
		return
	}
	h.timer = time.AfterFunc(d, h.end)
}

// startMax arms the hard bound; called once, before anything can end the hold.
func (h *fenceHold) startMax(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maxTimer = time.AfterFunc(d, h.end)
}

// end releases the hold and stops both timers; idempotent.
func (h *fenceHold) end() {
	h.mu.Lock()
	if h.ended {
		h.mu.Unlock()
		return
	}
	h.ended = true
	if h.timer != nil {
		h.timer.Stop()
	}
	if h.maxTimer != nil {
		h.maxTimer.Stop()
	}
	release := h.release
	h.mu.Unlock()
	if release != nil {
		release()
	}
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
