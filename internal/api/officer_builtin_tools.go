package api

import (
	"slices"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE OFFICER TOOLS AS A BUILTIN OFFICER IS OFFERED THEM (officers redesign
// O-B, F4)
//
// The registry's officer tool descriptions (queueToolDefs) are an HQ
// officer's: "your brief from HQ", "HQ picks the next item", "ask a named HQ
// member". A builtin officer has no HQ, and its kickoff used to carry a patch
// for it ("wherever a tool's description says HQ, read muster"). Now a
// builtin officer's instances are offered these descriptions instead — every
// tool a builtin officer gets, worded for muster's own queue — so no text a
// builtin instance reads says HQ (TestNoBuiltinOfficerTextSaysHQ walks all of
// it). An HQ officer's definitions are untouched.
// ---------------------------------------------------------------------------

// builtinToolDef is tool definition d as a builtin officer's instances are
// offered it.
func builtinToolDef(o officers.Officer, d agents.ToolDef) agents.ToolDef {
	item := strProp("The item's id, as officer_items or officer_brief returned it.")
	switch d.Name {
	case officers.ToolBrief:
		d.Description = "Load your officer's brief from muster: your charter (your standing instructions, versioned), your open items, recent outcomes and your memory scopes. Call it first when you wake, instead of looking for an earlier conversation."
	case officers.ToolItems:
		d.Description = "List your officer's items in muster's queue. status: open (default: what you can claim), claimed, waiting (an ask on it is open: see officer_ask_person), done or escalated."
		d.Parameters = objSchema(map[string]any{
			"status": map[string]any{"type": "string", "enum": officers.ItemStatuses},
			"limit":  intProp("At most this many (default muster's).", 1, 100),
			"cursor": strProp("The `next` value of the previous page (each page carries it; null on the last)."),
		})
	case officers.ToolClaim:
		d.Description = "Claim one item so no other instance works it. With NO item_id, muster picks the next item for you (highest priority first, then the earliest due, then the oldest; never a blocked one) and the answer is that item — or item null when nothing is claimable: stop and wait. " +
			"Give an item_id only when your charter says to claim a particular item. If YOU already hold it (e.g. you were restarted mid-item), this succeeds, renews your lease and says \"already yours\" — carry on with it. A conflict answer (refused, status 409) means ANOTHER instance holds it: pick another item. Heartbeat before the lease runs out."
	case officers.ToolAskPerson:
		d.Description = "Ask your officer's owner a question about an item you hold, when you need their decision to finish it. " +
			"muster shows it on their Attention view and their phone. 🔴 Asking ENDS your lease on the item: muster sets it to waiting with no holder, so stop working it — " +
			"do not heartbeat, complete or release it. muster re-opens it (claimable) with the answers on it once every open ask on it is answered or withdrawn, " +
			"and wakes an instance for it; if the deadline passes unanswered, muster escalates the item to its owner. " +
			"A second ask on a waiting item is allowed (the same item_id). Images are not served: describe what the owner must look at."
		d.Parameters = objSchema(map[string]any{
			"item_id":  item,
			"to":       strProp("\"owner\": the question always goes to your officer's owner, whatever this says."),
			"question": strProp("The question. Say what you need decided and why."),
			"options": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Optional answers to pick from: short, unique labels."},
			"context_url":      strProp("Optional http(s) URL with the context (a pull request, a preview, a dashboard)."),
			"deadline_seconds": map[string]any{"type": "integer", "description": "Optional: how long they have to answer, in seconds (muster's default is a day)."},
		}, "item_id", "question")
	case officers.ToolComplete:
		return builtinCompleteToolDef(o)
	}
	return d
}

// builtinCompleteToolDef is officer_complete for builtin officer o's pool: its
// outcomes (needs_judgement for a routine pool only), the optional
// investigation block (every pool but a routine one, as the built-in backend
// takes it), and no hand-off — that is an HQ review loop's.
func builtinCompleteToolDef(o officers.Officer) agents.ToolDef {
	desc := "Finish an item you hold: outcome done, escalated (a human must act; say who and why in the summary) or no_action"
	if slices.Contains(officers.CompleteOutcomes(o), officers.OutcomeNeedsJudgement) {
		desc += ", or " + officers.OutcomeNeedsJudgement + " (hand a routine item to the judgement pool: muster makes it a judgement item, open again with your summary kept " +
			"for the judgement instance that takes it, and your hold ends; its links are not kept — put them in the summary)"
	}
	desc += ", with a summary (at most 24,576 characters) and optional links. " +
		"An escalated item goes to your officer's owner (and whoever it names to escalate to) on muster's Attention view."
	props := map[string]any{
		"item_id": strProp("The item's id, as officer_items or officer_brief returned it."),
		"outcome": map[string]any{"type": "string", "enum": officers.CompleteOutcomes(o)},
		"summary": strProp("What you found and did, at most 24,576 characters."),
		"links":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": officers.MaxLinks, "description": "Optional http(s) URLs (a PR, a dashboard, a report), at most 10."},
	}
	if officers.CompleteTakesInvestigation(o) {
		props["investigation"] = map[string]any{"type": "object", "additionalProperties": true,
			"description": "Optional: your investigation, kept on outcome escalated: {hypotheses: [text], eliminated: [{hypothesis, evidence}], root_cause, " +
				"confidence: low|medium|high, fix: {pr_urls: [...]} OR {operator_steps: [text]}, not_checked: [text]}. muster checks it and refuses what does not fit, naming the field."}
	}
	return agents.ToolDef{Type: "function", Name: officers.ToolComplete, Description: desc,
		Parameters: objSchema(props, "item_id", "outcome", "summary")}
}
