package api

import (
	"context"
	"errors"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// The PR context read tool (officer_pr_context): a web-performance officer's
// JUDGEMENT instance reads, for one PR in a compare range its item names,
// what HQ holds about it, through HQ's officer PR context route with the
// officer's own token (officers/pr_context.go). Offered by
// officers.ToolOffered to that pool only.
//
// Like the task reads it is NOT a read of the item's read budget
// (officers.CountsRead): HQ rate-limits the route per officer (shared with
// obs), and the attribution checklist reads one PR at a time from a bounded
// shortlist.

// prContextToolDef is the PR context read tool.
func prContextToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: officers.ToolPRContext,
		Description: "Read what HQ holds about ONE PR of a compare range your item names (GET /officers/{id}/prs/{pr}/context?item=): its release row (title, author, merged_at, head_sha), " +
			"the release captain's notes for the releases that shipped it, its review rounds (status, verdict, blocking count, finding titles) and its why entries and finding responses, each with HQ's provenance. " +
			"ATTRIBUTION EVIDENCE ONLY: never use it to decide or restate a review verdict, and never quote it on GitHub. Cite what you use. " +
			"A 404 pr_not_visible means the PR is in no compare range your live item names: do not retry with another item. " +
			"A 403 pr_context_read_off means HQ does not let this officer read PR context: do not retry. " +
			"A refusal with code pr_context_route_unavailable means this HQ has no PR context route yet (it is older than this tool): it says nothing about the PR; do not retry, report it. " +
			"A 429 means wait a minute (the limit is shared with the obs queries). An argument muster does not know is refused, not ignored. Read-only.",
		Parameters: objSchema(map[string]any{
			"item_id": strProp("The item whose compare range names the PR, as officer_items or officer_brief returned it."),
			"pr":      intProp("The PR number, e.g. 7251.", 1, 9999999999),
		}, "item_id", "pr")}
}

// prContextToolResult is officerToolResult, except that a route an older HQ
// does not have (officers.PRContextRouteUnavailableError) is its own final
// refusal, pr_context_route_unavailable — never HQ's 404 passed on as "not
// in range".
func prContextToolResult(name string, body []byte, err error) string {
	var ru *officers.PRContextRouteUnavailableError
	if errors.As(err, &ru) {
		refused := map[string]any{"status": ru.HQ.Status, "code": officers.PRContextRouteUnavailableCode}
		return toolJSON(map[string]any{"error": "HQ has no officer PR context route (an HQ older than the read), so " + name +
			" cannot run: this says nothing about the PR. Do not retry; report it.", "refused": refused})
	}
	return officerToolResult(name, body, err)
}

// officerPRContext runs officer_pr_context with client c.
func officerPRContext(ctx context.Context, c *officers.Client, arguments string) string {
	a, err := officers.ParsePRContextArgs([]byte(orEmptyJSON(arguments)))
	if err != nil {
		return toolErr("%v", err)
	}
	body, err := c.PRContext(ctx, a)
	return prContextToolResult(officers.ToolPRContext, body, err)
}
