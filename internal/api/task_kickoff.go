package api

import (
	"math"
	"strings"

	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// A TASK'S TEXT IN FRONT OF AN AGENT — ONE RULE, ONE PLACE.
//
// 🔴 TASK TEXT A PLUGIN WROTE REACHES AN AGENT ONLY INSIDE THE UNTRUSTED FENCE,
// WHATEVER PATH HANDS IT OVER. A plugin files a task's title and body through
// POST /plugin/v1/tasks (plugin_tasks.go), pushes them as a reference's
// snapshot, or comments on a task, and muster stores all of it as sent.
//
//   - Every door that starts an agent on a task — the chief's agent_dispatch, a
//     session's approved dispatch_request, a host spawn, POST /api/agents with
//     note_id — builds the first message through taskKickoffText /
//     taskKickoffTextWithin. Each one used to paste task.Body raw.
//   - Every task tool that answers an agent with a task — task_get,
//     agent_get_task, a status answer — goes through taskToolText, and
//     tasks_list through taskListText: the plugin's text is taken out of the
//     JSON and handed over fenced, in front of it.
//
// The seam ledger (task_kickoff_ledger_test.go) pins every reader of a task's
// text and every tool answer built from a whole task, and fails when one is
// added that is not classified.
//
// Text the owner, the chief or an agent wrote is returned UNCHANGED, byte for
// byte: fencing it would tell an agent its own owner's task is untrusted.
// ---------------------------------------------------------------------------

// pluginTaskOrigin is notes.Note.PluginOrigin (the one predicate) as muster's
// line naming where the title and body came from, for inside the fence.
func pluginTaskOrigin(n notes.Note) (string, bool) {
	o, ok := n.PluginOrigin()
	switch {
	case !ok:
		return "", false
	case o.RefKey != "":
		return "is a reference to " + fence.Sanitize(o.RefKey) + ", kept by a plugin from an outside system", true
	default:
		return "filed by plugin " + fence.Sanitize(o.Label), true
	}
}

// taskKickoffText is task n's text as an agent's first message carries it: its
// body unchanged when the owner, the chief or an agent wrote it; the title and
// body inside the plugin fence (plugins.RenderTask) when a plugin did.
func taskKickoffText(n notes.Note) string {
	return taskKickoffTextWithin(n, "", math.MaxInt, "")
}

// taskKickoffTextWithin is head followed by taskKickoffText, in at most limit
// bytes; "" when not even head and the cut note fit. head is muster's own words
// (the host spawn's "Task #N…" line).
//
// An owner's text is cut exactly as the host spawn always cut it: head and
// body together, then cutNote — so its prompt is byte-identical to what it was
// before the rule, at every budget.
//
// 🔴 A PLUGIN'S TEXT IS CUT BEFORE IT IS FENCED, NEVER AFTER: the fence's
// closing marker is the last line of the block, so cutting the rendered block
// would leave it open. cutNote follows the closed block, outside it; head is
// never cut (a budget too small for head, an empty block and cutNote is "").
func taskKickoffTextWithin(n notes.Note, head string, limit int, cutNote string) string {
	origin, plugin := pluginTaskOrigin(n)
	if !plugin {
		s := head + n.Body
		switch {
		case len(s) <= limit:
			return s
		case limit > len(cutNote):
			return cutBytes(s, limit-len(cutNote)) + cutNote
		default:
			return ""
		}
	}
	render := func(body string) string { return head + plugins.RenderTask(origin, n.ID, n.Title, body) }
	out := render(n.Body)
	if len(out) <= limit {
		return out
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
		return ""
	}
	return out
}

// taskPluginText is n's plugin-written text (title and body when a plugin
// wrote them, every comment whose author is a plugin's, a reference
// snapshot's comments) for plugins.RenderTaskRead, and n with each of those
// fields BLANKED. ok is false when a plugin wrote none of it (n is then
// returned as it was).
func taskPluginText(n notes.Note) (plugins.TaskText, notes.Note, bool) {
	t := plugins.TaskText{ID: n.ID}
	origin, plugin := pluginTaskOrigin(n)
	if plugin {
		t.Origin, t.Title, t.Body = origin, n.Title, n.Body
		n.Title, n.Body = "", ""
	}
	if len(n.Comments) > 0 {
		cs := append([]notes.Comment(nil), n.Comments...)
		for i, c := range cs {
			if strings.HasPrefix(c.Author, notes.CreatedByPluginPrefix) && c.Body != "" {
				t.Comments = append(t.Comments, plugins.TaskComment{Author: c.Author, Body: c.Body})
				cs[i].Body = ""
			}
		}
		n.Comments = cs
	}
	if n.Ref != nil {
		ref := *n.Ref
		snap := ref.Snapshot
		snap.Title, snap.Body = "", ""
		if len(snap.Comments) > 0 {
			rc := append([]notes.RefComment(nil), snap.Comments...)
			for i, c := range rc {
				t.Comments = append(t.Comments, plugins.TaskComment{Author: "upstream " + c.Author, Body: c.Body})
				rc[i].Body = ""
			}
			snap.Comments = rc
		}
		ref.Snapshot = snap
		n.Ref = &ref
	}
	if t.Origin == "" && len(t.Comments) == 0 {
		return plugins.TaskText{}, n, false
	}
	return t, n, true
}

// taskToolText is a task tool's answer for task n (task_get, agent_get_task,
// a status answer): the JSON of n, with every plugin-written field blanked and
// handed over first inside the fence. A task with no plugin text answers its
// JSON exactly as before.
func taskToolText(n notes.Note) string {
	t, blanked, ok := taskPluginText(n)
	if !ok {
		return toolJSON(n)
	}
	return plugins.RenderTaskRead([]plugins.TaskText{t}) + "\n" + toolJSON(blanked)
}

// taskAgentJSON is GET /agent/task's (and its status answer's) body for task
// n: the task's JSON exactly as before when no plugin wrote any of it; else
// the same JSON with every plugin-written field blanked and one more field,
// untrusted_plugin_text, holding them fenced. A JSON answer stays JSON.
func taskAgentJSON(n notes.Note) any {
	t, blanked, ok := taskPluginText(n)
	if !ok {
		return n
	}
	return struct {
		notes.Note
		UntrustedPluginText string `json:"untrusted_plugin_text"`
	}{blanked, plugins.RenderTaskRead([]plugins.TaskText{t})}
}

// taskListTitle is a tasks_list row's title: taskTitle, or "" for a task a
// plugin wrote (its title is in taskListText's block instead).
func taskListTitle(n notes.Note) (string, *plugins.TaskText) {
	origin, plugin := pluginTaskOrigin(n)
	if !plugin {
		return taskTitle(n), nil
	}
	return "", &plugins.TaskText{ID: n.ID, Origin: origin, Title: taskTitle(n), TitleOnly: true}
}

// taskListText is tasks_list's answer: the rows' JSON, after a fence holding
// the titles of the listed tasks a plugin wrote (fenced, never their bodies:
// the list carries none). With no plugin task listed it is the JSON alone.
func taskListText(rows any, fenced []plugins.TaskText) string {
	if len(fenced) == 0 {
		return toolJSON(rows)
	}
	return plugins.RenderTaskRead(fenced) + "\n" + toolJSON(rows)
}
