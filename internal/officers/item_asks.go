package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// --- HQ-derived asks: task asks, flood-block and rollout proposals ------------
//
// Three HQ routes where the officer PROPOSES and a person, asked by HQ,
// DECIDES; HQ applies the decision with its own credentials. Each takes the
// officer's own token and the instance holding the item, and each creates an
// ordinary HQ person ask (civ-hq docs/OFFICERS.md §5h), so — like
// officer_ask_person — a success puts the item in `waiting` with no holder:
// the instance's lease ENDS.
//
//	POST /officers/{id}/items/{item}/task-asks           {"instance", "note"?}
//	POST /officers/{id}/items/{item}/flood-blocks        {"instance", "to"?, "unit_ids", "action", "ttl_hours"}
//	POST /officers/{id}/items/{item}/rollout-promotions  {"instance", "repo", "preview_tag", "evidence"}
//
// The route contract — what HQ derives, its limits and its refusal codes —
// is HQ's, in HQ's officer docs (docs/OFFICERS.md, "Task asks", "Flood
// remediation" and "Rollout promotions"). It is not copied here: a copy
// would drift.
//
// WHO MAY CALL: muster offers each tool by officer TYPE NAME (ToolOffered:
// task-reconciler, both pools; web-performance, its JUDGEMENT pool only —
// operator decision on muster#565; rollout, its ROUTINE pool (and an
// untiered one) — HQ tiers the sweep, the item that proposes, routine, and
// the judgement pool only escalates failed results); HQ enforces the
// type POLICY (task_asks, flood_actuator, rollout_actuator) and the lease. muster does NOT check the lease:
// officer_ask_person does not either, and HQ's check is the authoritative one
// (409 lease_lost) — a flood proposal is also allowed on a WAITING item,
// which a local "holds a live claim" check would wrongly refuse.
//
// 🔴 muster DOES NOT RE-CHECK HQ'S LIMITS: HQ enforces them and its 4xx,
// with its code, is what the instance gets back. What
// muster DOES own is the body's SHAPE: it builds the body itself, so the
// arguments are decoded STRICTLY — an argument muster does not know is
// refused before HQ is called, never silently dropped (HQ's decoder would
// refuse it too, but only if muster forwarded it).

// The three tools.
const (
	// ToolTaskAsk is a task-reconciler officer's: ask HQ to ask a finding's
	// task owner to approve one action (HQ picks the person and the options).
	ToolTaskAsk = "officer_task_ask"
	// ToolFloodBlockPropose is a web-performance officer's: propose a
	// Cloudflare managed challenge on a bot surge's flood units, for a person
	// to approve.
	ToolFloodBlockPropose = "officer_flood_block_propose"
	// ToolRolloutPromotePropose is a rollout officer's: propose promoting one
	// soaked preview tag to production, for a person to approve; HQ — never
	// the officer — dispatches the promotion with its own credentials.
	ToolRolloutPromotePropose = "officer_rollout_promote_propose"
)

// RolloutOfficerType is the officer type whose instances may propose
// rollout promotions.
const RolloutOfficerType = "rollout"

// RolloutNotConfiguredCode is the rollout route's 503 code for an HQ whose
// rollout credential is unset: a FINAL refusal (a deployment's decision, not
// an outage), as FloodNotConfiguredCode is the flood route's.
const RolloutNotConfiguredCode = "rollout_actuator_not_configured"

// rolloutDecisionCodes are the rollout route's 5xx codes that are decisions;
// rolloutRetryableCodes the ones it documents as transient. HQ's 502
// github_unavailable writes nothing (HQ could not read GitHub, so it decided
// nothing), so the same call may be made again — as the merge-asks route's.
var (
	rolloutDecisionCodes  = []string{RolloutNotConfiguredCode}
	rolloutRetryableCodes = []string{"github_unavailable"}
)

// WebPerformanceOfficerType is the officer type whose instances may propose
// flood blocks.
const WebPerformanceOfficerType = "web-performance"

// FloodBlockAction is the only action HQ's flood route takes. muster sends
// it; the tool does not take it as an argument.
const FloodBlockAction = "managed_challenge"

// FloodNotConfiguredCode is the flood route's 503 code for an HQ whose
// Cloudflare credential is unset: a FINAL refusal (a deployment's decision,
// not an outage), so it is passed back as one, never "retry in a minute".
const FloodNotConfiguredCode = "flood_actuator_not_configured"

