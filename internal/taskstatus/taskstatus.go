// Package taskstatus is the SINGLE definition of a task's lifecycle vocabulary.
//
// WHY IT IS ITS OWN PACKAGE, AND WHY IT IMPORTS NOTHING
// -----------------------------------------------------
// The vocabulary was previously owned by internal/notes, which is the right
// home semantically but the wrong one structurally: notes also carries the
// pgx-backed store, so anything wanting the four constants had to link a
// Postgres driver. A CLI declined to pay that (+448 KiB, measured) and kept its
// OWN copy of the four strings instead, guarded by a test that compared the two.
//
// 🔴 That guard was ONE-DIRECTIONAL and the drift it could not see was the
// dangerous direction. It failed if the CLI's copy changed, but adding a fifth
// constant to notes left the whole suite green — at which point the CLI would
// refuse a status CLIENT-SIDE, at exit 2, against a server that would have
// accepted it. Found by adversarial audit of #323, by mutation.
//
// A test that pins two copies together is strictly worse than not having two
// copies. This package has no imports, so every consumer — the store, the HTTP
// layer, and a CLI that never opens a database — can share one definition at no
// cost. There is nothing left to drift.
//
// internal/notes re-exports these as notes.Status* / notes.ValidStatus so the
// 35 files already using that spelling are untouched.
package taskstatus

// The four task lifecycle states.
//
// Complete is privileged: only the operator/human may set it. Agents are
// restricted to the working states — see AllowedForAgent, and note that the
// in-workspace agent route enforces it while the machine route deliberately does
// not.
const (
	Open           = "open"
	InProgress     = "in_progress"
	ReadyForReview = "ready_for_review"
	Complete       = "complete"
)

// all is an ARRAY, not a slice: an array is a value type, so it cannot be
// aliased and mutated through a returned reference the way a package-level
// slice can. Order is the order a user-facing message should list them in,
// which is the lifecycle order rather than alphabetical.
var all = [...]string{Open, InProgress, ReadyForReview, Complete}

// All returns the vocabulary, in lifecycle order.
//
// It returns a COPY. A caller that sorts or truncates the result — a help
// string, an error message, a test table — must not be able to reorder the
// vocabulary for every other caller in the process.
func All() []string {
	return append([]string(nil), all[:]...)
}

// Valid reports whether s is a known task status.
//
// 🔴 It iterates `all` rather than switching on the constants. That is the
// whole point: a switch is a SECOND enumeration, and the two can disagree — a
// status could be added to the list and not the switch, and Valid would reject
// a value All advertises. Deriving one from the other makes that
// unrepresentable rather than merely tested.
func Valid(s string) bool {
	for _, v := range all {
		if s == v {
			return true
		}
	}
	return false
}

// AllowedForAgent reports whether an agent (as opposed to the operator or
// human) may set status s. Agents move a task through the working states but
// may not declare it complete.
//
// Expressed as "valid, and not the privileged one" so that a status added to
// the vocabulary is automatically agent-allowed unless it is deliberately
// excluded here. The alternative — a second whitelist — silently denies agents
// every new status, which is a failure mode that looks like a permissions bug.
func AllowedForAgent(s string) bool {
	return Valid(s) && s != Complete
}

// Rank returns s's position in the lifecycle (Open=0 … Complete=3), or -1 for
// an unknown status.
//
// It is derived from `all` for the same reason Valid is: a hand-written switch
// would be a SECOND enumeration, and a fifth status added to the vocabulary and
// not to the switch would silently rank -1 — which IsDowngrade reads as "not a
// downgrade", i.e. the guard goes quiet exactly where a new state was added.
func Rank(s string) int {
	for i, v := range all {
		if s == v {
			return i
		}
	}
	return -1
}

// IsDowngrade reports whether moving a task from old to new walks the lifecycle
// BACKWARDS.
//
// 🔴 WHY THIS EXISTS. An agent set a task `ready_for_review`, and ~11 s later
// something put it back to `in_progress` with no trace in any log — the
// completion signal was lost and the agent rationalised the loss rather than
// retrying. A backwards move is the shape that ate it, so the one choke point
// that logs status writes raises its level for exactly this predicate (see
// notes.WithStatusLogging).
//
// Both statuses must be KNOWN. An unknown one (including the "UNKNOWN"
// placeholder the choke point uses when it could not read the previous status)
// ranks -1 and is never called a downgrade: a comparison against an absent
// operand must not manufacture a WARNING.
//
// It is deliberately WIDER than the one observed shape: the incident was
// ready_for_review→in_progress, but in_progress→open is the same class of
// event, and a predicate enumerating only the pairs that already bit us cannot
// see the next one.
func IsDowngrade(old, next string) bool {
	o, n := Rank(old), Rank(next)
	if o < 0 || n < 0 {
		return false
	}
	return n < o
}
