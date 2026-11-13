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
// Any other process running as the pod's user (one a tool call left behind) can
// call muster's MCP door with the agent's token. It is not a CLI, so it is not
// in CLIs; it is in Processes: ccd's scan of every live process it cannot
// account for (cmd/ccd strays.go), made while answering the request.

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
	// Processes is ccd's stray-process scan, made while answering this very
	// request (there is no cached copy to go stale). nil: a ccd from before
	// the scan — owner_asked refuses.
	Processes *ProcessReport `json:"processes,omitempty"`
}

// ProcessReport is ccd's scan for stray processes: live processes in the pod
// that are not ccd, its ancestors, the CLIs it started, their tmux server, or
// the few direct children it allows (its own helpers, hooks, the stdio MCP
// server it registers). In practice a stray is something a tool call started
// and left running.
type ProcessReport struct {
	// Scanned: the scan completed. False (with Error) when it could not be
	// trusted — owner_asked refuses.
	Scanned bool   `json:"scanned"`
	Error   string `json:"error,omitempty"`
	// Strays are the live processes the scan could not account for.
	Strays []StrayProcess `json:"strays,omitempty"`
}

// StrayProcess is one process ProcessReport could not account for. Comm is the
// process's own name, chosen by whoever started it: log it, never trust it.
type StrayProcess struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	Comm string `json:"comm"`
}

// CLIStatusIdle is the one sessions-file status that means "not working".
const CLIStatusIdle = "idle"
