package officers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// TestCompleteSendsTheItemCostAsHQNamesIt: Client.Complete with a cost sends
// HQ's two fields, cost_usd and cost_from (civ-hq docs/OFFICERS.md "Item
// cost"), in the body HQ decodes; without one it sends NEITHER field — an
// unknown cost is omitted, never a 0 or a null. The bodies are pinned as
// literal JSON.
func TestCompleteSendsTheItemCostAsHQNamesIt(t *testing.T) {
	hq := hqtest.New(map[string]string{tierTok: tierID})
	t.Cleanup(hq.Close)
	o := mustTiered(t)
	o.HQURL = hq.BaseURL()
	c := officers.NewClient(o, nil)
	ctx := context.Background()
	inst := routineStem + "1"

	for _, tc := range []struct {
		name string
		cost *officers.ItemCost
		want string
	}{
		{"a known cost", &officers.ItemCost{USD: 0.0428, From: officers.CostFromLLMProxy},
			`{"cost_from":"muster:llm-proxy","cost_usd":0.0428,"instance":"` + inst + `","links":null,"outcome":"done","summary":"checked"}`},
		{"an unknown cost", nil,
			`{"instance":"` + inst + `","links":null,"outcome":"done","summary":"checked"}`},
	} {
		item := hq.AddTieredItem(tierID, "gpu.check", "node-"+strings.ReplaceAll(tc.name, " ", "-"), officers.TierRoutine)
		if _, err := c.Claim(ctx, officers.ItemID(item), inst, 60, officers.TierRoutine); err != nil {
			t.Fatalf("%s: claim: %v", tc.name, err)
		}
		hq.ResetRequests()
		if _, err := c.Complete(ctx, officers.ItemID(item), inst, officers.Completion{Outcome: "done", Summary: "checked", Cost: tc.cost}); err != nil {
			t.Fatalf("%s: complete: %v", tc.name, err)
		}
		reqs := hq.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%s: %d requests, want 1", tc.name, len(reqs))
		}
		if got := string(reqs[0].Raw); got != tc.want {
			t.Errorf("%s: complete body\n got %s\nwant %s", tc.name, got, tc.want)
		}
		got := hq.Item(item)
		if tc.cost == nil {
			if got.CostUSD != nil || got.CostFrom != nil {
				t.Errorf("%s: HQ stored cost %v from %v, want none", tc.name, got.CostUSD, got.CostFrom)
			}
			continue
		}
		if got.CostUSD == nil || *got.CostUSD != 0.0428 || got.CostFrom == nil || *got.CostFrom != "muster:llm-proxy" {
			t.Errorf("%s: HQ stored cost %v from %v, want 0.0428 from muster:llm-proxy", tc.name, got.CostUSD, got.CostFrom)
		}
	}
}
