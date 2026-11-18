package ui

import (
	"slices"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE MESSAGES CLAUDE CODE HAS QUEUED ITSELF (internal/mirror/ccqueue.go): a
// message sent while Claude is busy — from here or typed in the terminal —
// waits in the CLI's input queue, and reaches the transcript only when Claude
// takes it. Until then the rendered view shows it here, under the
// conversation, labelled as queued in Claude Code: NOT a sent message, and not
// muster's own send queue (send_queue.go), which the composer draws.
//
// 🔴 ONE REGION, REDRAWN WHOLE from the host's whole list (a "patch" op), so a
// message leaves the moment Claude takes it and nothing drawn earlier can
// linger; the host sends the list on every view start and reconnect, drops
// what a CLI process that has since exited queued (cctranscript's
// Queue.DropBefore), and the page empties the region when the view ends or a
// snapshot is taken back (mirrorViewScript's clearQueued).
//
// 🔴 OUTSIDE THE CONVERSATION (#mv-log), so it never pushes the reading
// position: its appearing shrinks the log's box from below — a reader at the
// end stays at the end (the page's ResizeObserver), and one scrolled up keeps
// the same text at the top. Its height is bounded, and scrolls past that.

// ccQueuedID is the region's element id.
const ccQueuedID = "mv-ccq"

// CCQueued is the region with list in it — hidden, and empty, when list is
// empty. Each message is text (gomponents escapes it), the host's redacted,
// bounded copy (mirror.NormalizeQueued).
func CCQueued(list []string) g.Node {
	if len(list) == 0 {
		return Div(ID(ccQueuedID), g.Attr("data-cc-queued", "0"), g.Attr("hidden", ""))
	}
	items := make([]g.Node, 0, len(list))
	for _, s := range list {
		items = append(items, Li(
			g.Attr("data-cc-queued-item", ""),
			Class("max-w-[85%] self-end rounded-2xl border border-dashed border-line bg-bg/50 px-3 py-1.5 text-fg2"),
			P(Class("line-clamp-2 text-xs [overflow-wrap:anywhere]"), g.Text(s)),
		))
	}
	label := "Queued in Claude Code"
	if len(list) > 1 {
		label += " · " + strconv.Itoa(len(list))
	}
	return Section(
		ID(ccQueuedID),
		g.Attr("data-cc-queued", strconv.Itoa(len(list))),
		g.Attr("aria-label", "Messages queued in Claude Code"),
		Class("max-h-40 shrink-0 overflow-y-auto border-t border-line px-3 py-2"),
		P(Class("mb-1 text-right text-[11px] text-muted"), g.Text(label)),
		Ul(Class("flex flex-col gap-1.5"), g.Group(items)),
	)
}

// QueuedOps is the host's queued-message list (mirror.Msg.Queued) as the
// page's DOM operation: the region redrawn ("patch") when the list differs
// from the one this socket drew last — and always the first time, so a list
// an earlier socket or a snapshot drew is replaced.
func (t *MirrorTranscript) QueuedOps(list []string) []MirrorOp {
	list = mirror.NormalizeQueued(list)
	if t.queuedDrawn && slices.Equal(t.queued, list) {
		return nil
	}
	t.queued, t.queuedDrawn = list, true
	return []MirrorOp{{Op: "patch", ID: ccQueuedID, HTML: render(CCQueued(list))}}
}
