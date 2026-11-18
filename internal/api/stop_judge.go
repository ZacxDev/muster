package api

import "context"

// ---------------------------------------------------------------------------
// WHO JUDGES AN OWNER'S STOPS (the fleet plan's A8: one judge per owner).
//
// Three things can judge an owner's stopped sessions, and exactly ONE does:
//
//   - judgeMinder: a Session minder officer the owner switched on, and that
//     can run (sessionMinderOf);
//   - judgeFleet: the fleet — ONLY once it can judge a stop (phase 3's
//     triage), behind the one seam fleetJudgesStops;
//   - judgeChief: everyone else — the chief's stall nudges, as before.
//
// 🔴 IN THIS PHASE THE FLEET NEVER JUDGES (the operator's answer to #796's
// round 0): with MUSTER_FLEET_ORCHESTRATION=propose the nudge sweep, the stop's
// push wait and the per-stop push are exactly as with it off, and the fleet's
// proposals run ALONGSIDE (fleetPass, which asks no judge).
//
// 🔴 ONE FUNCTION, EVERY READER: the nudge sweep (sweepOwnerNudges), the
// stop's push wait (episodeChiefJudges) and the minder's feed
// (feedSessionMinder) all ask stopJudgeOf. TestTheCollectorAndStallLedger pins
// its callers; TestTheFleetDoesNotJudgeStopsInThisPhase pins the seam.
// ---------------------------------------------------------------------------

// The judges stopJudgeOf names.
const (
	judgeChief  = "chief_nudges"
	judgeMinder = "minder"
	judgeFleet  = "fleet"
)

// fleetJudgesStops is THE seam where phase 3 lets the fleet judge an owner's
// stops (its batched triage replacing the chief's per-stop check): false in
// this phase, whatever the flag.
func (s *Server) fleetJudgesStops() bool { return false }

// stopJudgeOf is who judges owner's stops.
func (s *Server) stopJudgeOf(octx context.Context, owner int64) string {
	if s.sessionMinderOf(octx, owner) != "" {
		return judgeMinder
	}
	if s.fleetJudgesStops() && s.fleetProposeOn() {
		if _, err := s.chiefAgent(octx); err == nil {
			return judgeFleet
		}
	}
	return judgeChief
}
