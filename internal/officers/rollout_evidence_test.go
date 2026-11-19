package officers

import (
	"strings"
	"testing"
)

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

// TestTheRetiredRolloutProposalIsNoTool: officer_rollout_promote_propose
// (HQ answers its route 410 officer_propose_retired) is unclassified — so
// ToolServed never serves it — and offered to no officer, the rollout
// officer's routine pool included.
func TestTheRetiredRolloutProposalIsNoTool(t *testing.T) {
	const retired = "officer_rollout_promote_propose"
	if c, known := ToolCapability(retired); known {
		t.Errorf("ToolCapability(%s) = %q, known; want unknown", retired, c)
	}
	for _, tier := range []string{"", TierRoutine, TierJudgement} {
		o := Officer{ID: "rollout:example", Tier: tier}
		if ToolOffered(o, retired) || ToolServed(o, hqCapabilities, retired) {
			t.Errorf("rollout:example %q is offered or served %s", tier, retired)
		}
	}
	// Positive control: the same officer IS served the new tool, so the
	// falses above are the retirement's, not a dead officer.
	if !ToolServed(Officer{ID: "rollout:example", Tier: TierRoutine}, hqCapabilities, ToolRolloutEvidenceAttach) {
		t.Fatal("control: the rollout routine pool is not served officer_rollout_evidence_attach")
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

// TestParseRolloutEvidenceRefusesTheRetiredShape: the old proposal's
// preview_tag is not an argument (the field is tag), so a caller still on
// the retired shape is told so instead of HQ's 400.
func TestParseRolloutEvidenceRefusesTheRetiredShape(t *testing.T) {
	_, err := ParseRolloutEvidence([]byte(`{"item_id":42,"repo":"example-org/ctl","preview_tag":"v1.2.3-preview.4","evidence":{"summary":"s","checks":[]}}`))
	if err == nil || !strings.Contains(err.Error(), `"preview_tag" is not an argument of officer_rollout_evidence_attach`) {
		t.Fatalf("err = %v, want preview_tag refused by name", err)
	}
}
