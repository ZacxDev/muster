package agents

// THE PODS' CLIs, AS THEY SAY THEMSELVES: ccd's GET /v1/clis.
//
// A claude-code agent's pod runs one Claude Code CLI per chat session (ccd's
// lanes) plus its primary, side by side, and they all call muster's MCP tools
// with ONE credential: a tool call does not say which CLI made it. So muster's
// owner_asked (internal/api chief_owner_release.go) asks ccd which CLIs are
// working, judged by each CLI ITSELF — the status Claude Code writes in its
// sessions file (busy, waiting, shell, idle) for every live process — and not
// by ccd's or muster's bookkeeping, so a turn ccd gave up on that the CLI is
// still running, or one the CLI started by itself, counts as working. A live
// CLI process with no sessions file is listed as Unaccounted, never left out.
//
// ⚠ CLAUDE CODE CLIs ONLY. Any other process running as the pod's user (one a
// tool call left behind) can call muster's MCP door with the agent's token and
// is not listed: ccd treats such processes as out of its reach.

// CLIActivityVersion is the shape of the answer below.
const CLIActivityVersion = 1

// CLIState is one live CLI in the pod.
type CLIState struct {
	// Lane is the lane's session key (the HeaderCCSession its turns carry);
	// "" for the primary CLI and for a CLI ccd does not know.
	Lane string `json:"lane"`
	// Primary: ccd's primary CLI (turns with no HeaderCCSession).
	Primary bool `json:"primary"`
	// Known: ccd matched it to the primary or one of its lanes. An unknown CLI
	// (a background job's process, anything else in the pod) is still listed.
	Known bool `json:"known"`
	// PID is the CLI's process id in the pod.
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	// Status is the sessions file's status as Claude Code wrote it; only
	// "idle" means idle.
	Status string `json:"status"`
	// Unaccounted: a live CLI process with no matching sessions file, so no
	// status of its own — it may be working, and counts as working.
	Unaccounted bool `json:"unaccounted"`
}

// CLIActivity is GET /v1/clis's answer.
type CLIActivity struct {
	Version int        `json:"version"`
	CLIs    []CLIState `json:"clis"`
}

// CLIStatusIdle is the one sessions-file status that means "not working".
const CLIStatusIdle = "idle"

// LaneVerdict is OnlyTheOwnersLaneWorks's answer.
type LaneVerdict string

// The verdicts: LaneVerdictOK passes; each other one is a refusal.
const (
	LaneVerdictOK LaneVerdict = ""
	// LaneVerdictUnaccounted: a live CLI (or opencode) process ccd cannot
	// account for may be working.
	LaneVerdictUnaccounted LaneVerdict = "unaccounted"
	// LaneVerdictOtherBusy: another CLI or opencode lane is working.
	LaneVerdictOtherBusy LaneVerdict = "other_busy"
	// LaneVerdictOwnerMissing: the owner's own lane is not in the answer, so
	// nothing the answer says about the others is trusted.
	LaneVerdictOwnerMissing LaneVerdict = "owner_missing"
)

// OnlyTheOwnersLaneWorks is THE check behind owner_asked's "only the owner's
// turn is running in the pod" (internal/api chief_owner_release.go): of the
// CLIs in act, the owner's lane — lane, "" for ccd's primary CLI — must be
// listed, and every OTHER entry idle and accounted for. An opencode lane is
// an entry like any other (ccd lists it, cmd/ccd opencode.go), so a busy one
// refuses a call from any other lane.
func OnlyTheOwnersLaneWorks(act CLIActivity, lane string) LaneVerdict {
	found := false
	for _, c := range act.CLIs {
		own := c.Known && ((lane == "" && c.Primary) || (lane != "" && !c.Primary && c.Lane == lane))
		if own {
			found = true
			continue
		}
		if c.Unaccounted {
			return LaneVerdictUnaccounted
		}
		if c.Status != CLIStatusIdle {
			return LaneVerdictOtherBusy
		}
	}
	if !found {
		return LaneVerdictOwnerMissing
	}
	return LaneVerdictOK
}
