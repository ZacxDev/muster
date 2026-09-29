package taskstatus

import (
	"sort"
	"testing"
)

// TestEveryStatusBelongsToExactlyOneLane is the guard that makes the lane table
// safe to add a status to.
//
// 🔴 It is a RELATIONSHIP guard, and both halves matter. A status in NO lane is
// invisible under every filter chip — including the ones a user reads as
// covering the whole board — so it would silently disappear from the queue. A
// status in TWO lanes double-counts and makes "which chip is this card under"
// unanswerable. Neither is a compile error, and neither is visible from any
// single lane's own test.
func TestEveryStatusBelongsToExactlyOneLane(t *testing.T) {
	for _, s := range All() {
		hits := []string{}
		for _, lane := range Lanes() {
			for _, m := range LaneStatuses(lane) {
				if m == s {
					hits = append(hits, lane)
				}
			}
		}
		if len(hits) != 1 {
			t.Errorf("status %q belongs to %d lanes (%v), want exactly 1 — a status in no lane is "+
				"hidden from every filter chip; a status in two makes the chips double-count", s, len(hits), hits)
		}
		if got := LaneOf(s); len(hits) == 1 && got != hits[0] {
			t.Errorf("LaneOf(%q) = %q, want %q", s, got, hits[0])
		}
	}
}

// TestNoLaneCoversAStatusOutsideTheVocabulary is the other direction: a lane
// naming a status that Valid() rejects would produce a SQL predicate matching
// nothing, i.e. a chip that always renders an empty board.
func TestNoLaneCoversAStatusOutsideTheVocabulary(t *testing.T) {
	for _, lane := range Lanes() {
		for _, s := range LaneStatuses(lane) {
			if !Valid(s) {
				t.Errorf("lane %q covers %q, which is not a valid status — that chip can only ever "+
					"render an empty board", lane, s)
			}
		}
	}
}

// TestLaneStatusesForAnUnknownLaneIsNilNotEmpty pins the degradation direction.
// nil means "no status predicate" (show everything); an empty non-nil slice
// would build `status = ANY('{}')`, which matches NOTHING — a typo'd ?status=
// would then empty the board while the chip row showed nothing selected.
func TestLaneStatusesForAnUnknownLaneIsNilNotEmpty(t *testing.T) {
	// `done` is in this list deliberately: it was the OLD lane value for
	// Complete, so a stale bookmark or localStorage entry still sends it. It must
	// degrade to the unfiltered board, not to an empty one.
	for _, bad := range []string{"", "Open", "in-progress", "done", "complete "} {
		if got := LaneStatuses(bad); got != nil {
			t.Errorf("LaneStatuses(%q) = %#v, want nil (no predicate) — a non-nil empty slice "+
				"would match no rows and silently empty the board", bad, got)
		}
		if ValidLane(bad) {
			t.Errorf("ValidLane(%q) = true, want false", bad)
		}
	}
}

// TestLaneStatusesReturnsACopy: the slice is handed straight to pgx as a query
// argument, and an appending caller would corrupt the table for the process.
func TestLaneStatusesReturnsACopy(t *testing.T) {
	got := LaneStatuses(LaneInProgress)
	if len(got) != 1 {
		t.Fatalf("LaneStatuses(in_progress) = %v, want 1 entry", got)
	}
	got[0] = "clobbered"
	if again := LaneStatuses(LaneInProgress); again[0] == "clobbered" {
		t.Fatalf("LaneStatuses returned the package's own slice — a caller mutated the lane table")
	}
}

