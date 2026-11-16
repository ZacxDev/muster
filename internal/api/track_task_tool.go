package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// track_task (docs/focus.md#a-sessions-tasks, migration 0137): a MIRRORED
// session records which tasks it works under, in its own words — the task
// keys, or "none" with one line — so the chief reads the session↔task link in
// fleet_list instead of guessing it from focus titles. Each call REPLACES the
// declaration; an empty list clears it.
//
// 🔴 THE CALLER WRITES ITS OWN LINK, AND ONLY ITS OWN. The tool takes no
// subject: it writes the bound caller's session (the mirror door binds every
// call to a session its host lists: mirror_mcp.go), under the owner's
// principal, whose row-level security makes the row the owner's. An unknown
// argument (a "subject", say) is refused, never ignored.
//
// WHAT A KEY IS: the key a plugin task reference is filed under
// (pluginTaskKeyRefusal, the one rule — "hq:<id>" with the HQ namespace
// enabled). The declaration stands alone, by key, because most upstream tasks
// have no row here, and it is CURRENT state the session replaces.
// ---------------------------------------------------------------------------

const trackTaskTool = "track_task"

// trackTaskOn reports whether the mirror door offers track_task: with the
// focus store (whose row holds the declaration) AND a plugin task namespace
// configured — without one no key is valid, so the tool could only refuse.
func (s *Server) trackTaskOn() bool {
	return s.ext.Focus != nil && len(notes.PluginTaskNamespaces()) > 0
}

// trackTaskExampleKey is the example key the tool, its instruction and the
// reconcile request name: in this server's first plugin task namespace.
func trackTaskExampleKey() string {
	ns := "<namespace>"
	if all := notes.PluginTaskNamespaces(); len(all) > 0 {
		ns = all[0]
	}
	return ns + ":<task id>"
}

// trackTaskInstruction is the mirror door's standing instruction when it
// offers track_task (handleMirrorMCP): short and deterministic.
func trackTaskInstruction() string {
	return "When you start work, switch to other work, or are asked which task you are on, call track_task with " +
		"the task keys you work under (e.g. \"" + trackTaskExampleKey() + "\"), or none: \"<one line>\" when the work is " +
		"deliberately untracked. Keep it current."
}

func trackTaskToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: trackTaskTool,
		Description: "Record which tasks THIS session works under, so your owner and their chief see it (fleet_list). Each call " +
			"replaces what was recorded. tasks: task keys, \"<namespace>:<id>\" as your owner's task plugin files them (e.g. " +
			"\"" + trackTaskExampleKey() + "\"); at most 10; an empty list clears them. none: instead of tasks, one line saying why this work is " +
			"deliberately untracked. note: an optional short line on what you do under the tasks. Call it when you start or switch work.",
		Parameters: objSchema(map[string]any{
			"tasks": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": focus.MaxTaskKeys,
				"description": "Task keys, \"<namespace>:<id>\"; [] clears them."},
			"none": strProp("Instead of tasks: one line (at most 200 characters) on why this work is deliberately untracked."),
			"note": strProp("Optional, with tasks: one short line (at most 200 characters) on what you do under them."),
		})}
}

// trackTaskIn is track_task's arguments: tasks (nil = not given) or none.
type trackTaskIn struct {
	Tasks *[]string `json:"tasks"`
	None  string    `json:"none"`
	Note  string    `json:"note"`
}

const trackTaskArgs = `arguments are {tasks: [...], note?} (tasks: [] clears them) or {none: "<one line>"}`

// toolTrackTask is track_task for caller c: a mirrored session (Claude Code or
// opencode) the mirror door bound. It writes c's OWN subject.
func (s *Server) toolTrackTask(ctx context.Context, c toolCaller, arguments string) string {
	if !s.trackTaskOn() {
		return toolErr("unknown tool %q", trackTaskTool)
	}
	sub := focus.MirrorSubject(c.asker.MirrorSession)
	if c.agent != nil || c.asker.MirrorHost == "" || sub.Valid() != nil {
		return toolErr("track_task records a mirrored session's own tasks; this caller is not one")
	}
	var in trackTaskIn
	dec := json.NewDecoder(bytes.NewReader([]byte(orEmptyJSON(arguments))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return toolErr("%s (it always writes this session's own tasks: there is no subject to pass)", trackTaskArgs)
	}
	switch {
	case in.None != "" && in.Tasks != nil:
		return toolErr(`none is a declaration on its own: pass tasks or none, never both`)
	case in.None == "" && in.Tasks == nil:
		return toolErr(`%s; to say this work has no task, pass none: "<one line>"`, trackTaskArgs)
	case in.Note != "" && (in.Tasks == nil || len(*in.Tasks) == 0):
		return toolErr("note goes with tasks")
	case in.Tasks != nil && len(*in.Tasks) > focus.MaxTaskKeys:
		return toolErr("at most %d tasks", focus.MaxTaskKeys)
	}
	var keys []string
	if in.Tasks != nil {
		for _, raw := range *in.Tasks {
			key := notes.NormalizeTag(raw)
			if why := pluginTaskKeyRefusal(key); why != "" {
				return toolErr("task %q: %s", capRunes(raw, 80), why)
			}
			keys = append(keys, key)
		}
	}
	got, err := s.ext.Focus.SetTaskLink(ctx, sub, focus.TaskLink{Keys: keys, None: in.None, Note: in.Note})
	switch {
	case errors.Is(err, focus.ErrTaskLinkText):
		return toolErr("none and note are one line of 1 to %d characters", focus.MaxTaskTextRunes)
	case err != nil:
		s.logger.Printf("track_task: %s: %v", sub, err)
		return toolErr("the tasks could not be recorded")
	}
	out := map[string]any{"subject": sub.String(), "tasks": got.Keys, "set_at": got.SetAt.UTC().Format(time.RFC3339)}
	if got.Keys == nil {
		out["tasks"] = []string{}
	}
	if got.None != "" {
		out["untracked"] = got.None
	}
	if got.Note != "" {
		out["task_note"] = got.Note
	}
	return toolJSON(out)
}
