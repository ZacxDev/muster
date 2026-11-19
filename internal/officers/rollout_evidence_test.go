package officers

import "testing"

// TestTheRolloutEvidenceToolIsOfferedToTheRolloutRoutinePoolOnly:
// officer_rollout_evidence_attach to a rollout officer's ROUTINE pool and an
// untiered one — HQ tiers the hourly sweep, the item that attaches, routine —
// never its judgement pool, and to no other type, a look-alike prefix or
// suffix included. Its capability is rollout-evidence.
func TestTheRolloutEvidenceToolIsOfferedToTheRolloutRoutinePoolOnly(t *testing.T) {
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
		{"datastore:example", TierRoutine, false},
		{"task-reconciler:hq", TierRoutine, false},
		{"release:alpha-web", TierJudgement, false},
		{"rollout-x:example", TierRoutine, false},
		{"x-rollout:example", TierRoutine, false},
		{"rollouts:example", TierRoutine, false},
	} {
		o := Officer{ID: c.id, Tier: c.tier}
		if got := ToolOffered(o, "officer_rollout_evidence_attach"); got != c.want {
			t.Errorf("ToolOffered(%s %q, officer_rollout_evidence_attach) = %v, want %v", c.id, c.tier, got, c.want)
		}
	}
	// A host officer is offered the queue tools only, whatever its type.
	host := Officer{ID: "rollout:example", Backend: BackendBuiltin, Runtime: RuntimeHost, Host: "host-alpha", Repo: "example-org/example-site"}
	if ToolOffered(host, "officer_rollout_evidence_attach") {
		t.Error("a host rollout officer is offered officer_rollout_evidence_attach")
	}
	if c, known := ToolCapability("officer_rollout_evidence_attach"); !known || c != "rollout-evidence" {
		t.Errorf("ToolCapability(officer_rollout_evidence_attach) = %q, %v; want rollout-evidence, true", c, known)
	}
}

// TestParseRolloutEvidenceKeepsTheEvidenceAsGiven: the evidence object is
// forwarded byte-for-byte, never re-shaped (HQ's decoder owns its fields).
func TestParseRolloutEvidenceKeepsTheEvidenceAsGiven(t *testing.T) {
	ev := `{"summary":"s1","checks":[{"name":"n1","query":"q1","result":"r1"}],"extra_hq_field":3}`
	e, err := ParseRolloutEvidence([]byte(`{"item_id":42,"repo":"example-org/ctl","tag":"v1.2.3-preview.4","evidence":` + ev + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Item != "42" || e.Repo != "example-org/ctl" || e.Tag != "v1.2.3-preview.4" || string(e.Evidence) != ev {
		t.Fatalf("got %+v (evidence %s)", e, e.Evidence)
	}
}
