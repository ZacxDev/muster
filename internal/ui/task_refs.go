package ui

import (
	"strconv"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/notes"
)

// PLUGIN TASK REFERENCES on the card and the task page
// (docs/plugins.md#task-references). A reference's upstream is canonical: the
// card says so, the page shows the upstream's own thread read-only, the
// writes people and agents queued for it ("sending…", "failed: …"), and how
// fresh the snapshot is. The task's own comments on a reference are muster's
// history only — they are not on the upstream.

// refStaleAfter is when a reference's snapshot is shown as stale.
const refStaleAfter = 15 * time.Minute

// taskRefChip is the board/detail chip marking a reference: "↗ <the
// upstream's status label>", a span (the card's stretched link owns clicks on
// the meta row; the link itself is on the task page).
func taskRefChip(n notes.Note) g.Node {
	if n.Ref == nil {
		return g.Text("")
	}
	label := n.Ref.Snapshot.StatusLabel
	if label == "" {
		label = n.Ref.Key
	}
	return Span(
		g.Attr("data-task-reference", n.Ref.Key),
		g.Attr("title", "Follows its upstream ("+n.Ref.Key+"): status and comments written here are sent there"),
		Class("inline-flex shrink-0 items-center gap-1 rounded-md bg-s2 px-1.5 py-0.5 text-xs text-muted"),
		g.Text("↗ "+label),
	)
}

// taskRefSection is the task page's upstream block: the link, the label, the
// sync time (stale past refStaleAfter), the upstream's comments, and the
// writes still on their way.
func taskRefSection(n notes.Note, writes []notes.RefWrite) g.Node {
	if n.Ref == nil {
		return g.Text("")
	}
	ref := n.Ref
	stale := time.Since(ref.SyncedAt) > refStaleAfter
	return Div(
		g.Attr("data-task-ref-section", ""),
		Class("flex flex-col gap-2 border-t border-line pt-3"),
		Div(
			Class("flex flex-wrap items-center gap-2 text-xs text-muted"),
			g.If(ref.Snapshot.URL != "", A(
				Href(ref.Snapshot.URL), Target("_blank"), Rel("noopener noreferrer"),
				Class("font-medium text-accent hover:underline"),
				g.Text("Open upstream ↗"),
			)),
			Span(Class("font-mono"), g.Text(ref.Key)),
			g.If(ref.Snapshot.StatusLabel != "", Span(g.Text(ref.Snapshot.StatusLabel))),
			Span(g.Attr("data-ref-synced", ""), g.Text("synced "), cardTime(ref.SyncedAt)),
			g.If(stale, Span(
				g.Attr("data-ref-stale", ""),
				g.Attr("title", "The upstream has not been read for a while; opening this page asked the plugin for a fresh copy"),
				Class("rounded-md bg-st-warning-bg px-1.5 py-0.5 text-xs text-st-warning-fg"),
				g.Text("stale"),
			)),
		),
		g.If(len(ref.Snapshot.Comments) > 0, Div(
			g.Attr("data-ref-comments", ""),
			Class("flex flex-col gap-2"),
			g.Map(ref.Snapshot.Comments, func(c notes.RefComment) g.Node {
				return Div(
					g.Attr("data-ref-comment", c.ID),
					Class("rounded-lg bg-bg/60 px-3 py-2 ring-1 ring-inset ring-line"),
					Div(
						Class("mb-1 flex items-center gap-2"),
						Span(Class("text-xs font-semibold text-fg2"), g.Text(commentAuthor(c.Author))),
						Span(Class("flex-1")),
						cardTime(c.CreatedAt),
					),
					// Upstream text is DATA: renderMarkdown escapes first.
					Div(Class("markdown-body break-words text-xs leading-relaxed text-fg2"), renderMarkdown(c.Body)),
				)
			}),
		)),
		g.If(len(writes) > 0, Div(
			g.Attr("data-ref-writes", ""),
			Class("flex flex-col gap-2"),
			g.Map(writes, refWriteRow),
		)),
	)
}

// refWriteRow is one queued write: what it asks, and where it is.
func refWriteRow(w notes.RefWrite) g.Node {
	state := "sending…"
	if w.State == notes.RefWriteFailed {
		state = "failed: " + w.Error
	}
	what := w.Body
	if w.Kind == notes.RefWriteStatus {
		what = "status → " + w.Status
	}
	return Div(
		g.Attr("data-ref-write", strconv.FormatInt(w.ID, 10)),
		g.Attr("data-ref-write-state", w.State),
		Class("rounded-lg bg-bg/60 px-3 py-2 ring-1 ring-inset ring-line"),
		Div(
			Class("mb-1 flex items-center gap-2"),
			Span(Class("text-xs font-semibold text-fg2"), g.Text(commentAuthor(w.Author))),
			Span(Class("flex-1")),
			g.If(w.State == notes.RefWriteFailed, Span(Class("text-xs text-st-error-fg"), g.Text(state))),
			g.If(w.State != notes.RefWriteFailed, Span(Class("text-xs italic text-muted"), g.Text(state))),
		),
		Div(Class("markdown-body break-words text-xs leading-relaxed text-fg2"), renderMarkdown(what)),
	)
}

// taskLocalCommentsLabel heads a reference's own comments: they are muster's
// history, not the upstream's thread.
func taskLocalCommentsLabel(n notes.Note) g.Node {
	if n.Ref == nil || len(n.Comments) == 0 {
		return g.Text("")
	}
	return Div(
		g.Attr("data-ref-local-label", ""),
		Class("border-t border-line pt-3 text-xs text-muted"),
		g.Text("Muster-only — not on the upstream"),
	)
}
