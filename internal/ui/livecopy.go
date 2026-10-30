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

// MirrorOfflineText is what a mirrored window's own page says while its host
// is not serving it.
const MirrorOfflineText = "The host's muster-mirror is not connected, or the host no longer lists this tmux session."

// mirrorHostBanner is the line under a mirrored window's page header saying
// what the retired Agents list's card said about it — its "offline" chip and
// the host's note (mirror.SessionView.AgentNote: what its view's credential
// needs, an opencode token about to expire). Server-drawn with the page, in
// the column above the transcript, so it moves nothing in after paint.
// Nothing when the host is online and has no note.
func mirrorHostBanner(id string, offline bool, note string) g.Node {
	if !offline && note == "" {
		return nil
	}
	return Div(
		ID(id),
		Class("flex flex-col gap-0.5 border-b border-line bg-s1 px-3 py-1.5 text-xs text-muted break-words"),
		g.If(offline, P(g.Attr("data-mirror-offline", ""),
			Span(Class("font-semibold text-fg2"), g.Text("Offline. ")), g.Text(MirrorOfflineText))),
		g.If(note != "", P(g.Attr("data-mirror-note", ""), g.Text(note))),
	)
}
