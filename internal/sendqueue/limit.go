package sendqueue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// 🔴 A TURN THAT FAILED AT ONCE IS NOT A TURN THAT ENDED. When Claude Code
// hits a usage limit it ends the turn with a record it wrote itself ("You've
// hit your session limit · resets 10:30pm (…)") and goes back to its prompt,
// so its status reads idle with a NEWER time — exactly what Decide waits for
// after a paste. Before this, the queue then sent the next message, which the
// limit rejected the same way, and the next, until the queue was empty: every
// message consumed by the limit and lost. The host now reads how the latest
// turn ended from the transcript (mirror.TurnError), and:
//
//   - A USAGE LIMIT HOLDS THE QUEUE (holdFor). Until the reset time it names
//     plus LimitMargin; with no reset time it can place, until a real answer
//     shows the limit has lifted (the host then reports no TurnError) or the
//     owner taps Send now. A real answer lifts it at once, reset or not.
//
//   - THE ONE MESSAGE THE LIMIT BOUNCED IS MARKED FAILED (rejectBounced), with
//     RejectedLimitDetail and the row's ordinary Retry — by hand, the
//     operator's decision; nothing re-sends it on its own. Only when the window
//     shows it was this send the limit refused: the latest turn ended in the
//     limit AFTER this paste (a status newer than the one it was pasted on),
//     nothing was pasted through muster since (Pasted), and the limit was that
//     turn's ONLY reply (TurnError.Rejected: no tool ran, nothing was written).
//     A limit that hit mid-turn means the message was processed: it stays
//     sent. A Retry by hand of a message that was in fact processed would run
//     it twice, so anything the window cannot show stays sent.
//
//   - A CIRCUIT BREAKER HOLDS ANY OTHER IMMEDIATE-FAILURE LOOP: when the
//     latest BreakerAfter turns each ended within mirror.QuickTurnMS of their
//     prompt in an error the CLI wrote itself ("Not logged in", an API error),
//     the queue holds, saying so, and lifts by itself after a back-off
//     (BreakerBackoff: 1, 2, 5, then every 15 minutes) counted from when muster
//     first saw that failure. Each message it lets through that fails at once
//     too counts one more and backs off further; a real answer resets it.
//
// Send now is the override throughout: it bypasses Decide and sends exactly
// the one message it was tapped on (Dispatcher.SendNow).

const (
	// LimitMargin is how long after a usage limit's reset the queue waits
	// before sending: the reset is the limit's own minute, and a clock a few
	// seconds off either side must not send into it.
	LimitMargin = time.Minute
	// BreakerAfter is how many turns in a row must each fail at once before
	// the breaker holds the queue.
	BreakerAfter = 2
	// BounceWithin is how long after its claim a sent message may still be
	// found bounced: a limit's rejection is written within a second or two,
	// and the host reports it within a refresh.
	BounceWithin = 10 * time.Minute
)

// BreakerBackoff is how long the breaker holds: the first trip (BreakerAfter
// quick failures in a row) for the first entry, each further quick failure the
// next, the last repeating. Counted on muster's clock from when muster first
// saw the failing turn's status (AgentState.Observed), never the host's.
var BreakerBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// What a held queue says (Decision.Why). The time it resumes, when there is
// one, is Decision.ResumeAt — drawn after these words in the READER's zone
// (internal/ui), never in the server's.
const (
	// LimitHeldText: a usage limit that names its reset.
	LimitHeldText = "Held — usage limit, resumes"
	// LimitHeldNoReset: a usage limit that names no reset time this can place.
	LimitHeldNoReset = "Held — usage limit; no reset time given. Resumes once Claude Code answers a message, or tap Send now."
	// RejectedLimitDetail is the detail of a sent message a usage limit
	// bounced, marked failed (Reject).
	RejectedLimitDetail = "Rejected: usage limit — Claude Code refused it before working on it. Retry it once the limit resets."
)

// BreakerHeldText is what a queue the circuit breaker holds says, before the
// time it retries.
func BreakerHeldText(e *mirror.TurnError) string {
	return fmt.Sprintf("Held — the last %d messages each failed at once (%s); retries at", e.Quick, strings.ReplaceAll(e.Kind, "_", " "))
}

// breakerBackoff is the breaker's hold for quick failures in a row.
func breakerBackoff(quick int) time.Duration {
	return BreakerBackoff[min(max(quick-BreakerAfter, 0), len(BreakerBackoff)-1)]
}

// holdFor is the hold st's latest turn error puts on its queue at now: a
// usage limit until it resets (plus LimitMargin), or until a real answer when
// it names no reset; the breaker on BreakerAfter quick failures in a row,
// until its back-off has run from st.Observed. false: no hold — no error, a
// limit that has reset, a back-off that has run, or fewer quick failures. A
// limit that has reset is NOT then held by the breaker: the quick turns it
// counted were the limit's.
func holdFor(st AgentState, now time.Time) (Decision, bool) {
	e := st.TurnError
	switch {
	case e == nil:
		return Decision{}, false
	case e.Kind == mirror.TurnErrorLimit && e.ResetsAt <= 0:
		return Decision{Why: LimitHeldNoReset, Held: true}, true
	case e.Kind == mirror.TurnErrorLimit:
		reset := time.UnixMilli(e.ResetsAt)
		if now.Before(reset.Add(LimitMargin)) {
			return Decision{Why: LimitHeldText, Held: true, ResumeAt: reset}, true
		}
		return Decision{}, false
	case e.Quick >= BreakerAfter:
		retry := st.Observed.Add(breakerBackoff(e.Quick))
		if now.Before(retry) {
			return Decision{Why: BreakerHeldText(e), Held: true, ResumeAt: retry}, true
		}
	}
	return Decision{}, false
}

// Bounced reports whether sent message it was REFUSED by a usage limit before
// Claude Code worked on it, as st shows its window now (see the file header):
// idle, on a status newer than the one it was pasted on, nothing pasted
// through muster since, and the latest turn ended in a usage limit that was
// its prompt's only reply.
func Bounced(it Item, st AgentState) bool {
	e := st.TurnError
	return it.State == Sent && st.Idle && st.Since > it.AfterSince && st.Pasted <= it.AfterSince &&
		e != nil && e.Kind == mirror.TurnErrorLimit && e.Rejected
}

// rejectBounced marks failed (Reject, RejectedLimitDetail) every message sent
// in the last BounceWithin that a usage limit bounced (Bounced). A row is
// rejected at most once: Reject moves only a row still sent.
func (d *Dispatcher) rejectBounced(ctx context.Context) {
	items, err := d.Store.RecentSent(ctx, d.now().Add(-BounceWithin))
	if err != nil {
		d.logf("sendqueue: %v", err)
		return
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		if !Bounced(it, d.Probe(d.AsOwner(ctx, it.Owner), Head{Item: it})) {
			continue
		}
		ok, err := d.Store.Reject(ctx, it.ID, RejectedLimitDetail)
		switch {
		case err != nil:
			d.logf("sendqueue: reject message %d (%s): %v", it.ID, it.Target, err)
		case ok:
			d.logf("sendqueue: message %d (%d bytes) to %s (owner %d) was refused by a usage limit before Claude Code worked on it; "+
				"marked failed for a Retry by hand", it.ID, len(it.Body), it.Target, it.Owner)
		}
	}
}
