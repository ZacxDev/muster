package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// claimResult is officer_claim's answer: HQ's {item, lease_expires_at}, or
// {item: null, claimed: false, note} when nothing is claimable.
type claimResult struct {
	Item *struct {
		ID        officers.ItemID `json:"id"`
		Status    string          `json:"status"`
		ClaimedBy string          `json:"claimed_by"`
	} `json:"item"`
	Claimed *bool  `json:"claimed"`
	Note    string `json:"note"`
}

func decodeClaim(t *testing.T, out string) claimResult {
	t.Helper()
	if isToolErr(out) {
		t.Fatalf("officer_claim: %s, want a result", out)
	}
	var r claimResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("officer_claim result is not JSON: %s", out)
	}
	return r
}

// waitNote is officer_claim's whole note when nothing is claimable, pinned
// literally (the instance acts on it).
const waitNote = "nothing to claim right now: every open item is held, blocked or not due. Stop and wait — muster wakes you when HQ has new work."

// claimPaths is the method and path of every claim-shaped request HQ saw.
func (r *officerRig) claimPaths() []string {
	var out []string
	for _, q := range r.hq.Requests() {
		if q.Method == "POST" {
			out = append(out, q.Method+" "+q.Path)
		}
	}
	return out
}

// TestOfficerClaimWithNoItemTakesTheNext: on an HQ with claim-next,
// officer_claim{} (no item_id) asks HQ for the next item and answers it —
// the NEWER high item ahead of the older normal one, then the normal one —
// claimed for the calling instance; with nothing claimable it answers item
// null, claimed false and a note to wait, not an error. Each call is ONE
// POST to /claim-next carrying the instance, and no list read.
func TestOfficerClaimWithNoItemTakesTheNext(t *testing.T) {
	r := newOfficerRig(t)
	r.hq.ClaimNext = true
	one := r.addInstance(r.a, 1, 971)
	older := r.hq.AddItem(offA, "engineering.task", "task:older")
	newer := r.hq.AddItem(offA, "engineering.task", "task:newer")
	r.hq.SetPriority(newer, "high")
	for _, want := range []officers.ItemID{officers.ItemID(newer), officers.ItemID(older)} {
		r.hq.ResetRequests()
		res := decodeClaim(t, r.s.dispatchAgentTool(context.Background(), one, officers.ToolClaim, `{}`))
		if res.Item == nil || res.Item.ID != want || res.Item.Status != "claimed" || res.Item.ClaimedBy != one.Name {
			t.Fatalf("officer_claim{}: %+v, want item %s claimed by %s", res.Item, want, one.Name)
		}
		reqs := r.hq.Requests()
		if len(reqs) != 1 || reqs[0].Method != "POST" || reqs[0].Path != "/api/v1/officers/"+offA+"/claim-next" || reqs[0].Body["instance"] != one.Name {
			t.Fatalf("requests %+v, want one POST claim-next naming the instance", reqs)
		}
		if _, ok := reqs[0].Body["tier"]; ok {
			t.Fatalf("an untiered officer's claim-next sent a tier: %+v", reqs[0].Body)
		}
	}
	res := decodeClaim(t, r.s.dispatchAgentTool(context.Background(), one, officers.ToolClaim, `{}`))
	if res.Item != nil || res.Claimed == nil || *res.Claimed || res.Note != waitNote {
		t.Fatalf("officer_claim{} with nothing claimable: %+v, want item null, claimed false and the wait note", res)
	}
}

// TestOfficerClaimWithAnItemClaimsThatItem: officer_claim{item_id} claims
// exactly that item — the older normal one, though a high one would come
// first — through POST …/items/{item}/claim, never claim-next.
func TestOfficerClaimWithAnItemClaimsThatItem(t *testing.T) {
	r := newOfficerRig(t)
	r.hq.ClaimNext = true
	one := r.addInstance(r.a, 1, 972)
	older := r.hq.AddItem(offA, "engineering.task", "task:older")
	newer := r.hq.AddItem(offA, "engineering.task", "task:newer")
	r.hq.SetPriority(newer, "high")
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(context.Background(), one, officers.ToolClaim, `{"item_id":"`+older+`"}`)
	if isToolErr(out) {
		t.Fatalf("officer_claim{item_id}: %s", out)
	}
	if it := r.hq.Item(older); it.Status != "claimed" || it.ClaimedBy != one.Name {
		t.Fatalf("item %s after officer_claim{item_id}: %+v, want claimed by %s", older, it, one.Name)
	}
	if it := r.hq.Item(newer); it.Status != "open" {
		t.Fatalf("the high item changed: %+v", it)
	}
	if got := r.claimPaths(); len(got) != 1 || got[0] != "POST /api/v1/officers/"+offA+"/items/"+older+"/claim" {
		t.Fatalf("POSTs %v, want only the item's claim", got)
	}
}

// TestOfficerClaimNextOn404SaysClaimByID: an HQ from before claim-next
// answers its route 404. officer_claim{} then answers the tool error naming
// the way that works there — officer_items, then officer_claim{item_id} —
// and muster picks nothing itself: no list read, no claim, the items
// untouched.
func TestOfficerClaimNextOn404SaysClaimByID(t *testing.T) {
	r := newOfficerRig(t)
	one := r.addInstance(r.a, 1, 973)
	b := r.hq.AddItem(offA, "engineering.task", "task:b")
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(context.Background(), one, officers.ToolClaim, `{}`)
	if !isToolErr(out) {
		t.Fatalf("officer_claim{} on an HQ without claim-next: %s, want a tool error", out)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil || e.Error != "this HQ has no claim-next; call officer_items and officer_claim{item_id}" {
		t.Fatalf("officer_claim{} on an HQ without claim-next: %s, want exactly the claim-by-id error", out)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 || reqs[0].Method != "POST" || reqs[0].Path != "/api/v1/officers/"+offA+"/claim-next" {
		t.Fatalf("requests %+v, want only the claim-next POST (no list, no claim)", reqs)
	}
	if it := r.hq.Item(b); it.Status != "open" || it.Attempts != 0 {
		t.Fatalf("the item changed: %+v", it)
	}
}
