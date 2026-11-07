package agents

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync"
)

// THE RUNTIME ACCEPTED THE TURN'S TEXT — and IN WHICH LANE.
//
// A turn's caller cannot tell from its own side when the runtime TOOK the text:
// ccd answers /v1/responses only when the turn is over, after every tool call
// the turn made. muster needs the earlier moment for one thing — the owner's
// turn marker (internal/api chief_owner_release.go), which must exist while the
// turn runs, must never exist for text the runtime refused, and must name the
// ccd LANE (the CLI) the turn runs in.
//
// The one signal: ccd's informational 103 carrying HeaderCCPromptAccepted: "1",
// written the moment the CLI reported a UserPromptSubmit for the very text ccd
// pasted, and only when the request asked for it (the same header, "1", on the
// request — wantsTurnAccepted). Before that ccd can still refuse (409 busy, 503
// not_ready) and the text never reached the model. The lane is the request's
// own HeaderCCSession ("" = ccd's primary CLI). A runtime that is not ccd, or a
// ccd that predates the header, never sends it, so its turns never mark — fail
// closed.
//
// ⚠ NON-FATAL BY CONSTRUCTION. The trace hook always returns nil, so a 103 —
// expected or not, well-formed or not — can never fail the turn: Go's client
// reads past any 1xx to the final answer either way. A proxy that dropped the
// 103 would cost only the marker (owner_asked refuses), never the chat.

// HeaderCCPromptAccepted, "1" on ccd's 103 informational response, says the
// CLI has accepted the pasted prompt (its UserPromptSubmit matched it). "1" on
// the REQUEST asks for that 103; ccd sends none to a caller that did not ask.
// cmd/ccd spells it itself (headerCCPromptAccepted);
// TestThePromptAcceptedHeaderIsMusters pins the two.
const HeaderCCPromptAccepted = "X-Muster-CC-Prompt-Accepted"

// TurnAcceptance is what MarkTurnAccepted reports: the ccd lane the accepted
// text runs in — the request's HeaderCCSession, "" for ccd's primary CLI.
type TurnAcceptance struct {
	Lane string
	// NewConversation is the session ccd's 103 names in
	// HeaderCCNewConversationSession: the NEW conversation a turn that asked
	// for one (HeaderCCNewConversation) was typed into. "" when the turn did
	// not ask, or when its ccd predates the header.
	NewConversation string
}

type turnAcceptedKey struct{}

// WithTurnAccepted returns ctx carrying fn, run AT MOST ONCE when ccd accepts
// the turn's text (MarkTurnAccepted). fn may run on a transport goroutine.
func WithTurnAccepted(ctx context.Context, fn func(TurnAcceptance)) context.Context {
	var once sync.Once
	return context.WithValue(ctx, turnAcceptedKey{}, func(a TurnAcceptance) { once.Do(func() { fn(a) }) })
}

// MarkTurnAccepted runs ctx's WithTurnAccepted callback, if any. The transport
// calls it on ccd's 103; a test's fake runtime calls it to play a ccd that took
// the text in lane a.Lane.
func MarkTurnAccepted(ctx context.Context, a TurnAcceptance) {
	if fn, ok := ctx.Value(turnAcceptedKey{}).(func(TurnAcceptance)); ok {
		fn(a)
	}
}

// wantsTurnAccepted reports whether ctx carries a WithTurnAccepted callback:
// then the request asks ccd for its 103.
func wantsTurnAccepted(ctx context.Context) bool {
	_, ok := ctx.Value(turnAcceptedKey{}).(func(TurnAcceptance))
	return ok
}

// traceTurnAccepted returns req carrying a client trace that marks the turn
// accepted, in req's own lane, on ccd's 103 — when req's context carries a
// callback; else req unchanged. Call it after every header is set.
func traceTurnAccepted(req *http.Request) *http.Request {
	ctx := req.Context()
	if !wantsTurnAccepted(ctx) {
		return req
	}
	req.Header.Set(HeaderCCPromptAccepted, "1")
	lane := req.Header.Get(HeaderCCSession)
	return req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints && h.Get(HeaderCCPromptAccepted) == "1" {
				MarkTurnAccepted(ctx, TurnAcceptance{Lane: lane, NewConversation: h.Get(HeaderCCNewConversationSession)})
			}
			return nil
		},
	}))
}
