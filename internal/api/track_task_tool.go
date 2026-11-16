package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// track_task (docs/focus.md#a-sessions-tasks, migration 0136): a MIRRORED
// session records which tasks it works under, in its own words — the task
// keys, or "none" with one line — so the chief reads the session↔task link in
// fleet_list instead of guessing it from focus titles.
//
// 🔴 THE CALLER WRITES ITS OWN LINK, AND ONLY ITS OWN. The tool takes no
// subject: it writes the bound caller's session (the mirror door binds every
// call to a session its host lists: mirror_mcp.go), under the owner's
// principal, whose row-level security makes the row the owner's. An unknown
// argument (a "subject", say) is refused, never ignored.
//
// WHAT A KEY IS: the key a plugin task reference is filed under
// (pluginTaskKeyRefusal, the one rule — "hq:<id>" with the HQ namespace
// enabled), or one of muster's own tasks, "#<id>", that the owner has. A key
// the owner's muster holds a live task for (a reference, a copy, or "#<id>")
// also joins that task's session thread as `worked` (task_sessions: the task
// page and GET /api/sessions/{id}/tasks show it) — the existing session↔task
// link, reused. The declaration itself stands alone because most upstream
// tasks have no row here, and it is CURRENT state the session replaces, where
// a thread is history that only grows.
// ---------------------------------------------------------------------------

const trackTaskTool = "track_task"

// trackTaskInstruction is the mirror door's standing instruction when it
// offers track_task (handleMirrorMCP): short and deterministic.
const trackTaskInstruction = "When you start work, switch to other work, or are asked which task you are on, call track_task with " +
	"the task keys you work under (e.g. \"hq:<task id>\", or \"#<id>\" for a muster task), or none: \"<one line>\" when the " +
	"work is deliberately untracked. Keep it current."

func trackTaskToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: trackTaskTool,
		Description: "Record which tasks THIS session works under, so your owner and their chief see it (fleet_list). " +
			"tasks: task keys — \"<namespace>:<id>\" as your owner's task plugin files them (e.g. \"hq:<task id>\"), or \"#<id>\" " +
			"for one of your owner's muster tasks; at most 10. mode: \"replace\" (default: these are your tasks now), \"add\" " +
			"(add to the ones recorded) or \"clear\". none: instead of tasks, one line saying why this work is deliberately " +
			"untracked. note: an optional short line on what you do under them. Call it when you start or switch work.",
		Parameters: objSchema(map[string]any{
			"tasks": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": focus.MaxTaskKeys,
				"description": "Task keys: \"<namespace>:<id>\" or \"#<muster task id>\"."},
			"mode": map[string]any{"type": "string", "enum": []string{"replace", "add", "clear"},
				"description": "replace (default), add, or clear."},
			"none": strProp("Instead of tasks: one line (at most 200 characters) on why this work is deliberately untracked."),
			"note": strProp("Optional: one short line (at most 200 characters) on what you do under these tasks."),
		})}
}

// trackTaskIn is track_task's arguments.
type trackTaskIn struct {
	Tasks []string `json:"tasks"`
	Mode  string   `json:"mode"`
	None  string   `json:"none"`
	Note  string   `json:"note"`
}

const trackTaskArgs = `arguments are {tasks: [...], mode?: "replace"|"add", note?}, {none: "<one line>"} or {mode: "clear"}`

