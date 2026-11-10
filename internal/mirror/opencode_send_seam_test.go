package mirror_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestThePageAndTheHubAgreeOnWhatCanBeSent is a SEAM GUARD: the page's
// reading (SendBlocked: the composer drawn disabled, and why) and the hub's
// enforcement (gatedSendCheck, on its own window list) must agree, for every
// combination of the host's advertisement and the window's binding — the page
// offering a send the hub refuses, or hiding one it would make, is the bug.
func TestThePageAndTheHubAgreeOnWhatCanBeSent(t *testing.T) {
	seen := map[bool]int{}
	for _, sends := range []bool{false, true} {
		for _, window := range []string{"@1", "@2", "@5", "@6"} { // bound, unbound, verified, cut title (all idle)
			r, raw := ocSendHost(t, sends, true)
			var e mirror.SessionView
			hosts := r.svc.Hosts(asUser(1))
			for _, x := range mirror.Ordered(hosts) {
				if x.Window == window {
					e = x
				}
			}
			if e.Window != window || len(hosts) != 1 {
				t.Fatalf("instrument check: no entry for %s", window)
			}
			pageOK := mirror.SendBlocked(hosts[0], e) == ""
			go func() {
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					if p := raw.of(mirror.TypePaste); len(p) == 1 {
						raw.write(t, mirror.Msg{Type: mirror.TypePasted, ID: p[0].ID})
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
			// As the page sends it: naming the session its view shows.
			err := r.svc.PasteWindow(ctx, "h", "work", window, "hello", mirror.PasteClaude, mirror.ShownSession(e.AgentSession))
			cancel()
			if hubOK := err == nil; hubOK != pageOK {
				t.Fatalf("sends %v window %s: the page says sendable %v (%q), the hub %v (%v)",
					sends, window, pageOK, mirror.SendBlocked(hosts[0], e), hubOK, err)
			}
			seen[pageOK]++
		}
	}
	// Positive control: the matrix holds cases of BOTH outcomes, so agreement
	// is not "both always refuse".
	if seen[true] != 2 || seen[false] != 6 {
		t.Fatalf("instrument check: sendable/blocked cases %v, want 2 and 6", seen)
	}
}
