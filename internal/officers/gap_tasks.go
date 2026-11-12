package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// --- Gap tasks: ONE open HQ task per (officer, store, gap kind) -------------
//
// The datastore officer's operator decision of 2026-11-04: "report + one
// task per gap", filed in shadow too. The instance holding an item names a
// store and a gap kind; HQ files the task from its own template, assigned to
// the officer's owner, and answers the gap's OPEN task on a repeat (once that
// task is closed, the next filing files a new one):
//
//	POST /officers/{id}/items/{item}/gap-tasks  {"instance", "store", "kind", "namespace"?, "tier"?, "affected"?}
//
// The route contract — the closed kind list, the slug patterns, the caps,
// what the task says, its refusal codes — is HQ's (civ-hq docs/OFFICERS.md
// §5o). It is not copied here: a copy would drift. Unlike an ask, a filing
// does NOT end the instance's lease: it keeps working the item.
//
// WHO MAY CALL: muster offers the tool by officer TYPE NAME (ToolOffered:
// datastore, both pools — the daily audit is a routine item); HQ enforces
// the type POLICY (gap_tasks) and the lease. muster owns the body's SHAPE:
// it builds the body itself, so the arguments are decoded STRICTLY — an
// argument muster does not know is refused before HQ is called.

// ToolGapTask is a datastore officer's: file one gap task.
const ToolGapTask = "officer_gap_task"

// DatastoreOfficerType is the officer type whose instances file gap tasks.
const DatastoreOfficerType = "datastore"

// GapTaskArgs is officer_gap_task's arguments: {item_id, store, kind,
// namespace?, tier?, affected?}.
type GapTaskArgs struct {
	Item      ItemID
	Store     string
	Kind      string
	Namespace string
	Tier      string
	Affected  []string
}

// ParseGapTaskArgs decodes officer_gap_task's arguments strictly. It checks
// TYPES and presence only: the values (which kinds, the slug patterns, the
// caps) are HQ's to refuse.
func ParseGapTaskArgs(raw []byte) (GapTaskArgs, error) {
	obj, err := strictObject(raw, ToolGapTask, "{item_id, store, kind, namespace?, tier?, affected?}",
		"item_id", "store", "kind", "namespace", "tier", "affected")
	if err != nil {
		return GapTaskArgs{}, err
	}
	item, err := itemArg(obj)
	if err != nil {
		return GapTaskArgs{}, err
	}
	a := GapTaskArgs{Item: item}
	for _, f := range []struct {
		key      string
		dst      *string
		required bool
	}{{"store", &a.Store, true}, {"kind", &a.Kind, true}, {"namespace", &a.Namespace, false}, {"tier", &a.Tier, false}} {
		v, ok := obj[f.key]
		if !ok || string(bytes.TrimSpace(v)) == "null" {
			if f.required {
				return GapTaskArgs{}, fmt.Errorf("%s is required", f.key)
			}
			continue
		}
		if json.Unmarshal(v, f.dst) != nil {
			return GapTaskArgs{}, fmt.Errorf("%s must be a string", f.key)
		}
		if f.required && strings.TrimSpace(*f.dst) == "" {
			return GapTaskArgs{}, fmt.Errorf("%s is required", f.key)
		}
	}
	if v, ok := obj["affected"]; ok && string(bytes.TrimSpace(v)) != "null" {
		if json.Unmarshal(v, &a.Affected) != nil {
			return GapTaskArgs{}, fmt.Errorf("affected must be an array of store slugs (strings)")
		}
	}
	return a, nil
}

// FileGapTask is POST …/items/{item}/gap-tasks, HQ's JSON passed through.
func (c *Client) FileGapTask(ctx context.Context, instance string, a GapTaskArgs) ([]byte, error) {
	return c.Do(ctx, http.MethodPost, c.itemPath(a.Item, "gap-tasks"), struct {
		Instance  string   `json:"instance"`
		Store     string   `json:"store"`
		Kind      string   `json:"kind"`
		Namespace string   `json:"namespace,omitempty"`
		Tier      string   `json:"tier,omitempty"`
		Affected  []string `json:"affected,omitempty"`
	}{instance, a.Store, a.Kind, a.Namespace, a.Tier, a.Affected})
}
