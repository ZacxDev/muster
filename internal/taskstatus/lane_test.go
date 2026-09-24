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
	for _, bad := range []string{"", "Open", "in-progress", "complete", "done "} {
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
	if len(got) != 2 {
		t.Fatalf("LaneStatuses(in_progress) = %v, want 2 entries", got)
	}
	got[0] = "clobbered"
	if again := LaneStatuses(LaneInProgress); again[0] == "clobbered" {
		t.Fatalf("LaneStatuses returned the package's own slice — a caller mutated the lane table")
	}
}

// TestLanesIsInLifecycleOrderAndACopy: the chip row renders in this order, and
// Go map iteration is randomised, so the order must come from the array.
func TestLanesIsInLifecycleOrderAndACopy(t *testing.T) {
	got := Lanes()
	want := []string{LaneOpen, LaneInProgress, LaneDone}
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
// text, which is unclickable in practice and invisible to a screen reader.
func TestEveryLaneHasALabel(t *testing.T) {
	for _, lane := range Lanes() {
		if LaneLabel(lane) == "" {
			t.Errorf("lane %q has no label — its chip would render with no text", lane)
		}
	}
	if LaneLabel("nope") != "" {
		t.Errorf("LaneLabel of an unknown lane must be empty")
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
