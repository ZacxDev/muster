package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// A TURN THAT ENDED IN AN ERROR (Window.TurnError): the host's reading of an
// IDLE Claude window's transcript, when the latest turn in it ended in a record
// Claude Code wrote itself to say it got no answer — a usage limit ("You've hit
// your session limit · resets …"), "Not logged in", an API error
// (cctranscript.ErrorEndOf). nil when the latest turn ended in a real answer,
// or has not ended.
//
// 🔴 WHY THE SEND QUEUE NEEDS IT: such a turn ends in a second or so and
// Claude Code goes back to its prompt, so its status reads idle with a newer
// time — exactly what a turn that was answered looks like. Without this the
// queue sent the next message into the same wall, and the next, until the
// queue was empty: every message consumed by a limit and lost
// (internal/sendqueue's Decide holds on it now).
//
// 🔴 BACKWARD-COMPATIBLE BOTH WAYS, like Pending: an older muster ignores it;
// an older host never sends it, and its windows' queues behave as they did.
//
// ⚠ SIZE: it rides the window list, ONE message within MaxFrame
// (TestTheLargestWindowListFitsOneFrame). So it carries no text — a kind, two
// times, a count and a digest, every field a closed alphabet that JSON writes
// as itself — and at most MaxTurnErrorWindows windows of a list carry one.
type TurnError struct {
	// Kind is TurnErrorLimit for a usage limit, else the record's own error
	// code ("authentication_failed", "server_error", …: ValidTurnErrorKind),
	// TurnErrorOther when it names none.
	Kind string `json:"kind"`
	// At is when the record was written: its timestamp, Unix ms ON THE HOST'S
	// CLOCK (the CLI's).
	At int64 `json:"at,omitempty"`
	// ResetsAt is when a usage limit resets, Unix ms: an absolute time the
	// limit itself names (cctranscript.LimitReset), compared with muster's
	// clock. 0 when it names none, and for every other kind.
	ResetsAt int64 `json:"resetsAt,omitempty"`
	// Prompt is PromptDigest of the prompt the turn answered, set ONLY when
	// that prompt got no real answer at all — the error is the turn's only
	// reply, no tool ran, nothing was written. It is how the queue knows a
	// message it sent was REJECTED rather than processed, and so may go back
	// to the queue without being sent twice. "" when any real reply came
	// first (the limit hit mid-turn: the message was processed) or no prompt
	// was found.
	Prompt string `json:"prompt,omitempty"`
	// Quick counts the latest turns, this one first, that EACH ended in such a
	// record within QuickTurnMS of their prompt with no real reply between:
	// the send queue's circuit breaker. 0 when this turn was not one.
	Quick int `json:"quick,omitempty"`
}

// The turn error kinds muster names; any other is the CLI's own code.
const (
	// TurnErrorLimit: a usage limit.
	TurnErrorLimit = "usage_limit"
	// TurnErrorOther: an error record that names no code.
	TurnErrorOther = "error"
)

// The TurnError bounds.
const (
	// QuickTurnMS is how soon after its prompt an error must end a turn to
	// count as an immediate failure (TurnError.Quick): measured 0.3–1.0 s from
	// prompt to limit row on real transcripts; a turn that ran longer did work.
	QuickTurnMS = 10_000
	// MaxTurnErrorQuick bounds TurnError.Quick (the host stops counting).
	MaxTurnErrorQuick = 20
	// MaxTurnErrorWindows bounds how many windows of ONE list carry a
	// TurnError.
	MaxTurnErrorWindows = 16
)

var turnErrorKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidTurnErrorKind reports whether s is a kind the wire carries.
func ValidTurnErrorKind(s string) bool { return turnErrorKind.MatchString(s) }

var promptDigest = regexp.MustCompile(`^[0-9a-f]{16}$`)

// PromptDigest is what names a prompt's text on the wire (TurnError.Prompt):
// the first 8 bytes of the SHA-256 of the text with its surrounding
// whitespace trimmed and CRLF read as LF — the host digests the prompt as the
// transcript holds it, muster the message it sent, and the two must agree on
// what was pasted however the terminal carried the line ends.
func PromptDigest(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// CleanTurnError is e as the wire carries it: nil for no kind or one the wire
// does not carry, no negative time, a reset only on a limit, a digest only in
// its own shape, Quick within its bound.
func CleanTurnError(e *TurnError) *TurnError {
	if e == nil || !ValidTurnErrorKind(e.Kind) {
		return nil
	}
	out := *e
	out.At = max(out.At, 0)
	if out.Kind != TurnErrorLimit || out.ResetsAt < 0 {
		out.ResetsAt = 0
	}
	if !promptDigest.MatchString(out.Prompt) {
		out.Prompt = ""
	}
	out.Quick = min(max(out.Quick, 0), MaxTurnErrorQuick)
	return &out
}

// boundTurnError applies the TurnError rules to a normalised list: none on a
// window that is not an IDLE Claude pane (a turn that ended has nothing to say
// about one still running), each cleaned, and at most MaxTurnErrorWindows
// kept — the most recent errors first, then list order.
func boundTurnError(out []Window) {
	var with []int
	for i := range out {
		if out[i].AgentRuntime() != RuntimeClaude || out[i].Status != StatusIdle {
			out[i].TurnError = nil
			continue
		}
		if out[i].TurnError = CleanTurnError(out[i].TurnError); out[i].TurnError != nil {
			with = append(with, i)
		}
	}
	if len(with) <= MaxTurnErrorWindows {
		return
	}
	sort.SliceStable(with, func(a, b int) bool { return out[with[a]].TurnError.At > out[with[b]].TurnError.At })
	for _, i := range with[MaxTurnErrorWindows:] {
		out[i].TurnError = nil
	}
}

func turnErrorEqual(a, b *TurnError) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
