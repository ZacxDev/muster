package plugins

import (
	"testing"
	"time"
)

// TestDraftTimeDecidedIsTheSweepsFirstStep: a read that does not sweep
// (DraftCardsByID) must not offer a decision on a draft the next sweep would
// expire — pending or approved, unclaimed, past its expiry — and must on any
// other.
func TestDraftTimeDecidedIsTheSweepsFirstStep(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	claimed := now.Add(-time.Minute)
	for _, c := range []struct {
		name string
		d    Draft
		want bool
	}{
		{"pending, past expiry", Draft{Status: DraftPending, ExpiresAt: now.Add(-time.Second)}, true},
		{"pending, at expiry", Draft{Status: DraftPending, ExpiresAt: now}, true},
		{"approved, unclaimed, past expiry", Draft{Status: DraftApproved, ExpiresAt: now.Add(-time.Second)}, true},
		{"pending, not yet", Draft{Status: DraftPending, ExpiresAt: now.Add(time.Second)}, false},
		{"approved and claimed, past expiry", Draft{Status: DraftApproved, ClaimedAt: &claimed, ExpiresAt: now.Add(-time.Second)}, false},
		{"rejected, past expiry", Draft{Status: DraftRejected, ExpiresAt: now.Add(-time.Second)}, false},
	} {
		if got := DraftTimeDecided(c.d, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
