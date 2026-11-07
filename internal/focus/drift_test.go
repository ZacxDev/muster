package focus

import (
	"testing"
	"time"
)

// TestDivergedIsWordOverlapWithTheName: the overlap of the name's content
// words, stemmed; filler and opener words do not count.
func TestDivergedIsWordOverlapWithTheName(t *testing.T) {
	for _, c := range []struct {
		name, title string
		want        bool
	}{
		{"Billing migration", "Fixing the Stripe webhook retry", true},
		{"Billing migration", "Fixing the billing webhook", false},  // 1 of 2 shared: not below half
		{"Billing migration", "Migrating the invoice tables", false}, // stems meet: migr
		{"Billing migration", "Bills export", false},
		{"Stripe webhook retry", "Fixing the Stripe dashboard", true}, // 1 of 3
		{"the a", "Anything at all", false},                           // a name with no content word never drifts
	} {
		if got := Diverged(c.name, c.title); got != c.want {
			t.Errorf("Diverged(%q, %q) = %v, want %v", c.name, c.title, got, c.want)
		}
	}
}

// TestDriftOfferNeedsSustainedDivergence: the offer needs the newest
// DriftMinTitles rows diverged, the oldest of them DriftMinAge old, a
// nameable current title, and no dismissal at that title. Each case breaks
// exactly one clause.
func TestDriftOfferNeedsSustainedDivergence(t *testing.T) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	h := func(ago time.Duration, title string) HistoryEntry { return HistoryEntry{Title: title, CreatedAt: now.Add(-ago)} }
	off := []HistoryEntry{
		h(1*time.Minute, "Fixing the Stripe webhook retry"),
		h(20*time.Minute, "Reading the Stripe webhook docs"),
		h(40*time.Minute, "Tracing the webhook retry queue"),
		h(90*time.Minute, "Billing migration"),
	}
	const name = "Billing migration"
	if got := DriftOffer(name, off, "", now); got != "Fixing the Stripe webhook retry" {
		t.Fatalf("sustained divergence: offer %q, want the current title", got)
	}
	// One of the newest three still about the name: no offer.
	back := append([]HistoryEntry{}, off...)
	back[1] = h(20*time.Minute, "Back to the billing migration")
	if got := DriftOffer(name, back, "", now); got != "" {
		t.Errorf("a recent title still on the name: offer %q", got)
	}
	// Too recent: the oldest of the three is 25 minutes old.
	young := append([]HistoryEntry{}, off...)
	young[2] = h(25*time.Minute, "Tracing the webhook retry queue")
	if got := DriftOffer(name, young, "", now); got != "" {
		t.Errorf("divergence under DriftMinAge: offer %q", got)
	}
	// Exactly DriftMinAge: offered (the boundary is inclusive).
	edge := append([]HistoryEntry{}, off...)
	edge[2] = h(DriftMinAge, "Tracing the webhook retry queue")
	if got := DriftOffer(name, edge, "", now); got == "" {
		t.Error("divergence of exactly DriftMinAge: no offer")
	}
	// Fewer rows than DriftMinTitles.
	if got := DriftOffer(name, off[:2], "", now); got != "" {
		t.Errorf("two rows: offer %q", got)
	}
	// Dismissed at this title: held; a new title releases it.
	if got := DriftOffer(name, off, "Fixing the Stripe webhook retry", now); got != "" {
		t.Errorf("dismissed at the current title: offer %q", got)
	}
	if got := DriftOffer(name, off, "Reading the Stripe webhook docs", now); got != "Fixing the Stripe webhook retry" {
		t.Errorf("dismissed at an older title: offer %q, want the current title", got)
	}
	// A current title the bar would not accept as a name.
	bad := append([]HistoryEntry{}, off...)
	bad[0] = h(time.Minute, "Debugging")
	if got := DriftOffer(name, bad, "", now); got != "" {
		t.Errorf("an unnameable current title: offer %q", got)
	}
}
