package api

import (
	"context"
	"errors"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// The task read tools (officer_task_list, officer_task_get): a task-reconciler
// officer reads HQ tasks through HQ's officer task route with its own officer
// token (officers/tasks.go). Offered by officers.ToolOffered to that type only.
//
// They are NOT reads of the item's read budget (officers.CountsRead): that
// budget bounds the cluster and observability reads an investigation spends,
// while HQ already rate-limits this route per officer (shared with obs), and
// the reconciler's bounded checklist reads one task per item plus, for a
// duplicate, the other one.

// taskReadToolDefs are the two task read tools.
func taskReadToolDefs() []agents.ToolDef {
	return []agents.ToolDef{
		{Type: "function", Name: officers.ToolTaskList,
			Description: "List HQ tasks (GET /officers/{id}/tasks): rows with id, title, type, status, parent, assignees, owner, due, priority, created, updated, rev, " +
				"last_comment_at, children_open and children_done. Private tasks are never listed. Page with cursor = the answer's `next` until it is empty. " +
				"An argument muster does not know is refused, not ignored. A 403 task_read_off means HQ does not let this officer read tasks (muster offers the tool by officer type; HQ enforces its task_read policy): do not retry. " +
				"A refusal with code task_route_unavailable means this HQ has no task route yet (it is older than this tool): it says nothing about any task; do not retry, report it. " +
				"A 429 means wait a minute (the limit is shared with the obs queries). Read-only.",
			Parameters: objSchema(map[string]any{
				"open":   map[string]any{"type": "boolean", "description": "true: only tasks not in a done status of their type."},
				"status": strProp("Only these statuses, comma-separated (e.g. \"to do,paused\"). Not together with open."),
				"since":  strProp("Only tasks updated after this time, RFC 3339 (e.g. 2026-11-01T00:00:00Z)."),
				"cursor": strProp("The previous answer's `next`."),
				"limit":  intProp("At most this many rows (default HQ's).", 1, 200),
			})},
		{Type: "function", Name: officers.ToolTaskGet,
			Description: "Read one HQ task (GET /officers/{id}/tasks/{task_id}): the task, its comments (oldest first) and its type's statuses and done_statuses. " +
				"A private or absent task is a 404 task_not_visible. A refusal with code task_route_unavailable means this HQ has no task route yet: NOT a missing task; do not retry, report it. " +
				"An answer over muster's 1 MiB cap is refused whole. Quote what you use from it with its date. Read-only.",
			Parameters: objSchema(map[string]any{
				"task_id": strProp("The task's id, as officer_task_list or an item's payload names it."),
			}, "task_id")},
	}
}

// taskToolResult is officerToolResult, except that a task route an older HQ
// does not have (officers.TaskRouteUnavailableError) is its own final
// refusal, task_route_unavailable — never HQ's 404 passed on as "absent".
func taskToolResult(name string, body []byte, err error) string {
	var ru *officers.TaskRouteUnavailableError
	if errors.As(err, &ru) {
		refused := map[string]any{"status": ru.HQ.Status, "code": officers.TaskRouteUnavailableCode}
		return toolJSON(map[string]any{"error": "HQ has no officer task route (an HQ older than the task read), so " + name +
			" cannot run: this says nothing about the task. Do not retry; report it.", "refused": refused})
	}
	return officerToolResult(name, body, err)
}

// officerTaskRead runs one task read tool with client c.
func officerTaskRead(ctx context.Context, c *officers.Client, name, arguments string) string {
	if name == officers.ToolTaskGet {
		id, err := officers.TaskGetID([]byte(orEmptyJSON(arguments)))
		if err != nil {
			return toolErr("%v", err)
		}
		body, err := c.TaskGet(ctx, id)
		return taskToolResult(name, body, err)
	}
	q, err := officers.TaskListQuery([]byte(orEmptyJSON(arguments)))
	if err != nil {
		return toolErr("%v", err)
	}
	body, err := c.TaskList(ctx, q)
	return taskToolResult(name, body, err)
}
