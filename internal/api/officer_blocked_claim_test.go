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
func TestClaimingABlockedItemSaysClaimAnother(t *testing.T) {
	r := newOfficerRig(t)
	one := r.addInstance(r.a, 1, 961)
	item := r.hq.AddItem(offA, "engineering.task", "task:a")
	r.hq.SetBlocked(item, "T7", "T9")
	out := r.s.dispatchAgentTool(context.Background(), one, officers.ToolClaim, `{"item_id":"`+item+`"}`)
	if isToolErr(out) {
		t.Fatalf("claim on a blocked item: %s, want a non-error result", out)
	}
	var res struct {
		Claimed   *bool    `json:"claimed"`
		Blocked   bool     `json:"blocked"`
		BlockedBy []string `json:"blocked_by"`
		Note      string   `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %s", out)
	}
	if res.Claimed == nil || *res.Claimed || !res.Blocked || strings.Join(res.BlockedBy, ",") != "T7,T9" {
		t.Fatalf("result = %s, want claimed:false, blocked:true, blocked_by:[T7,T9]", out)
	}
	if want := "blocked by T7, T9; claim another open item"; !strings.Contains(res.Note, want) {
		t.Fatalf("note = %q, want it to contain %q", res.Note, want)
	}
	if it := r.hq.Item(item); it.Status != "open" || it.ClaimedBy != "" || it.Attempts != 0 {
		t.Fatalf("a refused claim changed the item: %+v", it)
	}
}
