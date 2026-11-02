package mirror

import "strings"

// CLAUDE CODE'S QUEUED MESSAGES (tunnel v2): a message sent to a busy Claude
// Code session — from muster or typed in the terminal — waits in the CLI's
// own input queue until Claude takes it, and only then reaches the
// transcript as a prompt. The host folds the transcript's queue-operation
// records (internal/cctranscript's Queue) into the list of what is waiting,
// and sends it on the view's stream so the rendered view can show it.
//
// 🔴 NOT muster's send queue (internal/sendqueue): that one holds a message IN
// MUSTER until the window is idle, and muster draws it from its own store.
// This list is what the CLI already holds.
//
// One shape on the wire, host → muster: an events message with QueuedList
// set carries Queued, the WHOLE list (empty is a list too), sent when it
// changed, and on every Reset batch and resume marker — so a page that drew a
// list from an earlier socket (a snapshot, a reconnect) is told the current
// one at once, and nothing it drew earlier lingers.
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, like SubagentList: an
// older host sends no QueuedList, and muster draws no queued messages; an
// older muster ignores the fields (a list-only message relays as an empty
// batch at an offset it already had).
//
// 🔴 EVERY STRING IN IT IS TRANSCRIPT TEXT, REDACTED ON THE HOST like a
// prompt's text, then cleaned and bounded by NormalizeQueued — applied by
// BOTH halves — and drawn by the page as text.

const (
	// MaxQueued bounds the list: the OLDEST are kept, the next ones Claude
	// takes.
	MaxQueued = 20
	// MaxQueuedText bounds each message, in runes (CleanText): the view shows
	// a two-line preview, never the whole of a long message.
	MaxQueuedText = 500
)

// NormalizeQueued is in as the wire carries it: each message cleaned and
// bounded (CleanText), blank ones dropped, at most MaxQueued. nil when none.
func NormalizeQueued(in []string) []string {
	var out []string
	for _, s := range in {
		s = CleanText(s, MaxQueuedText)
		if strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, s)
		if len(out) == MaxQueued {
			break
		}
	}
	return out
}
