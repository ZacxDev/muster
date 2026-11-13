// Package sessionstate is one code-computed state per session of an owner's —
// needs_you, stalled or working — and the EPISODES those states open: one row
// per stop of one session (docs/design/session-state.md).
//
// What the owner sees about a session that needs them (the episode push, the
// Attention section's rows, check-ins) is a view of these, so they cannot
// disagree about who is stalled. This package holds the rules (StateOf,
// Asked, Advance, OwnerActed, Handled) and the store (session_episodes, migration
// 0073); the inputs
// are read by internal/api's stall facts (stallInputs), never a copy.
//
// 🔴 A FAILURE NEVER QUIETS A SESSION. A stall classifier that failed or did
// not run counts as "asked" (Asked), a host that dropped never ends an episode
// (the caller only ends one as gone from a host that is online and reporting),
// and nothing here depends on the chief.
package sessionstate

import (
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
)

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
	EndGone   = "gone"   // its window left a host that is online and reporting status, for several passes (api: episodeGoneMisses)
	EndOwner  = "owner"  // the owner acted in the session after it opened (OwnerActed)
	// EndViewed: the owner opened the session's own page in muster (its
	// rendered view, its terminal, an agent's conversation) after it opened.
	// Its own reason, not EndOwner's: the end reason is the record of WHY a
	// push cleared, and "the owner looked" is not "the owner answered".
	EndViewed = "viewed"
	// EndReset is written by migration 0079: every episode open when it
	// applied, ended once so the backlog of stops opened before an episode
	// could end on the owner's action is withdrawn. Muster itself writes it in
	// one place only: PG.EndRepeat, ending an episode another process (an
	// older image, mid-rollout) opened again on the streak a reset episode
	// ended on. PG.End refuses it.
	EndReset = "reset"
)

// Handled reports whether an episode that ended for reason was HANDLED on the
// streak it ended on: the owner opened the session in muster ('viewed'), or the
// backlog reset ended it ('reset'). 🔴 A HANDLED STOP IS NOT OPENED AGAIN ON THE
// SAME STREAK (PG.Open), and an episode another process opened on it anyway is
// ended again (PG.EndRepeat): the session has not moved since, so a new episode
// would push the same stop the owner already dealt with.
//
// 🔴 'owner' IS NOT HANDLED, ON PURPOSE: it means the owner ACTED — answered —
// so the session moved, and any stop after it is new. The episode is ended on
// the pass that READ the owner's word, which can be later than the pass that
// first saw the session's next stop (a failed read is retried), so the streak
// an 'owner' episode ends carrying can be the NEXT stop's. Treating it as
// handled would suppress that real stop's push until the session moved again.
// 'viewed' and 'reset' carry no such lag: a view ends the episode on the streak
// the owner was shown (episodeViewed), and the reset on the one last observed.
// A stop that ended because the session worked ('worked') or left ('gone') is
// not handled either — its session moved.
func Handled(reason string) bool {
	for _, r := range handledReasons {
		if reason == r {
			return true
		}
	}
	return false
}

// handledReasons is Handled's set, as PG.Open's and PG.EndRepeat's guards read
// it.
var handledReasons = []string{EndViewed, EndReset}

// resumedReasons is the end reasons whose stop, seen again on the SAME
// streak, is that same stop back: PG.Open writes the new episode carrying the
// ended one's pushed_at, so a stop already pushed is never pushed again, and
// linking the stop's first episode (Episode.ResumedFrom), so a chief's
// decision on it — a sent nudge, a "leave" on a stop that asked nothing —
// still quiets it (api streakDecision). 🔴
// THE ONE PLACE THIS RULE IS REASONED (pointers: PG.Open, session_push.go,
// session_state.go; docs/design/session-state.md, "gone").
//
// 🔴 ONLY 'gone'. A connected host's listing can leave a window out and list
// it again unchanged (measured on a live fleet, about hourly: every idle
// window of one host ended gone in one pass, opened again on its unchanged
// streak the next, and was pushed again once the chief's wait ran out — a
// burst of the same stops). The pass now ends an episode gone only after
// several consecutive missing passes (episodeGoneMisses, internal/api), so a
// one-pass omission ends nothing; this carry is the BACKSTOP for an omission
// longer than that. The streak is the host's own status time (or an agent's
// newest message), so an EQUAL streak means the session has not run since —
// any turn, a resume or a new process writes a new one, and a new
// conversation in a new window is a new session key altogether. The other
// reasons are not the same stop: 'worked' and 'owner' mean the session moved
// (see Handled for why 'owner' can still carry an unchanged streak), and
// 'viewed'/'reset' never reopen on their streak at all (Handled).
//
// 🔴 IT CARRIES WHAT THE STOP HAD, NEVER A NEW SILENCE: a stop that was never
// pushed carries no push; one the chief never judged carries no decision —
// held in its grace or the chief's wait, its return is pushed exactly as a
// new stop would be (the hold delays, it never silences). Only a decision the
// chief already took on this very stop and streak carries over. The first push's notification is closed when its episode ends
// gone and is not shown again; the stop stays on the Asks page.
var resumedReasons = []string{EndGone}

