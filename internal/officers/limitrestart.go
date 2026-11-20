package officers

import (
	"context"
	"time"
)

// The daily backstop on these restarts is limitpark.MaxRestartsPerDay, kept
// on the board WITH the rule that silences a stale refusal, so the two are
// decided in one place: past it the fleet reports the instance LimitHeld (not
// LimitParked), and the gateway's next refusal from it marks the account and
// asks the failover as before. The loop only logs the hold.

// limitParkedStop is why restartLimitParked stopped an instance: its start
// line names it ("woken for new work after muster scaled it to zero (…)").
const limitParkedStop = "its pane was parked on a stale usage-limit menu, and its account reads usable again"

// restartLimitParked stops every instance of pools whose pane is parked on a
// STALE usage-limit menu (InstanceState.LimitParked), and marks it down in obs
// so this same pass's Decide releases any claim it held and starts it again
// when work waits — the Stop and Start an operator would do, which is what
// cleared every such instance by hand.
//
// 🔴 WHY A RESTART, NOT AN ANSWER TO THE MENU. The menu's options are the
// CLI's, drawn on a screen muster reads by pattern (internal/ccdialog), and
// what the CLI does with an answer — resume the pending turn, drop it, or
// open another prompt — is not something muster can observe or pin; ccd
// deliberately never types into a dialog. A restart is the lifecycle muster
// already runs and tests: the CLI starts fresh, the conversation is on the
// volume, and the claimed work is released and claimed again.
//
//   - Only an instance that is up and NOT in error: an instance in error is
//     the auto-restart's (autorestart.go planRestarts, which acts only on an
//     instance in error), so the two never restart one instance twice. Nor
//     can this stop hand it to the auto-restart afterwards: Fleet.Stop scales
//     the instance to zero, so on the next pass it reads stopped (down, not
//     in error) and is started like any stopped instance; planRestarts needs
//     an instance in error that noteErrors saw enter it
//     (TestALimitStoppedInstanceIsNotAutoRestartedOnTheNextPass, and in
//     internal/api TestTheFleetReportsAStaleLimitParkAndAStopEndsIt). A
//     start that then FAILS is a new error episode of its own, which the
//     auto-restart handles by its own rules.
//   - Paced by the timing rule (the stop ends the park; the next restart
//     needs a new park and a newer reading: at most one per usage-poll
//     interval), with limitpark.MaxRestartsPerDay a day as its backstop: past
//     it the fleet reports LimitHeld, the loop logs it once that UTC day and
//     restarts nothing, and the instance's refusals mark and fail over again.
//   - Never while the account still reads limited: Board.Stuck answers only
//     for a pane parked since before a live reading that shows the account
//     usable, with no fresh limit hit since.
//   - One log line per restart and per held instance.
func (r *Reconciler) restartLimitParked(ctx context.Context, pools []Officer, obs []Observation, now time.Time) {
	day := utcDay(now)
	for i, p := range pools {
		for j := range obs[i].Instances {
			in := &obs[i].Instances[j]
			if !in.Up || in.Error {
				continue
			}
			if in.LimitHeld != "" {
				r.mu.Lock()
				s := r.st(p.Key())
				if s.limitHeldLogged == nil {
					s.limitHeldLogged = map[string]string{}
				}
				first := s.limitHeldLogged[in.Name] != day
				s.limitHeldLogged[in.Name] = day
				r.mu.Unlock()
				if first {
					r.log.Printf("officers: %s: instance %s is parked on a stale usage-limit menu but NOT restarted again today: %s; "+
						"its refusals mark the account and ask the failover again; stop and start it to free it", p.label(), p.InstanceLabel(in.N), in.LimitHeld)
				}
				continue
			}
			if in.LimitParked == "" {
				continue
			}
			if err := r.fleet.Stop(ctx, p, in.N); err != nil {
				r.log.Printf("officers: %s: stop instance %s off a stale usage-limit menu: %v", p.label(), p.InstanceLabel(in.N), err)
				continue
			}
			r.log.Printf("officers: %s: stopped instance %s to restart it: %s; its claims are released and it starts again when work waits",
				p.label(), p.InstanceLabel(in.N), in.LimitParked)
			// Its start (this pass or the next, once its released claim is
			// claimable again) says why it was down.
			r.scaledDownBy(p, in.N, limitParkedStop)
			in.Up, in.Down, in.LimitParked = false, limitParkedStop, ""
		}
	}
}
