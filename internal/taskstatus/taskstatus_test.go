package taskstatus

import "testing"

// TestValidAcceptsEveryAdvertisedStatus is the property the old cross-package
// drift guard was trying to buy, now available because there is only one
// definition: what All() advertises and what Valid() accepts cannot disagree.
func TestValidAcceptsEveryAdvertisedStatus(t *testing.T) {
	for _, s := range All() {
		if !Valid(s) {
			t.Fatalf("All() advertises %q but Valid rejects it", s)
		}
	}
	if len(All()) == 0 {
		t.Fatal("All() is empty — every caller's help text and validation would be vacuous")
	}
}

func TestValidRejectsNearMisses(t *testing.T) {
	// `dismissed` is the load-bearing one: it is documented as NOT a status
	// (dismissing soft-deletes the task instead of moving it), and a caller
	// reaching for it must be refused rather than quietly accepted.
	for _, bad := range []string{"", "dismissed", "in-progress", "readyforreview", "todo", "OPEN", " open"} {
		if Valid(bad) {
			t.Fatalf("Valid accepts %q, which is not a task status", bad)
		}
	}
}

// TestAllReturnsACopy pins the defence against a caller sorting or truncating
// the vocabulary for the whole process. Without it, `sort.Strings(All())` in one
// help string would reorder every other caller's error message.
func TestAllReturnsACopy(t *testing.T) {
	first := All()
	original := first[0]
	first[0] = "clobbered"

	if second := All(); second[0] != original {
		t.Fatalf("All()[0] = %q after a caller mutated a previous result, want %q — All must return a copy",
			second[0], original)
	}
}

// TestAllIsInLifecycleOrder: the order is load-bearing, because it is the order
// user-facing messages list the vocabulary in. Alphabetical would read as
// arbitrary to an operator.
func TestAllIsInLifecycleOrder(t *testing.T) {
	want := []string{Open, InProgress, ReadyForReview, Complete}
	got := All()
	if len(got) != len(want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("All()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestAllowedForAgentDeniesOnlyComplete pins the privilege split: agents move a
// task through the working states but may not declare it complete.
func TestAllowedForAgentDeniesOnlyComplete(t *testing.T) {
	if AllowedForAgent(Complete) {
		t.Fatal("an agent may not set `complete` — that is the operator/human's call")
	}
	for _, s := range []string{Open, InProgress, ReadyForReview} {
		if !AllowedForAgent(s) {
			t.Fatalf("agent should be allowed to set %q", s)
		}
	}
	// Derived from Valid, so a non-status is denied without needing its own
	// branch — and stays denied if the vocabulary grows.
	if AllowedForAgent("not-a-status") {
		t.Fatal("AllowedForAgent accepts a value that is not a status at all")
	}
}
