package taskstatus

import "testing"

// Rank must be derived from the vocabulary, in lifecycle order, and must answer
// -1 — not 0 — for anything it does not know. 0 is Open's rank, so an unknown
// status ranking 0 would read as "the task was open", which is a claim.
func TestRankIsLifecycleOrderAndMinusOneForUnknown(t *testing.T) {
	want := map[string]int{Open: 0, InProgress: 1, ReadyForReview: 2, Complete: 3}
	for s, w := range want {
		if got := Rank(s); got != w {
			t.Errorf("Rank(%q) = %d, want %d", s, got, w)
		}
	}
	for _, s := range []string{"", "UNKNOWN", "done", "Open", "in-progress"} {
		if got := Rank(s); got != -1 {
			t.Errorf("Rank(%q) = %d, want -1 — an unknown status must not rank as a real one", s, got)
		}
	}
	// Rank must AGREE with All(): a status the vocabulary advertises but Rank
	// cannot place would be invisible to IsDowngrade.
	for i, s := range All() {
		if Rank(s) != i {
			t.Errorf("All()[%d]=%q ranks %d — Rank and All disagree, so a downgrade "+
				"involving %q would go unclassified", i, s, Rank(s), s)
		}
	}
}

func TestIsDowngrade(t *testing.T) {
	cases := []struct {
		old, next string
		want      bool
		why       string
	}{
		// 🔴 THE INCIDENT. Task 372: ready_for_review put back to in_progress.
		{ReadyForReview, InProgress, true, "the exact transition that ate an agent's completion signal"},
		{ReadyForReview, Open, true, "named in the acceptance criteria"},
		{Complete, InProgress, true, "named in the acceptance criteria"},
		{Complete, Open, true, "named in the acceptance criteria"},
		{Complete, ReadyForReview, true, "backwards, one step"},
		// Deliberately WIDER than the observed shape.
		{InProgress, Open, true, "same class of event, one rung lower"},
		// Forward moves and no-ops are not downgrades.
		{Open, InProgress, false, "the dispatch advance"},
		{InProgress, ReadyForReview, false, "an agent finishing"},
		{ReadyForReview, Complete, false, "the operator accepting"},
		{Open, Complete, false, "a jump forward is still forward"},
		{InProgress, InProgress, false, "a re-save is not a downgrade"},
		{ReadyForReview, ReadyForReview, false, "a re-save is not a downgrade"},
		// 🔴 An absent operand must report NOT-a-downgrade, never a downgrade.
		// The choke point passes "UNKNOWN" when it could not read the previous
		// status, and a WARNING manufactured out of a failed read is a false alarm
		// in the one log line this feature exists to make trustworthy.
		{"UNKNOWN", Open, false, "unreadable previous status must not fabricate a WARNING"},
		{"UNKNOWN", InProgress, false, "unreadable previous status must not fabricate a WARNING"},
		{ReadyForReview, "UNKNOWN", false, "an unknown target is unclassifiable, not a downgrade"},
		{"", "", false, "two unknowns rank equal at -1; must not compare as anything"},
		{Complete, "", false, "empty target is not a status"},
	}
	for _, c := range cases {
		if got := IsDowngrade(c.old, c.next); got != c.want {
			t.Errorf("IsDowngrade(%q, %q) = %v, want %v — %s", c.old, c.next, got, c.want, c.why)
		}
	}
}
