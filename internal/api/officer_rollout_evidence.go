package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// The rollout evidence tool (officers/rollout_evidence.go): a rollout
// officer's ROUTINE-pool instance (or an untiered one's) attaches its soak
// evidence for one tag through HQ's officer rollout-evidence route (civ-hq
// docs/OFFICERS.md §5r) with the officer's own token, which only muster
// holds; the instance never sees it. HQ shows the newest evidence for the
// tag to the person who starts the rollout. Offered by officers.ToolOffered
// to that type and pool only, refused at dispatch for any other. It does NOT
// end the lease, and it starts nothing.

// rolloutEvidenceAttachedNote is what a successful attach adds to HQ's
// answer.
const rolloutEvidenceAttachedNote = "attached — NOTHING has been started, promoted, tagged or deployed. HQ shows this evidence, labelled as your claims, " +
	"to the person who decides whether to start a rollout; only that person starts one, in HQ. " +
	"Your lease on this item is UNCHANGED: keep working it and complete it as usual, and never say a rollout started or production changed."

// rolloutEvidenceToolDef is the tool's definition.
func rolloutEvidenceToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: officers.ToolRolloutEvidenceAttach,
		Description: "Attach your soak evidence for one tag to HQ (POST /officers/{id}/items/{item_id}/rollout-evidence), for an item you hold. " +
			"Send the repository, the tag the canary runs, and your evidence: a summary that leads with your verdict and each check you ran, controls included, with its query and what it returned. " +
			"HQ stores it and shows the newest evidence for the tag, labelled as your claims, to the person who decides whether to start a rollout — a person starts every rollout in HQ, never you, so on success NOTHING is started: never claim a rollout started or production changed. " +
			"It does NOT end your lease: keep working the item and complete it as usual. " +
			"HQ owns every limit; a refusal (e.g. 403 not_allowed, 409 lease_lost, 409 rollout_actuator_off) comes back verbatim with HQ's status, code and message, and nothing was written. Report it; do not retry an identical refused call.",
		Parameters: objSchema(map[string]any{
			"item_id": strProp("The item's id, as officer_items or officer_brief returned it. You must hold its live lease."),
			"repo":    strProp("The repository the evidence is for, as owner/name (one your charter names)."),
			"tag":     strProp("The tag the evidence is for (vX.Y.Z or a preview tag), exactly as you read it."),
			// 🔴 mirrors HQ's §5r evidence shape ({summary, checks: [{name,
			// query, result}]}, closed): change it with HQ's.
			"evidence": map[string]any{"type": "object",
				"description": "Your soak evidence, shown to the person labelled as your claims: {summary, checks: [{name, query, result}]}.",
				"properties": map[string]any{
					"summary": strProp("One paragraph for the person: your verdict first, then what you read."),
					"checks": map[string]any{"type": "array", "minItems": 1,
						"description": "Each soak check you ran, positive controls included.",
						"items": objSchema(map[string]any{
							"name":   strProp("The check's name, one line."),
							"query":  strProp("The query you sent."),
							"result": strProp("What it returned."),
						}, "name", "query", "result")},
				},
				"required": []string{"summary", "checks"}, "additionalProperties": false},
		}, "item_id", "repo", "tag", "evidence")}
}

// officerRolloutEvidence runs officer_rollout_evidence_attach for instance,
// with client c. On success HQ's JSON comes back with the note; a refusal
// carries HQ's status, code, message and body (itemAskToolResult's shape).
// muster's tracking of the item is untouched either way: the lease goes on.
func officerRolloutEvidence(ctx context.Context, c officers.Backend, instance, arguments string) string {
	name := officers.ToolRolloutEvidenceAttach
	e, err := officers.ParseRolloutEvidence([]byte(orEmptyJSON(arguments)))
	if err != nil {
		return toolErr("%v", err)
	}
	rb, err := officers.Serves[officers.RolloutEvidenceBackend](c, officers.CapRollout)
	if err != nil {
		return notServedResult(name, err)
	}
	body, err := rb.AttachRolloutEvidence(ctx, instance, e)
	return itemAskToolResult(name, rolloutEvidenceAttachedNote, body, err)
}