// EndAfter is how long a session must work, continuously, before its episode
// ends on its own (the design's N). A shorter blip — a quick tool call, a
// background task's notice — does not end one, so one stop of a session that
// carries on by itself is one episode. The OWNER acting in the session
// (OwnerActed) or opening it in muster (EndViewed) ends one at once.
const EndAfter = 120 * time.Second

// OwnerFacts is what OwnerActed reads about one open episode's session.
type OwnerFacts struct {
	// OpenedAt is when the episode opened.
	OpenedAt time.Time
	// WasWaiting: the episode waits on a prompt muster cannot answer — it
	// opened, or was last seen, waiting (Episode.Waiting).
	WasWaiting bool
	// Known: the session's status is known now. WaitingNow: it waits on a
	// prompt now.
	Known, WaitingNow bool
	// Msgs is the session's conversation as read now, oldest first; nil when
	// it was not read. Injected and Prefixes are what muster itself put into
	// it — its nudges, relays and routed messages — as chiefnudge.OwnerMessages
	// reads them.
	Msgs               []chiefnudge.Msg
	Injected, Prefixes []string
}

// OwnerActed reports whether an open episode ends because its OWNER acted in
// the session after it opened (EndOwner) — the owner went to the session and
// answered it, so the push and the Stalled / waiting row are done with:
//
//   - a waiting episode whose session no longer waits: the prompt it waited
//     on was answered;
//   - an owner message in the conversation written AFTER the episode opened.
//     Only the owner's own words count (chiefnudge.OwnerMessages): a chief
//     nudge, a relayed answer or a routed message muster pasted in, a fenced
//     message, or a message with no time is not the owner acting.
//
// 🔴 STRICTLY AFTER OpenedAt: the owner's message that preceded the stop (an
// Esc-interrupted turn, the owner_last case) is at or before the opening, and
// leaves the episode open until a NEWER one arrives. 🔴 A FAILURE NEVER QUIETS
// A SESSION: a status that is unknown, or a session that waits now, ends
// nothing — whatever the conversation says.
//
// The owner OPENING the session in muster is the other way they end it
// (EndViewed), recorded where the session's own page is rendered
// (internal/api's episodeViewed), not read here.
func OwnerActed(f OwnerFacts) bool {
	if !f.Known || f.WaitingNow {
		return false
	}
	if f.WasWaiting {
		return true
	}
	for _, m := range chiefnudge.OwnerMessages(f.Msgs, f.Injected, f.Prefixes, f.OpenedAt, false) {
		if m.At.After(f.OpenedAt) {
			return true
		}
	}
	return false
}

// Facts is what StateOf reads about one session, as the stall sweep read it.
type Facts struct {
	// Waiting: the host reports the session waiting on a prompt
	// (mirror.StatusWaiting).
	Waiting bool
	// Stalled: the stall rule (chiefnudge.Exclude) passed — idle past the
	// threshold, no background work, no turn error, no open ask — and the
	// session's last word is its own.
	Stalled bool
	// AskStall: the stall classifier scored the session as asking
	// (chiefnudge.AskStall). Read only when Stalled.
	AskStall bool
}

// StateOf is the session's state, and whether that state opens (or keeps) an
// episode. Precedence: needs_you > stalled > working.
//
//   - waiting on a prompt: needs_you, an episode — 🔴 AN ANSWERABLE ASK TOO
//     (a held AskUserQuestion, an opencode question or permission request).
//     Its push waits the same grace as any
//     prompt's (operator decision after #463): until then an answerable ask
//     was no episode and pushed at once, and nearly every one was answered at
//     the terminal within a minute;
//   - stalled and the classifier says it asked: needs_you, an episode;
//   - stalled otherwise: stalled, an episode;
//   - anything else: working.
func StateOf(f Facts) (state string, episode bool) {
	switch {
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
// not credited with the idle time. The cost runs the other way: between two
// passes the work is UNDER-counted by up to one interval, so a real turn of
// 120–150 s can be seen as 90–120 s and leave its stop open. Its NEXT stop then
// opens no new episode — and since episodes push (internal/api
// session_push.go), that next stop gets no push of its own: the open episode
// already pushed. That is QUIETER, not louder, and it is ACCEPTED, not fixed:
// the cost is bounded to a turn of 120–150 s, and its window still shows the
// stop as a row on the Asks page.
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
