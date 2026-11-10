package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// The HQ-derived ask tools (officers/item_asks.go): officer_task_ask (a
// task-reconciler officer's) and officer_flood_block_propose (a
// web-performance officer's). Each is ONE HQ call with the officer's own
// token, which only muster holds; the instance never sees it. HQ derives the
// person, the question and the options, and HQ — never the officer — applies
// the person's answer. Offered by officers.ToolOffered to the named type only,
// and refused at dispatch for any other (dispatchOfficerTool).
//
// The lease is HQ's to check (409 lease_lost), as for officer_ask_person.
// A success creates an HQ ask, which ENDS the instance's lease (the item is
// waiting with no holder), so the transcript forwarder and the item meter
// stop for it (officerItemEnded) and the result says so in words.

// taskAskedNote is what a successful officer_task_ask adds to HQ's answer.
const taskAskedNote = "asked — NOTHING has been applied to the task yet. HQ asked the person it chose (ask.to) to approve one action; " +
	"HQ applies their answer itself, as them, only while the task is unchanged since detection, and then closes this item. " +
	"Your lease on this item has ENDED (HQ set it to waiting with no holder): stop working it now — do not heartbeat, complete or release it, " +
	"and never say the task was changed. An unanswered ask expires at its deadline and HQ closes the item no_action."

// floodProposedNote is what a successful officer_flood_block_propose adds
// to HQ's answer.
const floodProposedNote = "proposed — NO challenge is in place yet. HQ asked a person (ask.to) to Approve & challenge or Reject; " +
	"only their Approve makes HQ re-check every guardrail and add the prefixes to HQ's Cloudflare rule, and HQ removes each at its expiry. " +
	"Your lease on this item has ENDED (HQ set it to waiting with no holder): stop working it now — do not heartbeat, complete or release it, " +
	"and never say traffic was challenged or blocked. HQ re-opens the item with the answer and its outcome on it; claim it then to report."

// itemAskToolDefs are the two tools' definitions.
func itemAskToolDefs() []agents.ToolDef {
	item := strProp("The item's id, as officer_items or officer_brief returned it. You must hold its live lease.")
	return []agents.ToolDef{
		{Type: "function", Name: officers.ToolTaskAsk,
			Description: "Ask HQ to ask a finding's task owner to approve one action on the task (POST /officers/{id}/items/{item_id}/task-asks). " +
				"Only for a task-reconciler.finding item you hold. You do NOT choose the task, the person, the question or the options: HQ reads the task, " +
				"detector and rev from the item, asks the task's first HQ-member assignee (else its owner), offers its own options (e.g. Close / Keep open) " +
				"and may post one evidence comment on the task. HQ — never you — applies the person's answer, as that person, only if the task is unchanged since detection. " +
				"So never claim the task was changed: on success NOTHING has been applied yet. " +
				"🔴 Success ENDS your lease on the item (it becomes waiting): stop working it — do not heartbeat, complete or release it. " +
				"Limits are HQ's: note at most 500 characters; one open ask per task; a cooldown (default 30 days) after a Keep, a no-choice answer or an expiry; " +
				"at most a few asks a day to one person (default 3). Every HQ refusal comes back verbatim with its status and code, and nothing was written: " +
				"403 task_asks_off (the type's policy does not allow task asks); 409 officer_shadow (a shadow officer asks nobody: put the ask you would make in officer_complete's summary), " +
				"finding_void (the task changed since detection: close the item without asking), task_ask_open, task_cooldown, task_asks_policy_invalid, lease_lost (you no longer hold the item: stop); " +
				"422 not_a_finding, no_recipient, no_options, recipient_muted, recipient_unreachable; 429 owner_daily_cap (defer the finding: a later sweep offers it again) or rate_limited. " +
				"Do not retry a refusal unchanged.",
			Parameters: objSchema(map[string]any{
				"item_id": item,
				"note":    strProp("Optional: your own words (at most 500 characters), shown last in the ask and labelled as yours. Never a credential."),
			}, "item_id")},
		{Type: "function", Name: officers.ToolFloodBlockPropose,
			Description: "Propose a Cloudflare managed challenge on a bot surge's flood units, for a person to approve (POST /officers/{id}/items/{item_id}/flood-blocks). " +
				"Only for a perf.bot_surge item you hold (or one already waiting on an ask). You name the item's surging prefix unit ids (opaque ids like \"prefix:…\" from the item) — never an address: " +
				"HQ resolves them itself. HQ checks every guardrail, then asks the officer's owner (or `to`, a policy approver) to Approve & challenge or Reject. " +
				"Only the person's Approve makes HQ — never you — add the prefixes to HQ's one Cloudflare rule, until ttl_hours from approval, then HQ removes them. " +
				"So never claim traffic was challenged or blocked: on success NOTHING is in place yet. muster sends action managed_challenge (the only action). " +
				"🔴 Success ENDS your lease on the item (it becomes waiting): stop working it. " +
				"Limits are HQ's: ttl_hours from 5 minutes up to the policy's max (at most 24); at most the policy's units per proposal (default 10); the rule's total prefixes and length; " +
				"no /16-or-wider IPv4 or /32-or-wider IPv6 prefix, nothing overlapping a protected range. Every HQ refusal comes back verbatim with its status and code, and nothing was written: " +
				"409 flood_actuator_off (the type's policy does not enable it), flood_policy_invalid, lease_lost (stop); 422 too_many_prefixes, not_a_prefix_unit, not_a_bot_surge, unit_not_surging, " +
				"unit_unknown, prefix_refused, list_full, ttl_refused, not_an_approver; 503 flood_actuator_not_configured (HQ has no Cloudflare credential: final, do not retry; report it). " +
				"A 404 means the item is not this officer's, or this HQ has no flood route yet. Do not retry a refusal unchanged.",
			Parameters: objSchema(map[string]any{
				"item_id": item,
				"unit_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1,
					"description": "The surging prefix unit ids of this item to challenge, exactly as the item names them (\"prefix:…\")."},
				"ttl_hours": map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 24,
					"description": "How long the challenge lasts once approved, in hours (HQ takes 5 minutes up to the policy's max, at most 24)."},
				"to": strProp("Optional: the HQ login to ask — a policy approver. Omit it to ask the officer's owner."),
			}, "item_id", "unit_ids", "ttl_hours")},
	}
}

// officerItemAsk runs one of the two tools for instance, with client c.
func (s *Server) officerItemAsk(ctx context.Context, c *officers.Client, o officers.Officer, instance, name, arguments string) string {
	var (
		item officers.ItemID
		body []byte
		err  error
		note string
	)
	switch name {
	case officers.ToolTaskAsk:
		a, perr := officers.ParseTaskAskArgs([]byte(orEmptyJSON(arguments)))
		if perr != nil {
			return toolErr("%v", perr)
		}
		item, note = a.Item, taskAskedNote
		body, err = c.CreateTaskAsk(ctx, instance, a)
	case officers.ToolFloodBlockPropose:
		p, perr := officers.ParseFloodBlockProposal([]byte(orEmptyJSON(arguments)))
		if perr != nil {
			return toolErr("%v", perr)
		}
		item, note = p.Item, floodProposedNote
		body, err = c.ProposeFloodBlock(ctx, instance, p)
	default:
		return toolErr("%s is not an item ask tool", name)
	}
	if err == nil {
		s.officerItemEnded(o, instance, item)
	}
	return itemAskToolResult(name, note, body, err)
}
