package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// AN AGENT'S OUTCOME, ON ITS PAGE AND ITS ROW (internal/agentoutcome;
// internal/api agent_outcome.go). A started agent that FAILED or went STUCK says
// so in three places, each carrying data-agent-outcome="<outcome>" so a test —
// or a reader — can find it:
//
//   - a badge on the agent page's strip, beside the status dot (outcomeBadge);
//   - a notice above the transcript with the reason and what to do (outcomeNotice);
//   - a marker on the sidebar row (navOutcome).
//
// A running or done agent draws nothing here: the status dot and the transcript
// already say it. A failed FIRST turn keeps its own "kickoff failed" badge and
// notice (they carry the resend remedy), so the failed badge is not drawn twice.
//
// ⚠ EVERY CLASS HERE IS ONE THE COMMITTED STYLESHEET ALREADY HOLDS (the kickoff
// and awaiting-grant notices' own), so these views add nothing for css-check to
// rebuild.

// outcomeFailed and outcomeStuck are the two outcomes the views draw
// (agentoutcome.Failed / Stuck; ui does not import that package).
const (
	outcomeFailed = "failed"
	outcomeStuck  = "stuck"
)

func outcomeShown(a AgentCardView) bool {
	return a.Outcome == outcomeStuck || a.Outcome == outcomeFailed && !a.KickoffFailed
}

// outcomeBadge is the strip's badge for a failed or stuck agent.
func outcomeBadge(a AgentCardView) g.Node {
	if !outcomeShown(a) {
		return nil
	}
	tone := "bg-st-error-bg text-st-error-fg ring-st-error-fg/40"
	glyph := glyphError
	if a.Outcome == outcomeStuck {
		tone = "bg-st-warning-bg text-st-warning-fg ring-st-warning-fg/40"
	}
	title := "This agent " + outcomeVerb(a.Outcome)
	if a.OutcomeReason != "" {
		title += ": " + a.OutcomeReason
	}
	return Span(
		g.Attr("data-agent-outcome", a.Outcome),
		g.Attr("title", title),
		g.Attr("aria-label", title),
		Class("inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-semibold ring-1 ring-inset "+tone),
		statusGlyph(glyph, "h-3 w-3", false),
		g.Text(a.Outcome),
	)
}

func outcomeVerb(outcome string) string {
	if outcome == outcomeStuck {
		return "looks stuck"
	}
	return "failed"
}

// outcomeNotice is the notice above the transcript: the reason, and the move.
func outcomeNotice(a AgentCardView) g.Node {
	if !outcomeShown(a) {
		return nil
	}
	head, move := "Failed: ", " Fix the cause (its model, its repository), then Start it again from ⋯ — or ask your chief to start a new one."
	tone := "bg-st-error-bg text-st-error-fg ring-st-error-fg/40"
	if a.Outcome == outcomeStuck {
		head, move = "Stuck: ", " It has shown no activity muster can see. Send it a message, or Stop and Start it from ⋯."
		tone = "bg-st-warning-bg text-st-warning-fg ring-st-warning-fg/40"
	}
	reason := a.OutcomeReason
	if reason == "" {
		reason = "no reason was recorded."
	}
	return P(
		g.Attr("data-agent-outcome-notice", a.Outcome),
		Class("break-words rounded-lg px-3 py-2 text-xs ring-1 ring-inset "+tone),
		Span(Class("font-semibold"), g.Text(head)),
		Span(g.Attr("data-agent-outcome-reason", ""), g.Text(reason)),
		g.Text(move),
	)
}

// navOutcome is the sidebar row's marker for a failed or stuck agent: the word,
// muted like "offline", the reason its tooltip.
func navOutcome(a *AgentCardView) g.Node {
	if a == nil || a.Outcome != outcomeStuck && a.Outcome != outcomeFailed {
		return nil
	}
	title := "This agent " + outcomeVerb(a.Outcome)
	if a.OutcomeReason != "" {
		title += ": " + a.OutcomeReason
	}
	return Span(
		g.Attr("data-nav-outcome", a.Outcome),
		g.Attr("title", title),
		Class("shrink-0 text-[11px] font-normal text-muted"),
		g.Text(a.Outcome),
	)
}
