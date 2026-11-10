package api

import (
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/plugins"
)

// TestARefusedTriageRouteFallsBackToTheChief (round 1, 🟡3): the one session
// muster would route to cannot take it (its host is read-only), so the route
// is refused — the event goes back to the queue and the chief gets it, as
// before; nothing is left claimed.
func TestARefusedTriageRouteFallsBackToTheChief(t *testing.T) {
	f := newTriageFixture(t, false)
	f.mirror.set(func(m *routeMirror) { m.interactive = false })
	f.authoredBy(t, f.asOwner, routeSID)
	_, m := f.assign(t, "Fix the flaky check", "See "+triRepo+"#"+strconv.Itoa(triPR)+".", false, 0)
	if n := f.turns.count(); n != 1 {
		t.Fatalf("the chief took %d turn(s); a refused route falls back to it", n)
	}
	if h := f.handled(t, m); h != "chief" {
		t.Fatalf("handled by %q, want chief", h)
	}
	if m.Status != plugins.StatusDelivered || m.SettleReason != nil {
		t.Fatalf("the event: %s, reason %v; want delivered to the chief, not settled by muster", m.Status, m.SettleReason)
	}
}
