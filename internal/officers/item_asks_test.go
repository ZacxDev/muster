package officers

import "testing"

// The HQ-derived ask tools' offer rule and argument decoding (item_asks.go).
// Names and types are literals: they are the contract the charters name.

// TestItemAskToolsAreOfferedByTypeName: officer_task_ask to both pools (and
// an untiered officer) of a task-reconciler; officer_flood_block_propose to a
// web-performance officer's JUDGEMENT pool only (operator decision on #565):
// never its routine pool, never an untiered web-performance officer.
func TestItemAskToolsAreOfferedByTypeName(t *testing.T) {
	for _, c := range []struct {
		id    string
		tier  string
		task  bool
		flood bool
	}{
		{"task-reconciler:hq", "", true, false},
		{"task-reconciler:hq", TierRoutine, true, false},
		{"task-reconciler:hq", TierJudgement, true, false},
		{"web-performance:alpha-web", TierJudgement, false, true},
		{"web-performance:other", TierJudgement, false, true},
		{"web-performance:alpha-web", TierRoutine, false, false},
		{"web-performance:alpha-web", "", false, false},
		{"release:alpha-web", TierJudgement, false, false},
		{"orchestration:fleet", TierJudgement, false, false},
		{"web-performance-x:alpha-web", TierJudgement, false, false},
		{"x-task-reconciler:hq", TierRoutine, false, false},
	} {
		o := Officer{ID: c.id, Tier: c.tier}
		if got := ToolOffered(o, "officer_task_ask"); got != c.task {
			t.Errorf("ToolOffered(%s %q, officer_task_ask) = %v, want %v", c.id, c.tier, got, c.task)
		}
		if got := ToolOffered(o, "officer_flood_block_propose"); got != c.flood {
			t.Errorf("ToolOffered(%s %q, officer_flood_block_propose) = %v, want %v", c.id, c.tier, got, c.flood)
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

// TestTheRolloutToolIsOfferedToTheRolloutTypeOnly: officer_rollout_promote_propose
// to a rollout officer's ROUTINE pool and an untiered one — HQ tiers the
// sweep that proposes routine — never its judgement pool (which only
// escalates failed results), and to no other type, a look-alike prefix or
// suffix included.
func TestTheRolloutToolIsOfferedToTheRolloutTypeOnly(t *testing.T) {
	for _, c := range []struct {
		id   string
		tier string
		want bool
	}{
		{"rollout:example", "", true},
		{"rollout:example", TierRoutine, true},
		{"rollout:example", TierJudgement, false},
		{"rollout:other", TierRoutine, true},
		{"web-performance:alpha-web", TierJudgement, false},
		{"task-reconciler:hq", TierRoutine, false},
		{"release:alpha-web", TierJudgement, false},
		{"rollout-x:example", TierRoutine, false},
		{"x-rollout:example", TierRoutine, false},
		{"rollouts:example", TierRoutine, false},
	} {
		o := Officer{ID: c.id, Tier: c.tier}
		if got := ToolOffered(o, "officer_rollout_promote_propose"); got != c.want {
			t.Errorf("ToolOffered(%s %q, officer_rollout_promote_propose) = %v, want %v", c.id, c.tier, got, c.want)
		}
	}
	// A host officer is offered the queue tools only, whatever its type.
	host := Officer{ID: "rollout:example", Backend: BackendBuiltin, Runtime: RuntimeHost, Host: "host-alpha", Repo: "example-org/example-site"}
	if ToolOffered(host, "officer_rollout_promote_propose") {
		t.Error("a host rollout officer is offered officer_rollout_promote_propose")
	}
	if c, known := ToolCapability("officer_rollout_promote_propose"); !known || c != "rollout-promotions" {
		t.Errorf("ToolCapability(officer_rollout_promote_propose) = %q, %v; want rollout-promotions, true", c, known)
	}
}

// TestParseRolloutPromotionKeepsTheEvidenceAsGiven: the evidence object is
// forwarded byte-for-byte (compacted by the encoder only), never re-shaped.
func TestParseRolloutPromotionKeepsTheEvidenceAsGiven(t *testing.T) {
	ev := `{"summary":"s1","checks":[{"name":"n1","query":"q1","result":"r1"}],"extra_hq_field":3}`
	p, err := ParseRolloutPromotion([]byte(`{"item_id":42,"repo":"example-org/ctl","preview_tag":"v1.2.3-preview.4","evidence":` + ev + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Item != "42" || p.Repo != "example-org/ctl" || p.PreviewTag != "v1.2.3-preview.4" || string(p.Evidence) != ev {
		t.Fatalf("got %+v (evidence %s)", p, p.Evidence)
	}
}
