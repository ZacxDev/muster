package api

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
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
	// The wake loop's next pass sees it stopped, NOT in error: the bounded
	// auto-restart (which acts only on an instance in error) cannot restart
	// it as well.
	after, err := f.Instances(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].Up || after[0].Error {
		t.Fatalf("after the stop instance 1 reads up=%v error=%v; want stopped, not in error", after[0].Up, after[0].Error)
	}
}

// TestLimitRestartableIsAPodOfficersInstanceOnly: THE rule both the gateway
// (whose stale refusal marks nothing) and the fleet (which reports a park the
// loop restarts) ask. A pod officer's instance is restartable; an ordinary
// agent is not, so its stale menu refusal keeps marking and failing over
// (agentgateway TestANonRestartedAgentsStaleMenuStillMarksAndFailsOver).
func TestLimitRestartableIsAPodOfficersInstanceOnly(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	inst := r.addNamed(officers.InstanceName(offA, 1), 3111)
	if !r.s.LimitRestartable(ctx, inst) {
		t.Fatal("a pod officer's instance is not limit-restartable")
	}
	plain := agents.Agent{ID: 3112, Name: "brave-otter", Kind: agents.KindClaudeCode, OwnerID: 1}
	if r.s.LimitRestartable(ctx, plain) {
		t.Fatal("an ordinary agent is limit-restartable: its stale menu refusal would be silenced with nothing to restart it")
	}
	// A HOST officer's instance: its fleet reports no park, so nothing
	// restarts it off the menu, and its stale refusal must keep marking.
	h := newHostOfficerRig(t, true)
	hostInst := agents.Agent{ID: 3113, Name: officers.InstanceName(h.host.ID, 1), Kind: agents.KindClaudeCode, OwnerID: h.host.Owner}
	if _, _, ok := h.s.officerOf(hostInst); !ok {
		t.Fatal("fixture: the host officer's instance is not an officer instance")
	}
	if h.s.LimitRestartable(ctx, hostInst) {
		t.Fatal("a host officer's instance is limit-restartable, but its fleet reports no park: its refusal would be silenced with nothing to restart it")
	}
	podInst := agents.Agent{ID: 3114, Name: officers.InstanceName(h.pod.ID, 1), Kind: agents.KindClaudeCode, OwnerID: 1}
	if !h.s.LimitRestartable(ctx, podInst) {
		t.Fatal("control: the same server's pod officer instance is not limit-restartable")
	}
}

// TestTheFleetReportsNoParkForAnAgentTheRuleExcludes: the fleet asks THE
// rule before it reports a park, so the loop never restarts an agent whose
// refusal the gateway did not silence. With the rule answering false (no
// officer runtime claims the name any more), a stuck park is not reported.
func TestTheFleetReportsNoParkForAnAgentTheRuleExcludes(t *testing.T) {
	r := newOfficerRig(t)
	ctx := context.Background()
	inst := r.addNamed(officers.InstanceName(offA, 1), 3121)
	now := time.Date(2001, 2, 3, 9, 19, 0, 0, time.UTC)
	board := limitpark.New(nil, func() time.Time { return now })
	r.s.ext.LimitParks = board
	const weekly = "You've hit your weekly limit · resets Nov 21, 9am (UTC)"
	board.NoteLimit(ctx, inst.ID, inst.OwnerID, "work", weekly, false)
	now = now.Add(15 * time.Minute)
	u := 21.0
	board.NoteReading(ctx, inst.OwnerID, "work", ccusage.Row{Account: "work", Source: ccusage.SourceRateLimitHeaders, Status: "allowed",
		ReadAt: now, CheckedAt: now, Windows: map[string]ccusage.Window{ccusage.SevenDay: {Utilization: &u, Status: "allowed"}}}, "")
	now = now.Add(time.Minute)
	board.NoteLimit(ctx, inst.ID, inst.OwnerID, "work", weekly, true)
	if _, stuck := board.Stuck(inst.ID); !stuck {
		t.Fatal("fixture: not stuck")
	}
	o, err := r.s.officerRT.reg.Get(offA)
	if err != nil {
		t.Fatal(err)
	}
	// The same officer, as a HOST officer from the rule's point of view: the
	// fleet call below is the pod path, but LimitRestartable now answers false.
	ho := o
	ho.Runtime, ho.Host = officers.RuntimeHost, "example-host"
	r.s.officerRT.reg.Swap([]officers.Officer{ho})
	defer r.s.officerRT.reg.Swap([]officers.Officer{o})
	insts, err := (officerFleet{s: r.s}).Instances(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if insts[0].LimitParked != "" {
		t.Fatalf("the fleet reported a park (%q) for an agent the rule excludes", insts[0].LimitParked)
	}
}
