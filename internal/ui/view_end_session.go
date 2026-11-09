package ui

import (
	"bytes"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ENDING A SESSION ON ITS VIEW (docs/design/chief-in-agent-view.md,
// "End-session"): the chief's end_session proposal, drawn in the view's
// proposal region (#mv-succession, which shows the session's newest proposal
// of either kind) — THE proposal card while it is open (view_proposal.go),
// then a line while it ends and once it ended.

// End-session states beyond the shared ones (SuccessionProposed, …Expired,
// …Declined, …Failed).
const (
	EndEnding = "ending"
	EndEnded  = "ended"
)

// MirrorProposalReadPath is a proposal's read by id (JSON: its state and its
// card), for a view whose window is gone.
const MirrorProposalReadPath = "/mirror/view/proposal"

// EndView is one end_session proposal as a view draws it.
type EndView struct {
	ID      int64
	State   string
	Runtime string
	Detail  string
	// Card is the proposal card while it is open.
	Card *ProposalView
}

// EndSessionCard is end_session x's card or line on the view at ref; nil when
// there is nothing to draw (declined).
func EndSessionCard(ref MirrorRef, x *EndView) g.Node {
	if x == nil || x.State == SuccessionDeclined {
		return nil
	}
	attrs := []g.Node{g.Attr("data-succession", strconv.FormatInt(x.ID, 10)), g.Attr("data-succession-state", x.State),
		g.Attr("data-proposal-kind", "end_session")}
	line := func(cls string, kids ...g.Node) g.Node {
		return Div(append(append(attrs, ID("mv-end-session"), Class("border-b border-line px-3 py-2 text-xs break-words "+cls)), kids...)...)
	}
	switch x.State {
	case SuccessionProposed:
		if x.Card == nil {
			return nil
		}
		return ProposalCard(ref, *x.Card, attrs...)
	case SuccessionExpired:
		return line("bg-s1 text-muted", g.Text("Your chief's proposal to end this session expired: ask it again."))
	case EndEnding:
		return line("bg-s1 text-fg2", Span(Class("animate-pulse"), g.Text("Ending this session… ")),
			Span(Class("text-muted"), g.Text("muster is ending it: its window closes within 30 s.")))
	case EndEnded:
		return line("bg-st-warning-bg text-st-warning-fg", Span(Class("font-semibold"), g.Text("Session ended. ")), g.Text(x.Detail))
	case SuccessionFailed:
		return line("bg-danger/15 text-fg", g.Text("This session could not be ended: "+x.Detail))
	}
	return nil
}

// RenderEndSessionCard is EndSessionCard as HTML.
func RenderEndSessionCard(ref MirrorRef, x *EndView) (string, error) {
	n := EndSessionCard(ref, x)
	if n == nil {
		return "", nil
	}
	var b bytes.Buffer
	err := n.Render(&b)
	return b.String(), err
}
