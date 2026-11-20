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
	if !strings.Contains(logs, "stopped instance 1 to restart it: "+staleWhy) {
		t.Fatalf("no restart line naming why:\n%s", logs)
	}
	if !strings.Contains(logs, "started instance 1 — woken for new work after muster scaled it to zero (its pane was parked on a stale usage-limit menu") {
		t.Fatalf("the start line does not say why it was down:\n%s", logs)
	}
}

// TestAnInstanceHeldAtTheBackstopIsNotRestarted: an instance the fleet
// reports LimitHeld (limitpark.MaxRestartsPerDay reached; the board's
// decision) is not stopped; the hold is logged once per UTC day.
func TestAnInstanceHeldAtTheBackstopIsNotRestarted(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	for i := 0; i < 3; i++ {
		r.clk.Advance(time.Minute)
		r.fleet.limitHeld = map[int]string{1: staleWhy + "; held"}
		r.check(t)
	}
	if len(r.fleet.stops) != 0 {
		t.Fatalf("a held instance was stopped %v", r.fleet.stops)
	}
	if got := strings.Count(r.logs.String(), "NOT restarted again today"); got != 1 {
		t.Fatalf("the hold was logged %d times, want once:\n%s", got, r.logs.String())
	}
	r.clk.Advance(24 * time.Hour)
	r.check(t)
	if got := strings.Count(r.logs.String(), "NOT restarted again today"); got != 2 {
		t.Fatalf("the next UTC day's hold logged %d lines in total, want 2", got)
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

// TestTheLimitRestartAndTheAutoRestartNeverBothFire is the seam with the
// bounded auto-restart (autorestart.go), BOTH enabled: an instance in error
// whose pane also reads stale-parked is restarted ONCE, by the auto-restart
// (one stop, one start); an up instance not in error that is stale-parked is
// stopped once, by the limit restart, and the auto-restart plans nothing for
// it. Over several passes neither instance is stopped twice in one pass.
func TestTheLimitRestartAndTheAutoRestartNeverBothFire(t *testing.T) {
	r, _ := newRestartRig(t, 2, restartAfter, 3)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.check(t) // both instances up
	r.errorOut(1, officers.ErrorKickoffStuck)
	r.check(t) // instance 1 seen entering error
	r.clk.Advance(restartAfter)
	r.fleet.limitParked = map[int]string{1: staleWhy, 2: staleWhy}
	stopsBefore := len(r.fleet.stops)
	res := r.check(t)
	stops := r.fleet.stops[stopsBefore:]
	if len(res.Decision.Restart) != 1 || res.Decision.Restart[0].N != 1 {
		t.Fatalf("auto-restart plan %+v, want instance 1 (in error) only", res.Decision.Restart)
	}
	slices.Sort(stops)
	if !slices.Equal(stops, []int{1, 2}) {
		t.Fatalf("stops this pass %v, want each instance stopped exactly once: 1 by the auto-restart, 2 by the limit restart", stops)
	}
	if strings.Count(r.logs.String(), "stopped instance 1 to restart it") != 0 {
		t.Fatalf("the limit restart also acted on the instance in error:\n%s", r.logs.String())
	}
	if strings.Count(r.logs.String(), "stopped instance 2 to restart it") != 1 {
		t.Fatalf("the limit restart did not stop instance 2 once:\n%s", r.logs.String())
	}
}

// TestALimitStoppedInstanceIsNotAutoRestartedOnTheNextPass (round 0's open
// point): with the bounded auto-restart on, an instance this path stops reads
// stopped — not in error — on the next pass, so the auto-restart plans no
// restart of it; the next pass simply starts it for its work. Over the
// following passes it is stopped exactly once.
func TestALimitStoppedInstanceIsNotAutoRestartedOnTheNextPass(t *testing.T) {
	r, notices := newRestartRig(t, 1, restartAfter, 3)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	r.fleet.limitParked = map[int]string{1: staleWhy}
	r.check(t)
	if !slices.Equal(r.fleet.stops, []int{1}) {
		t.Fatalf("fixture: stops %v, want the parked instance stopped", r.fleet.stops)
	}
	for i := 0; i < 3; i++ {
		r.clk.Advance(restartAfter)
		if res := r.check(t); len(res.Decision.Restart) != 0 {
			t.Fatalf("pass %d after the limit stop: the auto-restart planned %+v", i+1, res.Decision.Restart)
		}
	}
	if len(r.fleet.stops) != 1 {
		t.Fatalf("stops %v: the limit-stopped instance was stopped again", r.fleet.stops)
	}
	if got := notices.count(func(e officers.AutoRestartEvent) bool { return e.Restarted || e.Held }); got != 0 {
		t.Fatalf("auto-restart notices %+v after a limit stop, want none", notices.all())
	}
}
