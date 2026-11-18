package officers_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// TestCompleteDroppingCostRetriesOnlyA400: a costed complete HQ answers 400
// is sent once more without the cost (the positive control: exactly two
// requests). Every other failure — a 409, a 5xx, a transport error — is the
// complete's own answer, not the cost's: exactly ONE request, and the error
// is returned as is.
func TestCompleteDroppingCostRetriesOnlyA400(t *testing.T) {
	ctx := context.Background()
	inst := routineStem + "1"
	cost := &officers.ItemCost{USD: 0.0428, From: officers.CostFromLLMProxy}
	completes := func(hq *hqtest.Server) int {
		n := 0
		for _, q := range hq.Requests() {
			if strings.HasSuffix(q.Path, "/complete") {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		name   string
		status int // RefuseCompletes; 0: no lease (HQ's 409 lease_lost)
		want   int
	}{
		{"400 (the control)", 400, 2},
		{"409 lease_lost", 0, 1},
		{"503", 503, 1},
	} {
		hq := hqtest.New(map[string]string{tierTok: tierID})
		o := mustTiered(t)
		o.HQURL = hq.BaseURL()
		c := officers.NewClient(o, nil)
		item := hq.AddTieredItem(tierID, "gpu.check", "node-retry", officers.TierRoutine)
		if tc.status != 0 {
			if _, err := c.Claim(ctx, officers.ItemID(item), inst, 60, officers.TierRoutine); err != nil {
				t.Fatalf("%s: claim: %v", tc.name, err)
			}
			hq.RefuseCompletes(tc.status, "forced", "refused by the test")
		}
		hq.ResetRequests()
		_, refused, err := officers.CompleteDroppingCost(ctx, c, officers.ItemID(item), inst, officers.Completion{Outcome: "done", Summary: "checked", Cost: cost})
		if err == nil || refused != nil {
			t.Errorf("%s: err %v, refused %v; want the complete's error and no dropped-cost refusal", tc.name, err, refused)
		}
		if n := completes(hq); n != tc.want {
			t.Errorf("%s: %d complete requests reached HQ, want %d", tc.name, n, tc.want)
		}
		hq.Close()
	}

	// A transport error: the connection is cut before any answer.
	var mu sync.Mutex
	n := 0
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(cut.Close)
	o := mustTiered(t)
	o.HQURL = cut.URL
	_, refused, err := officers.CompleteDroppingCost(ctx, officers.NewClient(o, nil), "7", inst, officers.Completion{Outcome: "done", Summary: "checked", Cost: cost})
	mu.Lock()
	got := n
	mu.Unlock()
	if !errors.Is(err, officers.ErrHQUnavailable) || refused != nil || got != 1 {
		t.Errorf("a cut connection: err %v, refused %v, %d requests; want ErrHQUnavailable after exactly 1 request", err, refused, got)
	}
}

// TestCompleteSendsTheItemCostAsHQNamesIt:Client.Complete with a cost sends
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
