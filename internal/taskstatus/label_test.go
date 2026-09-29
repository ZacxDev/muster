package taskstatus

import "testing"

// 🔴 WHAT WAS DELETED WITH lane.go, AND WHY IT IS NOT A LOSS OF COVERAGE.
// lane_test.go carried ten tests. Eight of them tested functions that no longer
// exist — TestEveryStatusBelongsToExactlyOneLane, TestNoLaneCoversAStatusOutside
// TheVocabulary, TestLaneStatusesForAnUnknownLaneIsNilNotEmpty, TestLaneStatuses
// ReturnsACopy, TestTheLaneVocabularyIsTheStatusVocabulary, TestLanesIsIn
// LifecycleOrderAndACopy, TestLaneMembersPartitionsTheVocabulary and
// TestLaneOfRejectsUnknownStatuses — and every one of them had become a
// statement about an identity function once a lane was required to be a status.
//
// The HEADLINE defect they were written for — the chip set disagreeing with the
// status enum — is pinned by TestTheStatusChipSetIsTheStatusEnum in internal/ui,
// which asserts the RENDERED chips against All(). That was verified before the
// deletion rather than assumed: it was watched to fail at 8700b02, reporting
// `status chips are [ done in_progress open], want [ complete in_progress open
// ready_for_review]`. A guard that reds on the real defect is what makes eight
// identity assertions safe to remove.
//
// ONE property did NOT move to internal/ui and so did not survive here: "an
// unknown ?status= degrades to the whole board, never to an empty one". It left
// with LaneStatuses, and it is now pinned where the rule actually lives — see
// TestAnUnknownStatusFilterShowsEverything in internal/api.
//
// The two below are kept because neither is an identity: a label is real data
// that a status can be added without.

// TestEveryStatusHasALabel: the chip row renders one chip per All() entry, so a
// status added to the vocabulary without copy ships a control with no text —
// unclickable in practice and invisible to a screen reader.
func TestEveryStatusHasALabel(t *testing.T) {
	for _, s := range All() {
		if Label(s) == "" {
			t.Errorf("status %q has no label — its filter chip would render with no text. "+
				"Add it to `labels` in label.go", s)
		}
	}
	if Label("nope") != "" {
		t.Errorf("Label of an unknown status must be empty — the filtered-empty state uses that " +
			"to decide whether to name a status filter at all")
	}
}

// TestLabelsAreDistinct: two statuses sharing copy is worse than a missing label
// — the row renders two chips reading the same word and the user cannot tell
// which state either one selects. It is the shape the deleted lane table had, in
// the one form the collapse does not make impossible.
func TestLabelsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, s := range All() {
		l := Label(s)
		if prev, dup := seen[l]; dup {
			t.Errorf("statuses %q and %q both render as %q — two chips with one label", prev, s, l)
		}
		seen[l] = s
	}
}
