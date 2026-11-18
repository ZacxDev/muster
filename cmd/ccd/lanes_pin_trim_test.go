package main

import (
	"context"
	"testing"
)

// laneCounts is how many lanes run, and how many of them are unpinned.
func laneCounts(l *lanes) (all, unpinned int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.set), l.unpinnedLocked()
}

// TestPinMovesNeverGrowTheSetPastTheCap (PR #609 round 2 N1, the auditor's
// probe): at a cap of one, four fresh primaries in a row (each pins a new lane)
// and one more ordinary lane. Before, every pin move left the former primary
// running as one more unpinned CLI for good — 5 lanes, 4 unpinned. Now the
// unpinned lanes never exceed the cap: the set is the pinned lane plus at most
// the cap.
func TestPinMovesNeverGrowTheSetPastTheCap(t *testing.T) {
	r := newLaneRig(t, 1)
	ctx := context.Background()
	for _, k := range []string{"primary-1", "primary-2", "primary-3", "primary-4"} {
		ln, f := r.l.acquireIn(ctx, k, "", "", laneMode{pinned: true})
		if f != nil {
			t.Fatal(f)
		}
		// Measured while the new primary's turn still holds its lane: the
		// trim is the PIN MOVE's, not left to the next release.
		if _, unpinned := laneCounts(r.l); unpinned > 1 {
			t.Fatalf("pinning %s left %d unpinned lanes running at a cap of 1", k, unpinned)
		}
		r.l.release(ln)
		started(t, ln)
	}
	r.operator(t, "side-a")
	if all, unpinned := laneCounts(r.l); unpinned > 1 || all > 2 {
		t.Fatalf("after 4 pin moves and a side chat at a cap of 1: %d lanes, %d unpinned; want at most 2 and 1", all, unpinned)
	}
	if r.l.get(laneID("primary-4")) == nil {
		t.Error("the pinned primary was stopped")
	}
}

// TestAHeldFormerPrimaryIsStoppedWhenItsTurnEnds: a pin that moves while every
// unpinned lane is held by a turn (the old primary mid-handover) cannot trim at
// once — nothing held is ever stopped — and the excess goes when the turn
// releases its lane.
func TestAHeldFormerPrimaryIsStoppedWhenItsTurnEnds(t *testing.T) {
	r := newLaneRig(t, 1)
	ctx := context.Background()
	side, _ := r.l.acquire(ctx, "side-a", "") // held: mid-turn
	defer r.l.release(side)
	old, f := r.l.acquireIn(ctx, "old-primary", "", "", laneMode{pinned: true}) // held: its handover
	if f != nil {
		t.Fatal(f)
	}
	started(t, old)
	fresh, f := r.l.acquireIn(ctx, "new-primary", "", "", laneMode{pinned: true})
	if f != nil {
		t.Fatal(f)
	}
	r.l.release(fresh)
	started(t, fresh)
	if all, _ := laneCounts(r.l); all != 3 || r.l.get(old.id) != old {
		t.Fatalf("precondition: with every unpinned lane held, %d lanes (old kept=%v); want 3, nothing held stopped", all, r.l.get(old.id) == old)
	}
	r.l.release(old) // its handover turn is over
	if r.l.get(old.id) != nil {
		t.Error("THE FORMER PRIMARY OUTLIVED ITS TURN past the cap")
	}
	if all, unpinned := laneCounts(r.l); all != 2 || unpinned != 1 {
		t.Errorf("after the release: %d lanes, %d unpinned; want 2 and 1", all, unpinned)
	}
}