// floodDecisionCodes are the flood route's 5xx codes that are decisions.
var floodDecisionCodes = []string{FloodNotConfiguredCode}

// strictObject decodes raw as one JSON object whose keys are all in allowed.
func strictObject(raw []byte, tool, shape string, allowed ...string) (map[string]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	var obj map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("arguments must be one JSON object: %s", shape)
	}
	for k := range obj {
		known := false
		for _, a := range allowed {
			known = known || k == a
		}
		if !known {
			return nil, fmt.Errorf("%q is not an argument of %s: it takes %s only", k, tool, strings.Join(allowed, ", "))
		}
	}
	return obj, nil
}

// itemArg is a strict object's item_id: required, and safe in a URL path.
func itemArg(obj map[string]json.RawMessage) (ItemID, error) {
	var id ItemID
	if v, ok := obj["item_id"]; ok && json.Unmarshal(v, &id) != nil {
		return "", fmt.Errorf("item_id must be an item id")
	}
	if s := strings.TrimSpace(string(id)); s == "" || strings.ContainsAny(s, "/?#") {
		return "", fmt.Errorf("item_id is required (an id officer_items returned)")
	}
	return id, nil
}

// TaskAskArgs is officer_task_ask's arguments: {item_id, note?}.
type TaskAskArgs struct {
	Item ItemID
	Note string
}

// ParseTaskAskArgs decodes officer_task_ask's arguments strictly.
func ParseTaskAskArgs(raw []byte) (TaskAskArgs, error) {
	obj, err := strictObject(raw, ToolTaskAsk, "{item_id, note?}", "item_id", "note")
	if err != nil {
		return TaskAskArgs{}, err
	}
	item, err := itemArg(obj)
	if err != nil {
		return TaskAskArgs{}, err
	}
	a := TaskAskArgs{Item: item}
	if v, ok := obj["note"]; ok && string(bytes.TrimSpace(v)) != "null" {
		if json.Unmarshal(v, &a.Note) != nil {
			return TaskAskArgs{}, fmt.Errorf("note must be a string")
		}
	}
	return a, nil
}

// FloodBlockProposal is officer_flood_block_propose's arguments: {item_id,
// unit_ids, ttl_hours, to?}. The action is muster's (FloodBlockAction).
type FloodBlockProposal struct {
	Item     ItemID
	UnitIDs  []string
	TTLHours json.Number
	To       string
}

// ParseFloodBlockProposal decodes officer_flood_block_propose's arguments
// strictly. It checks TYPES only: the values (how many units, which, the ttl
// range, who may approve) are HQ's to refuse.
func ParseFloodBlockProposal(raw []byte) (FloodBlockProposal, error) {
	obj, err := strictObject(raw, ToolFloodBlockPropose, "{item_id, unit_ids, ttl_hours, to?}", "item_id", "unit_ids", "ttl_hours", "to")
	if err != nil {
		return FloodBlockProposal{}, err
	}
	item, err := itemArg(obj)
	if err != nil {
		return FloodBlockProposal{}, err
	}
	p := FloodBlockProposal{Item: item}
	if json.Unmarshal(obj["unit_ids"], &p.UnitIDs) != nil || len(p.UnitIDs) == 0 {
		return FloodBlockProposal{}, fmt.Errorf("unit_ids must be a non-empty array of the item's flood unit ids (strings, e.g. \"prefix:…\")")
	}
	d := json.NewDecoder(bytes.NewReader(obj["ttl_hours"]))
	d.UseNumber()
	var ttl any
	if d.Decode(&ttl) != nil {
		return FloodBlockProposal{}, fmt.Errorf("ttl_hours is required: a number of hours")
	}
	n, ok := ttl.(json.Number)
	if !ok {
		return FloodBlockProposal{}, fmt.Errorf("ttl_hours must be a number of hours")
	}
	p.TTLHours = n
	if v, ok := obj["to"]; ok && string(bytes.TrimSpace(v)) != "null" {
		if json.Unmarshal(v, &p.To) != nil {
			return FloodBlockProposal{}, fmt.Errorf("to must be a string (an HQ login)")
		}
	}
	return p, nil
}

