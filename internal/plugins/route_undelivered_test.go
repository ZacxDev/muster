package plugins

import (
	"testing"
	"time"
)

// TestRouteNeverDeliveredHandBackNamesTheWindow: a route's window (created to
// deadline) is named in whole minutes; one with no positive window — clocks,
// or a hand-edited row — names only the deadline.
func TestRouteNeverDeliveredHandBackNamesTheWindow(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		created time.Time
		want    string
	}{
		{at.Add(-90 * time.Minute), "it was never delivered: the session stayed busy for the whole 1 hour 30 minutes it had " +
			"(until 2026-10-31T12:00:00Z), and its queued message was cancelled unsent"},
		{at, "it was never delivered: the session stayed busy until its deadline (2026-10-31T12:00:00Z), " +
			"and its queued message was cancelled unsent"},
	} {
		if got := routeNeverDeliveredHandBack(Route{CreatedAt: c.created, TimeoutAt: at}); got != c.want {
			t.Errorf("window %v:\n got %q\nwant %q", at.Sub(c.created), got, c.want)
		}
	}
}
