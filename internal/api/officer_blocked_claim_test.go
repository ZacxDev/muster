package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestClaimingABlockedItemSaysClaimAnother: HQ refuses a claim on an item
// whose task waits on unfinished tasks with 409 {code: blocked, blocked_by}.
// That is not a failure of the instance's call and not "another instance has
// it": the instance is told, as a non-error result, which tasks block the item
// and to claim another open item. Nothing about the item changes.
//
// blocked_by holds TASK ids (HQ's, non-numeric strings), never officer item
// ids: the note must not read as if they were items to claim.
func TestClaimingABlockedItemSaysClaimAnother(t *testing.T) {
	r := newOfficerRig(t)
	one := r.addInstance(r.a, 1, 961)
	item := r.hq.AddItem(offA, "engineering.task", "task:a")
	r.hq.SetBlocked(item, "officers_ui_v2_xpf4", "billing_split_k2m9")
	out := r.s.dispatchAgentTool(context.Background(), one, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	if isToolErr(out) {
		t.Fatalf("claim on a blocked item: %s, want a non-error result", out)
	}
	var res struct {
		Claimed   *bool             `json:"claimed"`
		Blocked   bool              `json:"blocked"`
		BlockedBy []json.RawMessage `json:"blocked_by"`
		Note      string            `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %s", out)
	}
	if res.Claimed == nil || *res.Claimed || !res.Blocked {
		t.Fatalf("result = %s, want claimed:false, blocked:true", out)
	}
	// The ids are carried as JSON strings, verbatim.
	var by []string
	for _, raw := range res.BlockedBy {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			t.Fatalf("blocked_by entry %s is not a JSON string (result %s)", raw, out)
		}
		by = append(by, id)
	}
	if strings.Join(by, ",") != "officers_ui_v2_xpf4,billing_split_k2m9" {
		t.Fatalf("blocked_by = %v, want [officers_ui_v2_xpf4 billing_split_k2m9]", by)
	}
	if want := "not claimed: item " + item + " waits on tasks officers_ui_v2_xpf4, billing_split_k2m9 (not done yet); claim another open item"; !strings.HasPrefix(res.Note, want) {
		t.Fatalf("note = %q, want it to start %q", res.Note, want)
	}
	if strings.Contains(res.Note, "blocked by") {
		t.Fatalf("note = %q: the blockers are tasks, not items; it must not say \"blocked by\"", res.Note)
	}
	if it := r.hq.Item(item); it.Status != "open" || it.ClaimedBy != "" || it.Attempts != 0 {
		t.Fatalf("a refused claim changed the item: %+v", it)
	}
}
