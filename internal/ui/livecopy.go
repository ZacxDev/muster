package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// A mirrored Claude Code window whose conversation ALSO runs elsewhere under
// another session id (mirror.SessionView.LiveCopyWarning, mirror/livecopy.go)
// says so on its card and at the top of the Rendered and Terminal views: a
// message sent from here never reaches the other copy, and that silent
// divergence is what the warning exists for.

// liveCopyLabel leads the warning wherever it is drawn.
const liveCopyLabel = "Running in two places."

// liveCopyCardLine is the card's warning line, nothing for "".
func liveCopyCardLine(warning string) g.Node {
	if warning == "" {
		return nil
	}
	return P(
		g.Attr("data-mirror-live-copy", ""),
		Class("mt-1 break-words rounded-lg bg-st-warning-bg px-3 py-2 text-xs text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"),
		Span(Class("font-semibold"), g.Text(liveCopyLabel)),
		g.Text(" "+warning),
	)
}

// liveCopyBanner is a view's warning, under its header; nothing for "".
func liveCopyBanner(id, warning string) g.Node {
	if warning == "" {
		return nil
	}
	return P(
		ID(id),
		g.Attr("data-live-copy", ""),
		Class("border-b border-line bg-st-warning-bg px-3 py-1.5 text-xs text-st-warning-fg break-words"),
		Span(Class("font-semibold"), g.Text(liveCopyLabel)),
		g.Text(" "),
		Span(g.Attr("data-live-copy-text", ""), g.Text(warning)),
	)
}
