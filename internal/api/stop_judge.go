package api

import "context"

// ---------------------------------------------------------------------------
// WHO JUDGES AN OWNER'S STOPS (the fleet plan's A8: one judge per owner).
//
// Three things can judge an owner's stopped sessions, and exactly ONE does:
//
//   - judgeMinder: a Session minder officer the owner switched on, and that
//     can run (sessionMinderOf) — the owner's own, explicit choice, so it
//     wins over the deployment's fleet flag;
//   - judgeFleet: fleet proposals are on (MUSTER_FLEET_ORCHESTRATION=propose,
//     fleetProposeOn) with session state, and the owner has a chief — the
//     fleet's deterministic proposals (fleetPass) replace the chief's nudge
//     decision, as a minder does;
//   - judgeChief: everyone else — the chief's stall nudges, as before.
//
// 🔴 ONE FUNCTION, EVERY READER: the nudge sweep (sweepOwnerNudges), the
// stop's push wait (episodeChiefJudges), the minder's feed
// (feedSessionMinder) and the fleet's step (fleetPass) all ask stopJudgeOf, so
// two judges can never act on one stop. TestTheCollectorAndStallLedger pins
// its callers.
// ---------------------------------------------------------------------------

// The judges stopJudgeOf names.
const (
	judgeChief  = "chief_nudges"
	judgeMinder = "minder"
	judgeFleet  = "fleet"
)

// stopJudgeOf is who judges owner's stops.
func (s *Server) stopJudgeOf(octx context.Context, owner int64) string {
	if s.sessionMinderOf(octx, owner) != "" {
		return judgeMinder
	}
	if s.fleetProposeOn() && s.sessionStateOn() {
		if _, err := s.chiefAgent(octx); err == nil {
			return judgeFleet
		}
	}
	return judgeChief
}
