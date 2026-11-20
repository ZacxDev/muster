package api

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccusage"
	"github.com/ZacxDev/muster/internal/limitpark"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestTheFleetReportsAStaleLimitParkAndAStopEndsIt is the seam between the
// limit-park board (fed by the gateway and the usage poller) and the wake
// loop (which restarts what the fleet reports): an instance whose pane the
// board finds stuck on a stale usage-limit menu is reported LimitParked by
// the fleet, its neighbour is not, and the fleet's Stop forgets the park so
// the instance it starts again is not restarted for the old menu.
func TestTheFleetReportsAStaleLimitParkAndAStopEndsIt(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	stuck := r.addNamed(officers.InstanceName(offA, 1), 3101)
	other := r.addNamed(officers.InstanceName(offA, 2), 3102)
	now := time.Date(2001, 2, 3, 9, 19, 0, 0, time.UTC)
	board := limitpark.New(nil, func() time.Time { return now })
	r.s.ext.LimitParks = board

	const weekly = "You've hit your weekly limit · resets Nov 21, 9am (UTC)"
	board.NoteLimit(ctx, stuck.ID, stuck.OwnerID, "work", weekly, false)
	now = now.Add(15 * time.Minute)
	u := 21.0
	board.NoteReading(ctx, stuck.OwnerID, "work", ccusage.Row{Account: "work", Source: ccusage.SourceRateLimitHeaders, Status: "allowed",
		ReadAt: now, CheckedAt: now, Windows: map[string]ccusage.Window{ccusage.SevenDay: {Utilization: &u, Status: "allowed"}}}, "")
	now = now.Add(time.Minute)
	if stale, _ := board.NoteLimit(ctx, stuck.ID, stuck.OwnerID, "work", weekly+" — the CLI's pane shows its usage-limit menu", true); !stale {
		t.Fatal("fixture: the board did not find the park stale")
	}

	o, err := r.s.officerRT.reg.Get(offA)
	if err != nil {
		t.Fatal(err)
	}
	f := officerFleet{s: r.s}
	insts, err := f.Instances(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) < 2 || insts[0].LimitParked == "" || insts[1].LimitParked != "" {
		t.Fatalf("instances %+v: want instance 1 (agent %d) LimitParked and instance 2 (agent %d) not", insts, stuck.ID, other.ID)
	}
	if err := f.Stop(ctx, o, 1); err != nil {
		t.Fatal(err)
	}
	if _, parked := board.Parked(stuck.ID); parked {
		t.Fatal("the fleet's Stop left the instance's park: its next start would be restarted for the old menu")
	}
}
