package agentspec

import (
	"strings"
	"testing"
)

// TestTheInstructionsAndTheDaemonReferToEachOther is the guard the tripwire's own
// comment asked for by saying "Move both or neither".
//
// 🔴 IT PINS A RELATIONSHIP ACROSS TWO ARTEFACTS, WHICH IS THE ONLY SHAPE THAT
// CATCHES THIS. The hazard is not either artefact being wrong on its own — it is
// one of them being deleted, renamed or repathed while the other keeps talking
// about it. The instruction prose is the only thing that tells a dispatched agent
// the rescue log exists, so if the daemon's log path ever changes and this text does
// not, an agent hitting a push blocker is sent to read a file that is not there and
// reports "no autosave log" — which reads exactly like a daemon that never ran.
func TestTheInstructionsAndTheDaemonReferToEachOther(t *testing.T) {
	if !strings.Contains(WorkerInstructions, autosaveLogPath) {
		t.Errorf("the worker instructions do not name the daemon's log path %q.\n"+
			"That prose is the ONLY thing telling an agent the rescue log exists; without it the "+
			"daemon ships and nobody is told to look at it, which is what the carried-forward-debt "+
			"tripwire meant by \"move both or neither\".", autosaveLogPath)
	}

	// Control: the path must be non-trivial, or Contains would pass vacuously.
	if len(autosaveLogPath) < 5 || !strings.HasPrefix(autosaveLogPath, "/") {
		t.Fatalf("autosaveLogPath = %q is too trivial for the check above to mean anything", autosaveLogPath)
	}

	// And the daemon must actually write there, or the instruction points at a file
	// nothing creates. Read from the script, not from the constant — comparing the
	// constant to itself proves nothing.
	if !strings.Contains(autosaveScript, autosaveLogPath) {
		t.Errorf("autosave.sh does not mention %q, so the instructions send the agent to a file "+
			"the daemon never writes", autosaveLogPath)
	}
}

// TestTheInstructionsDoNotPromiseASelfServicePrivilegePath is the guard for the one
// section deliberately DROPPED on the way across.
//
// 🔴 THE HAZARD IS A 2XX THAT CHANGES NOTHING. muster registers
// /agent/privilege/request but the privilege applier stayed with the permission
// router by operator decision, so a request recorded here is never applied. Prose
// telling an agent to ask would earn it a success reply, no permissions, and — worst
// of all — a reason to stop looking for the real blocker. The instruction set must
// route that case to the blocked protocol instead.
func TestTheInstructionsDoNotPromiseASelfServicePrivilegePath(t *testing.T) {
	// Each of these is a way the upstream section could come back. The route path is
	// the decisive one; the others catch a reworded reintroduction.
	for _, forbidden := range []string{
		"/agent/privilege/request",
		"privilege/request",
		"Request elevated access",
	} {
		if strings.Contains(WorkerInstructions, forbidden) {
			t.Errorf("the worker instructions contain %q, promising a self-service privilege path.\n"+
				"muster records such a request and applies NOTHING — the applier stayed with the "+
				"permission router — so the agent gets a success that changes nothing and stops "+
				"looking for the real blocker. Route this case to \"Blocked and cannot proceed\" "+
				"instead, and only restore the section when a privilege applier lands in muster.", forbidden)
		}
	}

	// 🔴 AND THE POSITIVE HALF, WITHOUT WHICH THIS IS JUST AN ABSENCE. The capability
	// must be covered SOMEWHERE, or dropping the section quietly lost it. The blocked
	// protocol has to name the access case explicitly.
	if !strings.Contains(WorkerInstructions, "Blocked and cannot proceed") {
		t.Fatal("the blocked-close-out protocol is missing, so the dropped privilege section's " +
			"cases are covered nowhere")
	}
	if !strings.Contains(WorkerInstructions, "access you do not have and cannot grant yourself") {
		t.Error("the blocked protocol does not name the missing-access case, so an agent needing " +
			"elevated access is told neither to request it nor to report it — the capability was " +
			"lost rather than relocated")
	}
}

// TestTheInstructionsNameTheOneBaseURLAndNotTwo pins the other genericisation.
func TestTheInstructionsNameTheOneBaseURLAndNotTwo(t *testing.T) {
	if !strings.Contains(WorkerInstructions, EnvAPIURL) {
		t.Errorf("the instructions do not name %s, so an agent hand-rolling a curl has no base URL", EnvAPIURL)
	}
	if !strings.Contains(WorkerInstructions, EnvToken) {
		t.Errorf("the instructions do not name %s, so an agent has no credential to present", EnvToken)
	}

	// 🔴 A LEDGER OVER EVERY ENV REFERENCE, not a check that one old name is absent.
	// Rejecting the upstream's own task-side variable name would be walkable by
	// writing "MUSTER_TASK_API_URL" instead — the shape, not the spelling, is the
	// hazard, so this matches on the SHAPE.
	for _, line := range strings.Split(WorkerInstructions, "\n") {
		if strings.Contains(line, "TASK_API_URL") || strings.Contains(line, "TWO base URLs") {
			t.Errorf("the instructions describe a second, task-side base URL: %q\n"+
				"muster serves both route families from its own mux, so that hazard cannot occur "+
				"here and the warning would be prose asserting a shape the code does not have.", line)
		}
	}
}

// TestTheInstructionsNameTheInPodCLI catches a half-done rename, which is worse than
// none: an agent told to run a binary that is not on its PATH reports the task as
// blocked on tooling.
//
// 🔴 IT DOES NOT CHECK FOR THE UPSTREAM PRODUCT'S NAME, AND THAT IS A CONSOLIDATION
// RATHER THAN A GAP. An earlier draft did, and could not: tests/leakscan.py refuses
// that identifier ANYWHERE in this repository, including inside a test asserting its
// absence — so the guard could not be written without tripping the gate it
// duplicated. leakscan is also the better owner, because it covers all 260 files
// rather than this one string, and it fails the build rather than one package.
// Checking the POSITIVE here — that the right commands are present — is the half
// leakscan cannot do.
func TestTheInstructionsNameTheInPodCLI(t *testing.T) {
	// The commands it teaches must be the ones the CLI actually has. Spelled out
	// rather than derived: this is a contract with cmd/muster's verb names.
	for _, cmd := range []string{
		"muster agent task get",
		"muster agent task comment",
		"muster agent task status",
		"muster health",
	} {
		if !strings.Contains(WorkerInstructions, cmd) {
			t.Errorf("the instructions do not teach %q", cmd)
		}
	}
}

// TestTheInstructionsRefuseToTeachAStatusTheAgentCannotSet guards a specific way
// this prose can lie: promising `complete`, which the CLI and the server both refuse.
func TestTheInstructionsRefuseToTeachAStatusTheAgentCannotSet(t *testing.T) {
	if !strings.Contains(WorkerInstructions, "You CANNOT set") {
		t.Error("the instructions do not tell the agent which status it may not set, so it will " +
			"try `complete` and read the refusal as a bug")
	}
	// The three it MAY set must all be named, or an agent avoids one it is allowed.
	for _, status := range []string{"open", "in_progress", "ready_for_review"} {
		if !strings.Contains(WorkerInstructions, status) {
			t.Errorf("the instructions do not name the settable status %q", status)
		}
	}
}
