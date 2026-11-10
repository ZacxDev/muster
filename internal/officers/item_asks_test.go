package officers

import "testing"

// The HQ-derived ask tools' offer rule and argument decoding (item_asks.go).
// Names and types are literals: they are the contract the charters name.

func TestItemAskToolsAreOfferedByTypeName(t *testing.T) {
	for _, c := range []struct {
		id         string
		task, flood bool
	}{
		{"task-reconciler:hq", true, false},
		{"web-performance:civitai-web", false, true},
		{"web-performance:other", false, true},
		{"release:alpha-web", false, false},
		{"orchestration:fleet", false, false},
		{"web-performance-x:civitai-web", false, false},
		{"x-task-reconciler:hq", false, false},
	} {
		for _, tier := range []string{"", TierRoutine, TierJudgement} {
			o := Officer{ID: c.id, Tier: tier}
			if got := ToolOffered(o, "officer_task_ask"); got != c.task {
				t.Errorf("ToolOffered(%s %q, officer_task_ask) = %v, want %v", c.id, tier, got, c.task)
			}
			if got := ToolOffered(o, "officer_flood_block_propose"); got != c.flood {
				t.Errorf("ToolOffered(%s %q, officer_flood_block_propose) = %v, want %v", c.id, tier, got, c.flood)
			}
		}
	}
}

func TestParseFloodBlockProposalKeepsTheNumberAsGiven(t *testing.T) {
	p, err := ParseFloodBlockProposal([]byte(`{"item_id":42,"unit_ids":["prefix:b","prefix:a"],"ttl_hours":0.25,"to":"perf-lead"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Item != "42" || p.TTLHours.String() != "0.25" || p.To != "perf-lead" || len(p.UnitIDs) != 2 || p.UnitIDs[0] != "prefix:b" {
		t.Fatalf("got %+v", p)
	}
}