// TestTheLaneVOCABULARYISTheStatusVOCABULARY is the regression guard for the
// measured defect: the filter chips and the per-card status <select> were two
// enumerations of one thing and had drifted.
//
// 🔴 ASSERTED AGAINST All(), NOT AGAINST LITERAL STRINGS. A test listing
// "open", "in_progress", "ready_for_review", "complete" would pass a rename of
// one enum member in the other, which is the same two-copies shape that produced
// the bug. Comparing the two SETS is the only form that cannot drift.
//
// What it refuses, concretely: a lane covering more than one status (that is how
// ready_for_review became unreachable — folded under "In progress", where 30 of
// the 50 loaded cards carried a status no chip named), and a status with no lane
// of its own.
func TestTheLaneVocabularyIsTheStatusVocabulary(t *testing.T) {
	lanes, statuses := Lanes(), All()
	if len(lanes) != len(statuses) {
		t.Fatalf("Lanes() = %v (%d), All() = %v (%d) — every status must be reachable by exactly "+
			"one chip, so the two vocabularies must be the same size", lanes, len(lanes), statuses, len(statuses))
	}
	for i := range statuses {
		if lanes[i] != statuses[i] {
			t.Fatalf("Lanes() = %v, All() = %v — a lane that is not a status (or in a different "+
				"lifecycle order) means a chip the board cannot explain", lanes, statuses)
		}
		if got := LaneStatuses(statuses[i]); len(got) != 1 || got[0] != statuses[i] {
			t.Errorf("LaneStatuses(%q) = %v, want exactly [%q] — a lane covering two statuses hides "+
				"one of them behind the other's chip label", statuses[i], got, statuses[i])
		}
	}
}

// TestLanesIsInLifecycleOrderAndACopy: the chip row renders in this order, and
// Go map iteration is randomised, so the order must come from the vocabulary.
func TestLanesIsInLifecycleOrderAndACopy(t *testing.T) {
	got := Lanes()
	want := []string{LaneOpen, LaneInProgress, LaneReadyForReview, LaneDone}
	if len(got) != len(want) {
		t.Fatalf("Lanes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lanes() = %v, want %v (lifecycle order — a chip row that reshuffles between "+
				"renders is a usability bug)", got, want)
		}
	}
	got[0] = "clobbered"
	if Lanes()[0] != LaneOpen {
		t.Fatalf("Lanes returned the package's own array")
	}
}

// TestEveryLaneHasALabel: a lane added without copy would render a chip with no
// text, which is unclickable in practice and invisible to a screen reader. Since
// a lane IS a status, this is also the check that adding a status to the
// vocabulary cannot ship a blank chip.
func TestEveryLaneHasALabel(t *testing.T) {
	for _, lane := range Lanes() {
		if LaneLabel(lane) == "" {
			t.Errorf("lane %q has no label — its chip would render with no text. Add it to "+
				"laneLabels in lane.go", lane)
		}
	}
	if LaneLabel("nope") != "" {
		t.Errorf("LaneLabel of an unknown lane must be empty")
	}
}

// TestLaneLabelsAreDistinct: two lanes sharing copy is worse than a missing
// label — the row renders two chips reading the same word and the user cannot
// tell which state either one selects. It is the shape the old table had
// (ready_for_review had no chip of its own at all).
func TestLaneLabelsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, lane := range Lanes() {
		l := LaneLabel(lane)
		if prev, dup := seen[l]; dup {
			t.Errorf("lanes %q and %q both render as %q — two chips with one label", prev, lane, l)
		}
		seen[l] = lane
	}
}

// TestLaneOfRejectsUnknownStatuses guards the direction that reads as "handled".
func TestLaneOfRejectsUnknownStatuses(t *testing.T) {
	for _, bad := range []string{"", "OPEN", "in progress", "done", "archived"} {
		if got := LaneOf(bad); got != "" {
			t.Errorf("LaneOf(%q) = %q, want \"\"", bad, got)
		}
	}
}

// TestLaneMembersPartitionsTheVocabulary is the count check the per-status loop
// above cannot make: it proves the lanes cover the vocabulary and nothing else,
// so a status can neither be dropped from the table nor invented in it.
func TestLaneMembersPartitionsTheVocabulary(t *testing.T) {
	covered := []string{}
	for _, lane := range Lanes() {
		covered = append(covered, LaneStatuses(lane)...)
	}
	want := All()
	sort.Strings(covered)
	sort.Strings(want)
	if len(covered) != len(want) {
		t.Fatalf("lanes cover %d statuses (%v), vocabulary has %d (%v)", len(covered), covered, len(want), want)
	}
	for i := range want {
		if covered[i] != want[i] {
			t.Fatalf("lanes cover %v, vocabulary is %v", covered, want)
		}
	}
}
