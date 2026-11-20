package officers

import (
	"context"
	"time"
)

// LimitRestartsPerDay bounds how many times one instance is restarted off a
// stale usage-limit menu (restartLimitParked) per UTC day. Each restart also
// needs a NEW live reading of its account newer than the pane's park
// (internal/limitpark), so the rate is at most one per usage-poll interval
// before this cap is reached.
const LimitRestartsPerDay = 4

// limitRestartDay is one instance's limit restarts on day (a UTC date).
type limitRestartDay struct {
	day string
	n   int
	// heldLogged: the cap's hold was logged for day.
	heldLogged bool
}

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
//     the error path's (it holds its slot until its own restart), so the two
//     never restart one instance twice.
//   - Bounded: at most LimitRestartsPerDay per instance per UTC day; past it
//     the instance is held (logged once that day) and an operator acts.
//   - Never while the account still reads limited: Board.Stuck answers only
//     for a pane parked since before a live reading that shows the account
//     usable, with no fresh limit hit since.
//   - One log line per restart and per held instance.
func (r *Reconciler) restartLimitParked(ctx context.Context, pools []Officer, obs []Observation, now time.Time) {
	day := utcDay(now)
	for i, p := range pools {
		for j := range obs[i].Instances {
			in := &obs[i].Instances[j]
			if in.LimitParked == "" || !in.Up || in.Error {
				continue
			}
			r.mu.Lock()
			s := r.st(p.Key())
			if s.limitRestarts == nil {
				s.limitRestarts = map[string]limitRestartDay{}
			}
			rec := s.limitRestarts[in.Name]
			if rec.day != day {
				rec = limitRestartDay{day: day}
			}
			if rec.n >= LimitRestartsPerDay {
				logHeld := !rec.heldLogged
				rec.heldLogged = true
				s.limitRestarts[in.Name] = rec
				r.mu.Unlock()
				if logHeld {
					r.log.Printf("officers: %s: instance %s is parked on a stale usage-limit menu, but it was restarted off one %d times today (the cap): "+
						"NOT restarted again until the UTC day ends; stop and start it to free it (%s)", p.label(), p.InstanceLabel(in.N), rec.n, in.LimitParked)
				}
				continue
			}
			r.mu.Unlock()
			if err := r.fleet.Stop(ctx, p, in.N); err != nil {
				r.log.Printf("officers: %s: stop instance %s off a stale usage-limit menu: %v", p.label(), p.InstanceLabel(in.N), err)
				continue
			}
			r.mu.Lock()
			rec.n++
			s.limitRestarts[in.Name] = rec
			r.mu.Unlock()
			r.log.Printf("officers: %s: stopped instance %s to restart it (%d of %d today): %s; its claims are released and it starts again when work waits",
				p.label(), p.InstanceLabel(in.N), rec.n, LimitRestartsPerDay, in.LimitParked)
			// Its start (this pass or the next, once its released claim is
			// claimable again) says why it was down.
			r.scaledDownBy(p, in.N, limitParkedStop)
			in.Up, in.Down, in.LimitParked = false, limitParkedStop, ""
		}
	}
}
