package officers_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

const staleWhy = "its pane has been parked on the usage-limit menu since 2000-01-01T09:19:00Z, and account work was read usable at 2000-01-01T11:45:00Z"

// TestAnInstanceOnAStaleLimitMenuIsRestartedAndItsClaimReleased: an up
// instance whose pane is parked on a stale usage-limit menu, holding a claim,
// is stopped and its claim released in one pass, and started again on the
// next (a released item is claimable once HQ shows it open) — the operator's
// Stop and Start. Before, the loop only re-prompted it
// (ccd refused every prompt from the menu) and it held its slot and its claim
// for hours. CONTROL: the same instance not parked is left alone.
func TestAnInstanceOnAStaleLimitMenuIsRestartedAndItsClaimReleased(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	id := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	if len(r.fleet.starts) != 1 {
		t.Fatalf("fixture: starts = %v, want instance 1 started", r.fleet.starts)
	}
	holder := officers.InstanceName(relID, 1)
	r.hq.SetClaim(id, holder, r.clk.Now().Add(20*time.Minute))
	r.clk.Advance(officers.RepromptAfter)
	if res := r.check(t); len(r.fleet.stops) != 0 || len(res.Decision.Release) != 0 {
		t.Fatalf("control: an instance NOT parked was stopped %v / released %v", r.fleet.stops, res.Decision.Release)
	}

	r.fleet.limitParked = map[int]string{1: staleWhy}
	res := r.check(t)
	if !slices.Equal(r.fleet.stops, []int{1}) {
		t.Fatalf("stops = %v, want the parked instance 1 stopped", r.fleet.stops)
	}
	if len(res.Decision.Release) != 1 || string(res.Decision.Release[0].ID) != id {
		t.Fatalf("release = %v, want its claim on %s released", res.Decision.Release, id)
	}
	if it := r.hq.Item(id); it.ClaimedBy != "" {
		t.Fatalf("the parked instance still holds %s: %+v", id, it)
	}
	r.check(t)
	if !slices.Equal(r.fleet.starts, []int{1, 1}) {
		t.Fatalf("starts = %v, want instance 1 started again for its released item", r.fleet.starts)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "stopped instance 1 to restart it (1 of 4 today): "+staleWhy) {
		t.Fatalf("no restart line naming why:\n%s", logs)
	}
	if !strings.Contains(logs, "started instance 1 — woken for new work after muster scaled it to zero (its pane was parked on a stale usage-limit menu") {
		t.Fatalf("the start line does not say why it was down:\n%s", logs)
	}
}

// TestAStaleLimitRestartIsBoundedPerDay: an instance that lands on a stale
// menu again and again is restarted at most LimitRestartsPerDay times per UTC
// day, then held (logged once); the next UTC day it may be restarted again.
func TestAStaleLimitRestartIsBoundedPerDay(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	for i := 0; i < officers.LimitRestartsPerDay+2; i++ {
		r.clk.Advance(time.Minute)
		r.fleet.limitParked = map[int]string{1: staleWhy}
		r.check(t)
	}
	if got := len(r.fleet.stops); got != officers.LimitRestartsPerDay {
		t.Fatalf("restarted %d times in one day, want the cap %d", got, officers.LimitRestartsPerDay)
	}
	if got := strings.Count(r.logs.String(), "NOT restarted again until the UTC day ends"); got != 1 {
		t.Fatalf("the hold was logged %d times, want once:\n%s", got, r.logs.String())
	}
	// A new UTC day: allowed again.
	r.clk.Advance(24 * time.Hour)
	r.fleet.limitParked = map[int]string{1: staleWhy}
	r.check(t)
	if got := len(r.fleet.stops); got != officers.LimitRestartsPerDay+1 {
		t.Fatalf("stops on the next UTC day = %d, want %d", got, officers.LimitRestartsPerDay+1)
	}
}

// TestAnInstanceInErrorIsNotLimitRestarted: an instance in error is the
// error path's to restart; the limit restart never stops it as well, so one
// instance is never restarted twice. And a down instance has nothing to stop.
func TestAnInstanceInErrorIsNotLimitRestarted(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.check(t)
	r.fleet.errored[1] = true
	r.fleet.up[2] = false
	r.fleet.limitParked = map[int]string{1: staleWhy, 2: staleWhy}
	r.check(t)
	if len(r.fleet.stops) != 0 {
		t.Fatalf("stopped %v: an errored and a down instance must not be limit-restarted", r.fleet.stops)
	}
	// Control: the same instance out of error and up is.
	r.fleet.errored[1] = false
	r.fleet.up[1] = true
	r.check(t)
	if !slices.Equal(r.fleet.stops, []int{1}) {
		t.Fatalf("control: stops = %v, want [1]", r.fleet.stops)
	}
}
