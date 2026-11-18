package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
)

// TestATurnGateClosureHasOneOwner: a second close of a closed id is REFUSED
// (gateClosed) and changes nothing — so only the first closer's reopen opens
// it; a running turn refuses a close as gateBusy; reopen opens it again.
func TestATurnGateClosureHasOneOwner(t *testing.T) {
	var g turnGate
	if ok, why := g.closeFor(7); !ok || why != "" {
		t.Fatalf("first close: %v %q", ok, why)
	}
	if ok, why := g.closeFor(7); ok || why != gateClosed {
		t.Fatalf("second close of a closed id: %v %q, want refused as %q", ok, why, gateClosed)
	}
	if g.enter(7) {
		t.Fatal("a refused second close opened the gate")
	}
	if g.close(7) {
		t.Fatal("close (the bool form) accepted a second close")
	}
	g.reopen(7)
	if !g.enter(7) {
		t.Fatal("reopen did not open the gate")
	}
	if ok, why := g.closeFor(7); ok || why != gateBusy {
		t.Fatalf("close while a turn runs: %v %q, want refused as %q", ok, why, gateBusy)
	}
	g.leave(7)
	if ok, _ := g.closeFor(7); !ok {
		t.Fatal("close after the turn left")
	}
	// Another id is independent.
	if ok, _ := g.closeFor(8); !ok {
		t.Fatal("close of another id")
	}
}

// TestChiefRestartClaimOutlivesTheRestart: the restart's session claim lasts
// past its worst case — the Stop and the Start, each bounded by the lifecycle
// adapter's own op timeout, plus the wait — so a delivery cannot claim the
// chief while it is still being restarted.
func TestChiefRestartClaimOutlivesTheRestart(t *testing.T) {
	if chiefRestartOpBound < agentprovision.DefaultOpTimeout {
		t.Fatalf("chiefRestartOpBound %s is shorter than the adapter's op timeout %s", chiefRestartOpBound, agentprovision.DefaultOpTimeout)
	}
	worst := 2*agentprovision.DefaultOpTimeout + chiefRestartWait + 2*chiefRestartReadBound
	if ttl := chiefRestartClaimTTL(); ttl <= worst {
		t.Fatalf("claim TTL %s does not outlive the worst-case restart %s", ttl, worst)
	}
	old := chiefRestartWait
	chiefRestartWait = 30 * time.Minute
	defer func() { chiefRestartWait = old }()
	if ttl := chiefRestartClaimTTL(); ttl <= 2*agentprovision.DefaultOpTimeout+chiefRestartWait {
		t.Fatalf("claim TTL %s does not follow a longer wait", ttl)
	}
}
