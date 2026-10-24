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
//   - A MESSAGE THE LIMIT REJECTED GOES BACK TO THE HEAD (returnRejected),
//     marked returned and not counted as sent — but ONLY when the transcript
//     shows it was rejected rather than processed: the window's latest turn
//     ended in the limit, AFTER this paste (a status newer than the one it was
//     pasted on), and the prompt that turn answered is this message's text
//     (TurnError.Prompt, set by the host only when the limit was the prompt's
//     ONLY reply — no tool ran, nothing was written). A limit that hit
//     mid-turn means the message was processed: it stays sent. Anything the
//     host cannot show stays sent too. A double send is the worse failure.
//
//   - A CIRCUIT BREAKER HOLDS ANY OTHER IMMEDIATE-FAILURE LOOP: when the
//     latest BreakerAfter turns each ended within mirror.QuickTurnMS of their
//     prompt in an error the CLI wrote itself ("Not logged in", an API error),
//     the queue holds, saying so, until a real answer or a Send now.
//     Those messages are NOT returned: they were not a limit, and an error
//     the CLI names may have been processed in part.
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
	// ReturnWithin is how long after its claim a sent message may still be
	// found rejected and returned: a limit's rejection is written within a
	// second or two, and the host reports it within a refresh.
	ReturnWithin = 10 * time.Minute
)

// What a held queue says (Decision.Why).
const (
	// LimitHeldNoReset: a usage limit that names no reset time this can place.
	LimitHeldNoReset = "Held — usage limit; no reset time given. Resumes once Claude Code answers a message, or tap Send now."
	// ReturnedLimitDetail is the detail of a message returned to the queue
	// because a usage limit rejected it unprocessed.
	ReturnedLimitDetail = "returned: usage limit"
)

// LimitHeldPrefix is what a queue held by a usage limit that names its reset
// says before the time (internal/ui draws the same words, the time as a
// <time> in the reader's own zone).
const LimitHeldPrefix = "Held — usage limit, resumes "

// LimitHeldText is what a queue held by a usage limit that resets at
// resume says, in loc's time.
func LimitHeldText(resume time.Time, loc *time.Location) string {
	return LimitHeldPrefix + resume.In(loc).Format("3:04 PM")
}

// BreakerHeldText is what a queue the circuit breaker holds says.
func BreakerHeldText(e *mirror.TurnError) string {
	return fmt.Sprintf("Held — the last %d messages each failed at once (%s). Resumes once Claude Code answers a message, or tap Send now.",
		e.Quick, strings.ReplaceAll(e.Kind, "_", " "))
}

// holdFor is the hold the target's latest turn error puts on its queue at
// now: a usage limit until it resets (plus LimitMargin), or until a real
// answer when it names no reset; the breaker on BreakerAfter quick failures in
// a row. false: no hold — no error, a limit that has reset, or fewer quick
// failures. A limit that has reset is NOT then held by the breaker: the limit
// turns it counted were the limit's.
func holdFor(e *mirror.TurnError, now time.Time) (Decision, bool) {
	switch {
	case e == nil:
		return Decision{}, false
	case e.Kind == mirror.TurnErrorLimit && e.ResetsAt <= 0:
		return Decision{Why: LimitHeldNoReset, Held: true}, true
	case e.Kind == mirror.TurnErrorLimit:
		reset := time.UnixMilli(e.ResetsAt)
		if now.Before(reset.Add(LimitMargin)) {
			return Decision{Why: LimitHeldText(reset, time.Local), Held: true, ResumeAt: reset}, true
		}
		return Decision{}, false
	case e.Quick >= BreakerAfter:
		return Decision{Why: BreakerHeldText(e), Held: true}, true
	}
	return Decision{}, false
}

// Rejected reports whether sent message it was REJECTED unprocessed by a
// usage limit, as st shows its window now (see the file header): idle, on a
// status newer than the one it was pasted on, the latest turn ended in a usage
// limit, and that turn's prompt — the limit its only reply — is it's text.
func Rejected(it Item, st AgentState) bool {
	e := st.TurnError
	// (A digest is never "", so a turn that named no prompt never matches.)
	return it.State == Sent && st.Idle && st.Since > it.AfterSince && e != nil && e.Kind == mirror.TurnErrorLimit &&
		e.Prompt == mirror.PromptDigest(it.Body)
}

// returnRejected puts back at the head of its queue every message sent in the
// last ReturnWithin that a usage limit rejected unprocessed (Rejected), as
// queued, marked returned (ReturnedLimitDetail) and not counted as sent. A
// row is returned at most once per send: Return moves only a row still sent.
func (d *Dispatcher) returnRejected(ctx context.Context) {
	items, err := d.Store.RecentSent(ctx, d.now().Add(-ReturnWithin))
	if err != nil {
		d.logf("sendqueue: %v", err)
		return
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		st := d.Probe(d.AsOwner(ctx, it.Owner), Head{Item: it})
		if !Rejected(it, st) {
			continue
		}
		ok, err := d.Store.Return(ctx, it.ID, ReturnedLimitDetail)
		switch {
		case err != nil:
			d.logf("sendqueue: return message %d (%s): %v", it.ID, it.Target, err)
		case ok:
			d.logf("sendqueue: message %d (%d bytes) to %s (owner %d) was rejected by a usage limit before Claude Code processed it; "+
				"returned to the head of the queue, not counted as sent", it.ID, len(it.Body), it.Target, it.Owner)
		}
	}
}
