package agents

import (
	"testing"
	"time"
)

// TestSwitcherOrderPutsThePrimaryFirst: the primary first whatever its
// activity, then the side chats newest first (id breaking a tie, newest first),
// the archived ones apart in the same order. The fixture's ids, activity times
// and input order are pairwise distinct and disagree with each other, so an
// order by id, by input position or by created_at lands differently.
func TestSwitcherOrderPutsThePrimaryFirst(t *testing.T) {
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	archived := at(0)
	chats := []ChatListing{
		{ChatSession: ChatSession{ID: 4, UpdatedAt: at(30), CreatedAt: at(1)}},                        // side
		{ChatSession: ChatSession{ID: 9, UpdatedAt: at(5), IsDefault: true, CreatedAt: at(2)}},        // primary, the OLDEST activity
		{ChatSession: ChatSession{ID: 2, UpdatedAt: at(50), CreatedAt: at(3)}, Unread: true},          // side, newest
		{ChatSession: ChatSession{ID: 7, UpdatedAt: at(40), CreatedAt: at(4), ArchivedAt: &archived}}, // archived
		{ChatSession: ChatSession{ID: 3, UpdatedAt: at(30), CreatedAt: at(5)}},                        // side, ties 4: id 4 first
		{ChatSession: ChatSession{ID: 8, UpdatedAt: at(45), CreatedAt: at(6), ArchivedAt: &archived}}, // archived, newer
	}
	sw := SwitcherOrder(chats)
	if sw.Primary == nil || sw.Primary.ID != 9 {
		t.Fatalf("primary = %+v, want 9", sw.Primary)
	}
	ids := func(s []ChatListing) []int64 {
		out := make([]int64, len(s))
		for i, c := range s {
			out[i] = c.ID
		}
		return out
	}
	if got, want := ids(sw.Side), []int64{2, 4, 3}; !sameIDs(got, want) {
		t.Errorf("side chats %v, want %v", got, want)
	}
	if got, want := ids(sw.Archived), []int64{8, 7}; !sameIDs(got, want) {
		t.Errorf("archived %v, want %v", got, want)
	}
	if !sw.Side[0].Unread {
		t.Error("the unread mark was lost")
	}
	if SwitcherOrder(nil).Primary != nil {
		t.Error("no chats has a primary")
	}
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClaudeContextWindow: the Claude models muster names, with and without
// the CLI's [1m] suffix, and nothing for any other model.
func TestClaudeContextWindow(t *testing.T) {
	for model, want := range map[string]int64{
		"opus": 200_000, "sonnet": 200_000, "haiku": 200_000, "claude": 200_000,
		"claude-opus-4-6": 200_000, "opus[1m]": 1_000_000, "claude-sonnet-4-6[1m]": 1_000_000,
		"anthropic/claude-sonnet-4": 200_000, "openrouter/xiaomi/mimo-v2.5": 0, "": 0, "gpt-5": 0,
	} {
		if got := ClaudeContextWindow(model); got != want {
			t.Errorf("ClaudeContextWindow(%q) = %d, want %d", model, got, want)
		}
	}
}
