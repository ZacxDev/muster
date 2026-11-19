package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// --- Rollout evidence: a rollout officer attaches its soak evidence -----------
//
// A PERSON starts every rollout in HQ (the rollout officer's owner or one of
// its escalation logins, for a tag they pick); the officer reads the canary's
// soak and ATTACHES its evidence for the tag, which HQ shows that person,
// labelled as the officer's claims, on the confirm step of a start:
//
//	POST /officers/{id}/items/{item}/rollout-evidence  {"instance", "repo", "tag", "evidence"}
//
// The route contract — the tag's form, the evidence's limits, the allowlist,
// the refusal codes — is HQ's (civ-hq docs/OFFICERS.md §5r, "The evidence").
// It is not copied here: a copy would drift. HQ requires the instance to
// hold the item's LIVE lease (409 lease_lost otherwise). Unlike an ask, an
// attach does NOT end the lease: the instance keeps working the item and
// completes it as usual. It starts nothing: no route an officer token
// reaches starts a rollout.
//
// It replaces officer_rollout_promote_propose (the officer proposing a
// promotion), whose HQ route answers 410 officer_propose_retired since HQ
// moved the start to a person.
//
// WHO MAY CALL: muster offers the tool by officer TYPE NAME (ToolOffered:
// rollout, its ROUTINE pool and an untiered one); HQ enforces the type
// POLICY (rollout_actuator, its repository allowlist) and the lease. muster
// owns the body's SHAPE: it builds the body itself, so the arguments are
// decoded STRICTLY — an argument muster does not know is refused before HQ
// is called, never silently dropped.

// ToolRolloutEvidenceAttach is a rollout officer's: attach its soak evidence
// for one tag to HQ, for the person who starts the rollout.
const ToolRolloutEvidenceAttach = "officer_rollout_evidence_attach"

// RolloutOfficerType is the officer type whose instances attach rollout
// evidence.
const RolloutOfficerType = "rollout"

// RolloutEvidence is officer_rollout_evidence_attach's arguments: {item_id,
// repo, tag, evidence}.
type RolloutEvidence struct {
	Item ItemID
	Repo string
	Tag  string
	// Evidence is the evidence object exactly as the instance gave it. muster
	// forwards it whole, so nothing in it is ever silently dropped; HQ's
	// decoder refuses an unknown or ill-typed field inside it (400 invalid).
	// The tool's SCHEMA (internal/api rolloutEvidenceToolDef) mirrors HQ's
	// §5r evidence shape and must change with it.
	Evidence json.RawMessage
}

// ParseRolloutEvidence decodes officer_rollout_evidence_attach's arguments
// strictly. It checks TYPES and presence only: the values (which repository,
// the tag's form, the evidence's limits) are HQ's to refuse.
func ParseRolloutEvidence(raw []byte) (RolloutEvidence, error) {
	obj, err := strictObject(raw, ToolRolloutEvidenceAttach, "{item_id, repo, tag, evidence}", "item_id", "repo", "tag", "evidence")
	if err != nil {
		return RolloutEvidence{}, err
	}
	item, err := itemArg(obj)
	if err != nil {
		return RolloutEvidence{}, err
	}
	e := RolloutEvidence{Item: item}
	if json.Unmarshal(obj["repo"], &e.Repo) != nil || strings.TrimSpace(e.Repo) == "" {
		return RolloutEvidence{}, fmt.Errorf("repo is required: the repository as owner/name (a string)")
	}
	if json.Unmarshal(obj["tag"], &e.Tag) != nil || strings.TrimSpace(e.Tag) == "" {
		return RolloutEvidence{}, fmt.Errorf("tag is required: the tag the evidence is for, as you read it (a string)")
	}
	ev := bytes.TrimSpace(obj["evidence"])
	var probe map[string]json.RawMessage
	if len(ev) == 0 || ev[0] != '{' || json.Unmarshal(ev, &probe) != nil {
		return RolloutEvidence{}, fmt.Errorf("evidence is required: one JSON object {summary, checks: [{name, query, result}]}")
	}
	e.Evidence = append(json.RawMessage(nil), ev...)
	return e, nil
}

// AttachRolloutEvidence is POST …/items/{item}/rollout-evidence, HQ's JSON
// passed through (its 201 {evidence}); every refusal is a 4xx.
func (c *Client) AttachRolloutEvidence(ctx context.Context, instance string, e RolloutEvidence) ([]byte, error) {
	return c.Do(ctx, http.MethodPost, c.itemPath(e.Item, "rollout-evidence"), struct {
		Instance string          `json:"instance"`
		Repo     string          `json:"repo"`
		Tag      string          `json:"tag"`
		Evidence json.RawMessage `json:"evidence"`
	}{instance, e.Repo, e.Tag, e.Evidence})
}