// toolTrackTask is track_task for caller c: a mirrored session (Claude Code or
// opencode) the mirror door bound. It writes c's OWN subject.
func (s *Server) toolTrackTask(ctx context.Context, c toolCaller, arguments string) string {
	if s.ext.Focus == nil {
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
	mode := focus.TaskLinkReplace
	switch in.Mode {
	case "", "replace":
	case "add":
		mode = focus.TaskLinkAdd
	case "clear":
		if len(in.Tasks) > 0 || in.None != "" || in.Note != "" {
			return toolErr("%s", trackTaskArgs)
		}
	default:
		return toolErr("%s", trackTaskArgs)
	}
	switch {
	case in.None != "" && len(in.Tasks) > 0, in.None != "" && mode == focus.TaskLinkAdd:
		return toolErr(`none is a declaration on its own: pass tasks or none, never both`)
	case in.Mode != "clear" && in.None == "" && len(in.Tasks) == 0:
		return toolErr(`%s; to say this work has no task, pass none: "<one line>"`, trackTaskArgs)
	case len(in.Tasks) > focus.MaxTaskKeys:
		return toolErr("at most %d tasks", focus.MaxTaskKeys)
	}
	keys := make([]string, 0, len(in.Tasks))
	var linked []notes.Note
	for _, raw := range in.Tasks {
		key, n, why := s.trackedTaskKey(ctx, raw)
		if why != "" {
			return toolErr("task %q: %s", capRunes(raw, 80), why)
		}
		keys = append(keys, key)
		if n.ID != 0 {
			linked = append(linked, n)
		}
	}
	got, err := s.ext.Focus.SetTaskLink(ctx, sub, focus.TaskLink{Keys: keys, None: in.None, Note: in.Note, SetBy: sub.String()}, mode)
	switch {
	case errors.Is(err, focus.ErrTaskLinkTooMany):
		return toolErr("at most %d tasks are recorded per session; replace them instead of adding", focus.MaxTaskKeys)
	case errors.Is(err, focus.ErrTaskLinkText):
		return toolErr("none and note are one line of 1 to %d characters", focus.MaxTaskTextRunes)
	case err != nil:
		s.logger.Printf("track_task: %s: %v", sub, err)
		return toolErr("the tasks could not be recorded")
	}
	// The existing session↔task thread, for a Claude Code session (its id
	// space is Claude's: task_sessions), for each key muster holds a task for.
	if mirrorClaudeCaller(c) {
		for _, n := range linked {
			_, changed, err := s.ext.Notes.LinkSession(ctx, n.ID, notes.SessionLink{SessionID: c.asker.MirrorSession,
				Role: notes.RoleWorked, Host: capRunes(c.asker.MirrorHost, maxSessionHostLen)}, false)
			if err != nil {
				s.logger.Printf("track_task: %s: join task %d's thread: %v", sub, n.ID, err)
				continue
			}
			s.broadcastTaskLink(ctx, changed, n.ID)
		}
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

// mirrorClaudeCaller reports whether c is a mirrored Claude Code session (its
// session is a Claude session id, not an opencode key).
func mirrorClaudeCaller(c toolCaller) bool {
	return c.agent == nil && !c.openCodeSession() && mirror.ValidClaudeSession(c.asker.MirrorSession)
}

// trackedTaskKey is raw as a task key a session may declare, with the live
// task the owner's muster holds for it (zero for none), or why it is not one:
// "#<id>" — one of the owner's muster tasks, which must exist — or a plugin
// task key (pluginTaskKeyRefusal: the reference rule).
func (s *Server) trackedTaskKey(ctx context.Context, raw string) (string, notes.Note, string) {
	key := strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(key, "#"); ok {
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != rest {
			return "", notes.Note{}, `a muster task is "#<id>"`
		}
		if s.ext.Notes == nil {
			return "", notes.Note{}, "this server has no task store"
		}
		n, err := s.ext.Notes.Get(ctx, id)
		if err != nil {
			return "", notes.Note{}, "not one of your owner's tasks"
		}
		return key, n, ""
	}
	key = notes.NormalizeTag(key)
	if why := pluginTaskKeyRefusal(key); why != "" {
		return "", notes.Note{}, why
	}
	if s.ext.Notes != nil {
		if n, ok, err := s.ext.Notes.FindLiveByTag(ctx, key); err == nil && ok {
			return key, n, ""
		}
	}
	return key, notes.Note{}, ""
}
