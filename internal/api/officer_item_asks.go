package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// The HQ-derived ask tools (officers/item_asks.go): officer_task_ask (a
// task-reconciler officer's) and officer_flood_block_propose (a
// web-performance officer's JUDGEMENT pool only). Each is ONE HQ call with the officer's own
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
	"HQ applies their answer itself. " +
	"Your lease on this item has ENDED (HQ set it to waiting with no holder): stop working it now — do not heartbeat, complete or release it, " +
	"and never say the task was changed."

// floodProposedNote is what a successful officer_flood_block_propose adds
// to HQ's answer.
const floodProposedNote = "proposed — NO challenge is in place yet. HQ asked a person (ask.to) to Approve & challenge or Reject; " +
	"only their approval makes HQ apply it. " +
	"Your lease on this item has ENDED (HQ set it to waiting with no holder): stop working it now — do not heartbeat, complete or release it, " +
	"and never say traffic was challenged or blocked. HQ re-opens the item with the answer and its outcome on it; claim it then to report."

// itemAskToolDefs are the two tools' definitions.
func itemAskToolDefs() []agents.ToolDef {
	item := strProp("The item's id, as officer_items or officer_brief returned it. You must hold its live lease.")
	return []agents.ToolDef{
		{Type: "function", Name: officers.ToolTaskAsk,
			Description: "Ask HQ to ask a finding's task owner to approve one action on the task (POST /officers/{id}/items/{item_id}/task-asks), for a task-reconciler.finding item you hold. " +
				"You do NOT choose the task, the person, the question or the options: HQ derives them from the item. HQ — never you — applies the person's answer. " +
				"On success NOTHING has been applied yet, so never claim the task was changed. " +
				"🔴 Success ENDS your lease on the item (it becomes waiting): stop working it — do not heartbeat, complete or release it. " +
				"HQ owns every limit; a refusal comes back verbatim with HQ's status, code and message, and nothing was written. Do not retry an identical refused call. If muster says HQ is unavailable, the request may still have committed: check the item (officer_items) for an open ask before retrying, so you do not ask twice.",
			Parameters: objSchema(map[string]any{
				"item_id": item,
				"note":    strProp("Optional: your own words, shown last in the ask and labelled as yours. Never a credential."),
			}, "item_id")},
		{Type: "function", Name: officers.ToolFloodBlockPropose,
			Description: "Propose a Cloudflare managed challenge on a bot surge's flood units, for a person to approve (POST /officers/{id}/items/{item_id}/flood-blocks), for a perf.bot_surge item you hold. " +
				"Name the item's unit ids exactly as the item gives them — never an address; HQ resolves them. muster sends action managed_challenge. " +
				"Only the person's approval makes HQ — never you — apply the challenge, so on success NOTHING is in place yet: never claim traffic was challenged or blocked. " +
				"🔴 Success ENDS your lease on the item (it becomes waiting): stop working it — do not heartbeat, complete or release it. " +
				"HQ owns every limit; a refusal comes back verbatim with HQ's status, code and message, and nothing was written. Do not retry an identical refused call. If muster says HQ is unavailable, the request may still have committed: check the item (officer_items) for an open ask before retrying, so you do not ask twice.",
			Parameters: objSchema(map[string]any{
				"item_id": item,
				"unit_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1,
					"description": "The unit ids of this item to challenge, exactly as the item names them."},
				"ttl_hours": map[string]any{"type": "number", "exclusiveMinimum": 0,
					"description": "How long the challenge lasts once approved, in hours (a positive number; HQ sets the range)."},
				"to": strProp("Optional: the HQ login to ask. Omit it to ask the officer's owner."),
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
