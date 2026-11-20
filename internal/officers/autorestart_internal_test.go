package officers

import (
	"testing"
	"time"
)

// TestAHeldClaimIsRestartWorkOnlyWhenItCouldBeClaimedAgain: a live claim of
// the instance in error is released by its restart either way (all), but is
// a REASON to restart (claimable) only when a fresh instance could claim it
// again: not over the daily budget (unless the owner's own item bypasses
// it), not while the spend is unreadable, and not under a spent judgement
// session cap unless it was already counted today.
func TestAHeldClaimIsRestartWorkOnlyWhenItCouldBeClaimedAgain(t *testing.T) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	lease := now.Add(time.Hour)
	o := Officer{ID: "release:example-web", MaxInstances: 1}
	in := InstanceState{N: 1, Name: o.InstanceName(1), Error: true}
	item := func(id string, origin string) Item {
		return Item{ID: ItemID(id), Status: StatusClaimed, ClaimedBy: in.Name, LeaseExpiresAt: &lease, Origin: origin}
	}
	for _, c := range []struct {
		name          string
		obs           Observation
		wantClaimable int
	}{
		{"in budget", Observation{Claimed: []Item{item("31", "")}}, 1},
		{"over budget", Observation{Claimed: []Item{item("32", "")}, OverBudget: true}, 0},
		{"over budget, the owner's own item", Observation{Claimed: []Item{item("33", OriginOwner)}, OverBudget: true}, 1},
		{"spend unreadable", Observation{Claimed: []Item{item("34", "")}, SpendUnreadable: true}, 0},
		{"session cap spent, not counted", Observation{Claimed: []Item{item("35", "")}, Capped: true}, 0},
		{"session cap spent, counted today", Observation{Claimed: []Item{item("36", "")}, Capped: true, Counted: map[ItemID]bool{"36": true}}, 1},
	} {
		claimable, all := heldClaims(o, c.obs, in, now)
		if len(claimable) != c.wantClaimable || len(all) != 1 {
			t.Errorf("%s: %d claimable, %d released; want %d and 1", c.name, len(claimable), len(all), c.wantClaimable)
		}
		if got := restartWork(o, Decision{}, in, claimable); got != (c.wantClaimable == 1) {
			t.Errorf("%s: restartWork = %v, want %v", c.name, got, c.wantClaimable == 1)
		}
	}
}