// CreateTaskAsk is POST …/items/{item}/task-asks, HQ's JSON passed through.
func (c *Client) CreateTaskAsk(ctx context.Context, instance string, a TaskAskArgs) ([]byte, error) {
	return c.Do(ctx, http.MethodPost, c.itemPath(a.Item, "task-asks"), struct {
		Instance string `json:"instance"`
		Note     string `json:"note,omitempty"`
	}{instance, a.Note})
}

// ProposeFloodBlock is POST …/items/{item}/flood-blocks, HQ's JSON passed
// through; a 503 flood_actuator_not_configured is an *HQError (final).
func (c *Client) ProposeFloodBlock(ctx context.Context, instance string, p FloodBlockProposal) ([]byte, error) {
	body, err := json.Marshal(struct {
		Instance string      `json:"instance"`
		To       string      `json:"to,omitempty"`
		UnitIDs  []string    `json:"unit_ids"`
		Action   string      `json:"action"`
		TTLHours json.Number `json:"ttl_hours"`
	}{instance, p.To, p.UnitIDs, FloodBlockAction, p.TTLHours})
	if err != nil {
		return nil, fmt.Errorf("officers: encode request: %w", err)
	}
	return c.doCoded(ctx, http.MethodPost, c.itemPath(p.Item, "flood-blocks"), "application/json", body, floodDecisionCodes, nil)
}

// RolloutPromotion is officer_rollout_promote_propose's arguments: {item_id,
// repo, preview_tag, evidence}.
type RolloutPromotion struct {
	Item       ItemID
	Repo       string
	PreviewTag string
	// Evidence is the evidence object exactly as the instance gave it. muster
	// forwards it whole, so nothing in it is ever silently dropped; HQ's
	// decoder refuses an unknown or ill-typed field inside it (400 invalid).
	// The tool's SCHEMA (internal/api itemAskToolDefs) mirrors HQ's §5r
	// evidence shape and must change with it.
	Evidence json.RawMessage
}

// ParseRolloutPromotion decodes officer_rollout_promote_propose's arguments
// strictly. It checks TYPES only: the values (which repository, the tag's
// form, the evidence's limits) are HQ's to refuse.
func ParseRolloutPromotion(raw []byte) (RolloutPromotion, error) {
	obj, err := strictObject(raw, ToolRolloutPromotePropose, "{item_id, repo, preview_tag, evidence}", "item_id", "repo", "preview_tag", "evidence")
	if err != nil {
		return RolloutPromotion{}, err
	}
	item, err := itemArg(obj)
	if err != nil {
		return RolloutPromotion{}, err
	}
	p := RolloutPromotion{Item: item}
	if json.Unmarshal(obj["repo"], &p.Repo) != nil || strings.TrimSpace(p.Repo) == "" {
		return RolloutPromotion{}, fmt.Errorf("repo is required: the repository as owner/name (a string)")
	}
	if json.Unmarshal(obj["preview_tag"], &p.PreviewTag) != nil || strings.TrimSpace(p.PreviewTag) == "" {
		return RolloutPromotion{}, fmt.Errorf("preview_tag is required: the canary's preview tag (a string)")
	}
	ev := bytes.TrimSpace(obj["evidence"])
	var probe map[string]json.RawMessage
	if len(ev) == 0 || ev[0] != '{' || json.Unmarshal(ev, &probe) != nil {
		return RolloutPromotion{}, fmt.Errorf("evidence is required: one JSON object {summary, checks: [{name, query, result}]}")
	}
	p.Evidence = append(json.RawMessage(nil), ev...)
	return p, nil
}

// ProposeRolloutPromotion is POST …/items/{item}/rollout-promotions, HQ's
// JSON passed through; a 503 rollout_actuator_not_configured is an *HQError
// (final), a 502 github_unavailable a *RetryableError.
func (c *Client) ProposeRolloutPromotion(ctx context.Context, instance string, p RolloutPromotion) ([]byte, error) {
	body, err := json.Marshal(struct {
		Instance   string          `json:"instance"`
		Repo       string          `json:"repo"`
		PreviewTag string          `json:"preview_tag"`
		Evidence   json.RawMessage `json:"evidence"`
	}{instance, p.Repo, p.PreviewTag, p.Evidence})
	if err != nil {
		return nil, fmt.Errorf("officers: encode request: %w", err)
	}
	return c.doCoded(ctx, http.MethodPost, c.itemPath(p.Item, "rollout-promotions"), "application/json", body, rolloutDecisionCodes, rolloutRetryableCodes)
}
