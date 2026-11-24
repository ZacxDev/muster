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
// the same text at the top. Its rows are bounded (queuedRowsBound) and scroll
// past that, and the list folds under its count line ("Queued in Claude Code ·
// 3"), closed staying closed for that window on that device across every
// patch and reload (ccQueuedKeepJS) — the send queue's treatment
// (send_queue.go), under a key of its own.

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
		Class("shrink-0 border-t border-line px-3"),
		Details(
			ID(ccQueuedFoldID),
			// Drawn open; the page closes it in the task that draws it when
			// the person closed this window's list (ccQueuedKeepJS).
			g.Attr("open", ""),
			Class("group/ccq"),
			Summary(
				Class("press flex min-h-[44px] cursor-pointer list-none items-center justify-end gap-1.5 rounded-lg text-[11px] text-muted transition hover:text-fg [&::-webkit-details-marker]:hidden"),
				g.Raw(`<svg class="h-3.5 w-3.5 shrink-0 transition-transform group-open/ccq:rotate-90" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7 5l5 5-5 5"/></svg>`),
				Span(g.Attr("data-cc-queued-head", ""), g.Text(label)),
			),
			Ul(ID(ccQueuedRowsID), Class("flex flex-col gap-1.5 pb-2 "+queuedRowsBound), g.Group(items)),
		),
	)
}

// ccQueuedFoldID is the region's fold, and ccQueuedRowsID its rows: the part
// that scrolls.
const (
	ccQueuedFoldID = "mv-ccq-fold"
	ccQueuedRowsID = "mv-ccq-rows"
)

// CCQueuedClosedKeyPrefix starts the localStorage key under which a window's
// list is kept closed on this device ("1"); the rest is the view's socket
// path (the window's address). Not the send queue's key
// (SendQueueClosedKeyPrefix): the two lists fold apart.
const CCQueuedClosedKeyPrefix = "muster.ccqueue.closed:"

// ccQueuedKeepJS defines ccqKeep() in the view's script (mirrorViewScript),
// called just BEFORE a patch replaces the region; it returns what to call just
// after, in the same task, so no frame is painted between: the list folded as
// the person left it, and its rows scrolled where they were (a patch replaces
// them, which would put them back at the top). The first list a socket hears
// is a patch too, so a folded list is never painted open on a load.
//
// The fold's state is stored from the toggle event (it does not bubble:
// taken in the capture phase), as the list stands when it is handled — not
// the event's — so the open of a patch that ccqKeep then closed stores
// closed.
var ccQueuedKeepJS = `
  function ccqKey() {
    var l = document.getElementById('mv-log');
    return '` + CCQueuedClosedKeyPrefix + `' + (l ? l.getAttribute('data-ws') || '' : '');
  }
  function ccqStore() { try { return window.localStorage; } catch (e) { return null; } }
  function ccqKeep() {
    var r = document.getElementById('` + ccQueuedRowsID + `');
    var top = r ? r.scrollTop : 0;
    return function () {
      var d = document.getElementById('` + ccQueuedFoldID + `'), s = ccqStore();
      try { if (d && s && s.getItem(ccqKey()) === '1') d.open = false; } catch (e) {}
      var nr = document.getElementById('` + ccQueuedRowsID + `');
      if (nr && top) nr.scrollTop = top;
    };
  }
  document.addEventListener('toggle', function (e) {
    var d = e.target, s = ccqStore();
    if (!d || d.id !== '` + ccQueuedFoldID + `' || !d.isConnected || !s) return;
    try { if (d.open) s.removeItem(ccqKey()); else s.setItem(ccqKey(), '1'); } catch (err) {}
  }, true);
`

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
