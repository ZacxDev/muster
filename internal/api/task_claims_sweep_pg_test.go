package api

import (
	"context"
	"net/http"
	"testing"
)

// THE CLAIM SWEEP (task_claims.go ReleaseEndedClaims, run every
// ClaimSweepInterval by cmd/muster-server): a claim whose session ended is
// released without waiting for a decision to read it, so the board stops
// naming a session that is gone. Every id is invented.

func TestTheClaimSweepReleasesAnEndedSessionsClaim(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
	f.pickUp(t, id, http.StatusAccepted)
	ctx := context.Background()
	if n, err := f.srv.ReleaseEndedClaims(ctx); err != nil || n != 0 {
		t.Fatalf("a sweep with the session still running released %d (%v); want 0", n, err)
	}
	if len(f.claimedBy(t, id)) != 1 {
		t.Fatal("the sweep released a running session's claim")
	}
	// The host stops reporting status: it cannot say the window is gone.
	f.mirror.set(func(m *routeMirror) { m.claudeSession, m.statusReport = triSID2, false })
	if n, err := f.srv.ReleaseEndedClaims(ctx); err != nil || n != 0 {
		t.Fatalf("a sweep against a host that cannot say released %d (%v); want 0", n, err)
	}
	f.mirror.set(func(m *routeMirror) { m.statusReport = true })
	if n, err := f.srv.ReleaseEndedClaims(ctx); err != nil || n != 1 {
		t.Fatalf("the sweep after the session ended released %d (%v); want 1", n, err)
	}
	if got := f.claimedBy(t, id); len(got) != 0 {
		t.Fatalf("after the sweep the task is still claimed by %q", got)
	}
}

// TestTheClaimSweepReleasesAnEndedUpstreamClaimant: an upstream claimant's
// session (linked with the host that listed it) ended: the sweep releases
// that claim at the snapshot's revision, and the next assignment gets its card.
func TestTheClaimSweepReleasesAnEndedUpstreamClaimant(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	snap := func() map[string]any {
		return claimSnap("in_progress", "in progress", map[string]any{"rev": "5",
			"claimants": []map[string]any{{"agent": "ag_presscal01", "agent_session": routeSID}}})
	}
	id := f.upsertRef(t, key, snap())
	ctx := context.Background()
	if n, err := f.srv.ReleaseEndedClaims(ctx); err != nil || n != 0 {
		t.Fatalf("a sweep with the claimant's session running released %d (%v); want 0", n, err)
	}
	f.mirror.set(func(m *routeMirror) { m.claudeSession = triSID2 })
	if n, err := f.srv.ReleaseEndedClaims(ctx); err != nil || n != 1 {
		t.Fatalf("the sweep after the claimant's session ended released %d (%v); want 1", n, err)
	}
	if n, err := f.srv.ReleaseEndedClaims(ctx); err != nil || n != 0 {
		t.Fatalf("a second sweep released %d (%v); want 0 (already released at this revision)", n, err)
	}
	_, m := f.assignRef(t, key, snap())
	f.mustGetItsCard(t, id, m)
}
