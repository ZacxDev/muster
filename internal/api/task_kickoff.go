package api

import (
	"math"
	"strings"

	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// A TASK'S TEXT IN AN AGENT'S FIRST MESSAGE — ONE RULE, ONE PLACE.
//
// 🔴 TASK TEXT A PLUGIN WROTE REACHES AN AGENT ONLY INSIDE THE UNTRUSTED FENCE,
// WHATEVER PATH STARTS THE AGENT. A plugin files a task's title and body through
// POST /plugin/v1/tasks (plugin_tasks.go), or pushes them as a reference's
// snapshot, and muster stores them as sent. Every door that starts an agent on a
// task — the chief's agent_dispatch, a session's approved dispatch_request, a
// host spawn, POST /api/agents with note_id — builds the first message from the
// task, and each one used to paste task.Body raw: a plugin's "ignore previous
// instructions" arrived as the agent's own task, in its first user turn.
//
// So every one of them calls taskKickoffText / taskKickoffTextWithin, and
// nothing else in this package reads a task's body into a first message: the
// seam ledger (task_kickoff_ledger_test.go) pins every reader of a task's text
// and fails when one is added that is not classified.
//
// Text the owner, the chief or an agent wrote is returned UNCHANGED: fencing it
// would tell an agent its own owner's task is untrusted.
// ---------------------------------------------------------------------------

// pluginTaskOrigin reports whether a plugin wrote n's title and body, and, when
// it did, muster's line naming where the task came from (for inside the fence).
// The two plugin shapes:
//   - a COPY task a plugin filed: CreatedBy is notes.CreatedByPlugin(label)
//     (plugin_tasks.go). A plugin's name is never blank (plugins_name_check), so
//     a plugin-filed copy always records it.
//   - a REFERENCE (n.Ref != nil): the row's title and body are the plugin's last
//     applied snapshot (notes/references.go), whoever created the row.
func pluginTaskOrigin(n notes.Note) (string, bool) {
	if n.Ref != nil {
		return "is a reference to " + fence.Sanitize(n.Ref.Key) + ", kept by a plugin from an outside system", true
	}
	if n.CreatedBy != nil && strings.HasPrefix(*n.CreatedBy, notes.CreatedByPluginPrefix) {
		return "filed by plugin " + fence.Sanitize(strings.TrimPrefix(*n.CreatedBy, notes.CreatedByPluginPrefix)), true
	}
	return "", false
}

// taskKickoffText is task n's text as an agent's first message carries it: its
// body unchanged when the owner, the chief or an agent wrote it; the title and
// body inside the plugin fence (plugins.RenderTask) when a plugin did.
func taskKickoffText(n notes.Note) string {
	t, _ := taskKickoffTextWithin(n, math.MaxInt, "")
	return t
}

// taskKickoffTextWithin is taskKickoffText in at most limit bytes. Text that does
// not fit is cut and cutNote appended (cutNote is muster's own words); ok is
// false when not even that fits, and the text is then "".
//
// 🔴 A PLUGIN'S TEXT IS CUT BEFORE IT IS FENCED, NEVER AFTER: the fence's
// closing marker is the last line of the block, so cutting the rendered block
// would leave it open. cutNote follows the closed block, outside it.
func taskKickoffTextWithin(n notes.Note, limit int, cutNote string) (string, bool) {
	origin, plugin := pluginTaskOrigin(n)
	if !plugin {
		switch {
		case len(n.Body) <= limit:
			return n.Body, true
		case limit > len(cutNote):
			return cutBytes(n.Body, limit-len(cutNote)) + cutNote, true
		default:
			return "", false
		}
	}
	render := func(body string) string { return plugins.RenderTask(origin, n.ID, n.Title, body) }
	out := render(n.Body)
	if len(out) <= limit {
		return out, true
	}
	// First cut to the room the empty block leaves (sanitising can grow a
	// line, so this can still be over), then halve until it fits.
	b := cutBytes(n.Body, limit-len(render(""))-len(cutNote))
	out = render(b) + cutNote
	for len(out) > limit && b != "" {
		b = cutBytes(b, len(b)/2)
		out = render(b) + cutNote
	}
	if len(out) > limit {
		return "", false
	}
	return out, true
}
