package agents

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync"
)

// THE RUNTIME ACCEPTED THE TURN'S TEXT — a signal a caller can hang work on.
//
// A turn's caller cannot tell from its own side when the runtime TOOK the text:
// ccd answers /v1/responses only when the turn is over, so the final status
// comes after every tool call the turn made. muster needs the earlier moment for
// one thing — the owner's turn marker (internal/api chief_owner_release.go),
// which must exist while the turn runs and never for text the runtime refused.
//
// Two signals mark it, whichever comes first, at most once per turn:
//
//   - ccd's informational 103 carrying HeaderCCPromptAccepted: "1", written the
//     moment the CLI reported a UserPromptSubmit for the very text ccd pasted.
//     Before that ccd can still refuse (409 busy, 503 not_ready) and the text
//     never reached the model. A ccd that predates the header never sends it,
//     so its turns never mark — fail closed.
//   - a 200 on the turn's first request: a streaming runtime (the gateway kind)
//     answers 200 only once it is running the turn. On ccd the 200 is the end
//     of the turn, so marking there is harmless — the caller clears its marker
//     as soon as the call returns.
//
// A refusal (any non-200, non-103 answer) never marks.

// HeaderCCPromptAccepted, "1" on ccd's 103 informational response, says the
// CLI has accepted the pasted prompt (its UserPromptSubmit matched it). "1" on
// the REQUEST asks for that 103; ccd sends none to a caller that did not ask.
// cmd/ccd spells it itself (headerCCPromptAccepted);
// TestThePromptAcceptedHeaderIsMusters pins the two.
const HeaderCCPromptAccepted = "X-Muster-CC-Prompt-Accepted"

type turnAcceptedKey struct{}

// WithTurnAccepted returns ctx carrying fn, run AT MOST ONCE when the runtime
// accepts the turn's text (MarkTurnAccepted). fn may run on a transport
// goroutine.
func WithTurnAccepted(ctx context.Context, fn func()) context.Context {
	var once sync.Once
	return context.WithValue(ctx, turnAcceptedKey{}, func() { once.Do(fn) })
}

// MarkTurnAccepted runs ctx's WithTurnAccepted callback, if any. The runtime
// transports call it on the signals above; a test's fake runtime calls it to
// play a runtime that took the text.
func MarkTurnAccepted(ctx context.Context) {
	if fn, ok := ctx.Value(turnAcceptedKey{}).(func()); ok {
		fn()
	}
}

// wantsTurnAccepted reports whether ctx carries a WithTurnAccepted callback:
// then the request asks ccd for its 103 (HeaderCCPromptAccepted: "1" on the
// request), which ccd sends only when asked.
func wantsTurnAccepted(ctx context.Context) bool {
	_, ok := ctx.Value(turnAcceptedKey{}).(func())
	return ok
}

// traceTurnAccepted returns ctx with a client trace that marks the turn
// accepted on ccd's 103, when ctx carries a callback; else ctx unchanged.
func traceTurnAccepted(ctx context.Context) context.Context {
	if !wantsTurnAccepted(ctx) {
		return ctx
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints && h.Get(HeaderCCPromptAccepted) == "1" {
				MarkTurnAccepted(ctx)
			}
			return nil
		},
	})
}
