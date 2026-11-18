package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// The gap task tool (officers/gap_tasks.go): a datastore officer's instance
// has HQ file ONE open task per (store, gap kind) through HQ's officer gap-tasks
// route with the officer's own token, which only muster holds. HQ writes the
// task; the instance names the store and the kind. Offered by
// officers.ToolOffered to that type only (both pools), refused at dispatch
// for any other. It does not end the lease.

// gapTaskToolDef is the tool's definition.
func gapTaskToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: officers.ToolGapTask,
		Description: "File ONE HQ task for a backup/DR gap you found (POST /officers/{id}/items/{item_id}/gap-tasks), for an item you hold. " +
			"Name the store and the gap kind; HQ writes the task's title and text from its own template and assigns it to your owner — you cannot write the text or pick a person. " +
			"Kinds: no_backup, backup_unobservable, no_offsite, no_stale_alert, no_failed_alert, no_wal_alert, no_metrics, no_size_metric, mirror_unverified, routing_gap, unregistered_store. " +
			"Filed in shadow too. HQ keeps ONE OPEN task per store and kind: filed:false means that task is still open (its task_id is in the answer) — that is not an error, report the link. " +
			"Once a person closes it, filing a gap you still find files a NEW task (filed:true): file it as usual and say it was refiled. " +
			"A 429 gap_task_cap means list the rest in your summary as unfiled, do not retry. A 403 gap_tasks_off means HQ does not let this officer file: do not retry. " +
			"It does NOT end your lease: keep working the item and complete it as usual, naming each task you filed.",
		Parameters: objSchema(map[string]any{
			"item_id":   strProp("The item's id, as officer_items or officer_brief returned it. You must hold its live lease."),
			"store":     strProp("The store, as your charter's store registry names it (e.g. vector-store, git-host), or @<scope> for one fix covering many stores (e.g. @fleet-x)."),
			"kind":      strProp("The gap kind, one of the list above."),
			"namespace": strProp("Optional: the store's Kubernetes namespace."),
			"tier":      strProp("Optional: critical, secondary, index or tool (a critical store's task is high priority)."),
			"affected": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Only with store @<scope>: the stores the one fix covers."},
		}, "item_id", "store", "kind")}
}

// officerGapTask runs officer_gap_task for instance, with client c.
func officerGapTask(ctx context.Context, c officers.Backend, instance, arguments string) string {
	gb, err := officers.Serves[officers.GapTasksBackend](c, officers.CapGapTasks)
	if err != nil {
		return notServedResult(officers.ToolGapTask, err)
	}
	a, err := officers.ParseGapTaskArgs([]byte(orEmptyJSON(arguments)))
	if err != nil {
		return toolErr("%v", err)
	}
	body, err := gb.FileGapTask(ctx, instance, a)
	return officerToolResult(officers.ToolGapTask, body, err)
}
