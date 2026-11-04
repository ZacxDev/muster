// Package sessionstate is one code-computed state per session of an owner's —
// needs_you, stalled or working — and the EPISODES those states open: one row
// per stop of one session (docs/design/session-state.md).
//
// What the owner sees about a session that needs them (the episode push, the
// Attention section's rows, check-ins) is a view of these, so they cannot
// disagree about who is stalled. This package holds the rules (Classify,
// Asked, Advance) and the store (session_episodes, migration 0072); the inputs
// are read by internal/api's stall facts (stallInputs), never a copy.
//
// 🔴 A FAILURE NEVER QUIETS A SESSION. A stall classifier that failed or did
// not run counts as "asked" (Asked), a host that dropped never ends an episode
// (the caller only ends one as gone from a host that is online and reporting),
// and nothing here depends on the chief.
package sessionstate

import "time"

// The states. `done` (a session that ended, or whose PRs are all merged) is a
// LABEL a reader adds, never a state: it can never hide a session.
const (
	StateWorking  = "working"
	StateNeedsYou = "needs_you"
	StateStalled  = "stalled"
)

// The session kinds, as the stall sweep keys sessions (chiefnudge.KindMirror,
// chiefnudge.KindAgent).
const (
	KindMirror = "mirror"
	KindAgent  = "agent"
)

// The reasons an episode ends.
const (
	EndWorked = "worked" // the session worked, continuously, for EndAfter
	EndGone   = "gone"   // its window left a host that is online and reporting status
)

// EndAfter is how long a session must work, continuously, before its episode
// ends (the design's N). A shorter blip — a permission let through, a quick
// tool call, the owner answering at the terminal — never ends one, so one
// stop is one episode.
const EndAfter = 120 * time.Second

// Facts is what Classify reads about one session, as the stall sweep read it.
type Facts struct {
	// Waiting: the host reports the session waiting on a prompt
	// (mirror.StatusWaiting).
	Waiting bool
	// Answerable: the wait is an ask muster itself can answer — a held
	// AskUserQuestion, an opencode question or permission request, a Hermes
	// approval with a prompt id. It stays an answerable ask with its own
	// full-text push, and opens NO episode.
	Answerable bool
	// Stalled: the stall rule (chiefnudge.Exclude) passed — idle past the
	// threshold, no background work, no turn error, no open ask — and the
	// session's last word is its own.
	Stalled bool
	// AskStall: the stall classifier scored the session as asking
	// (chiefnudge.AskStall). Read only when Stalled.
	AskStall bool
}

// Classify is the session's state, and whether that state opens (or keeps) an
// episode. Precedence: needs_you > stalled > working.
//
//   - waiting on a prompt muster cannot answer: needs_you, an episode;
//   - waiting on an answerable ask: needs_you, but NO episode (the ask is the
//     owner's item, with its own push);
//   - stalled and the classifier says it asked: needs_you, an episode;
//   - stalled otherwise: stalled, an episode;
//   - anything else: working.
func Classify(f Facts) (state string, episode bool) {
	switch {
	case f.Waiting && f.Answerable:
		return StateNeedsYou, false
	case f.Waiting:
		return StateNeedsYou, true
	case f.Stalled && f.AskStall:
		return StateNeedsYou, true
	case f.Stalled:
		return StateStalled, true
	}
	return StateWorking, false
}

// Asked is the leave rule's input: whether the session counts as having asked
// the owner something. A classifier that did not run (classified false) or
// failed (errClass not "") counts as asked, so its failure cannot quiet the
// session.
func Asked(classified bool, errClass string, askStall bool) bool {
	return !classified || errClass != "" || askStall
}

// Step is what one observation does to an open episode (Advance).
type Step struct {
	// End: the episode ends now, for Reason.
	End    bool
	Reason string
	// BusySince and BusySeen are the episode's new busy_since and busy_seen_at
	// (zero: not working).
	BusySince, BusySeen time.Time
}

// Advance is one observation, at now, of an open episode's session: busy
// reports whether it is working now; busySince and busySeen are the episode's
// stored first and latest sightings of its current run of work (zero: not
// seen working).
//
// The episode ends when the session has been SEEN working, continuously, for
// EndAfter: while it still works, or when it stops after a run whose first and
// last sightings are at least EndAfter apart (a turn that completed after that
// much work). Shorter work leaves it open and forgets the run.
//
// 🔴 MEASURED BETWEEN SIGHTINGS, NEVER TO NOW: a run is as long as the sweep saw
// it, so a session that worked 90 s and then sat idle until the next pass is
// not credited with the idle time. Between two passes the work is
// UNDER-counted by up to one interval, which keeps an episode open a little
// longer — never quieter.
func Advance(busy bool, busySince, busySeen, now time.Time) Step {
	switch {
	case busy && busySince.IsZero():
		return Step{BusySince: now, BusySeen: now}
	case busy && now.Sub(busySince) >= EndAfter:
		return Step{End: true, Reason: EndWorked, BusySince: busySince, BusySeen: now}
	case busy:
		return Step{BusySince: busySince, BusySeen: now}
	case !busySince.IsZero() && busySeen.Sub(busySince) >= EndAfter:
		return Step{End: true, Reason: EndWorked, BusySince: busySince, BusySeen: busySeen}
	}
	return Step{}
}
