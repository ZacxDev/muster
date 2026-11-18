// Package askattempt is the answer-attempt log: one row (table ask_attempts,
// migration 0113) for every Submit or Dismiss of a question card — a mirrored
// Claude Code AskUserQuestion picker, or an opencode question or permission
// request — from every door that answers one, with how it ended and why
// (docs/mirroring.md#answer-attempts).
//
// 🔴 ONE ENUMERATED SET OF REASONS, ONE PLACE: Reasons. Every refusal before
// anything is typed (a stale sign-in, an unknown card, a closed one, an
// invalid form, a host that cannot type…), every outcome the host reports of
// typing (internal/api's askTypingResult), an opencode relay's, a dismissal's
// and a send-as-message's is one of its keys; the store refuses any other.
// Each reason says how it ends (Outcome) and how a card recovers from it
// (Recovery) — the recoverable-reason table the docs print.
//
// 🔴 NEVER THE QUESTION'S OR THE ANSWER'S TEXT. A row holds the question's
// shape (Shape) only. The one exception is Snapshot, the terminal pane the
// host read at a failure: redacted (redact.Secrets) and capped (MaxSnapshot)
// by the store, the one writer, so no caller can store it raw.
package askattempt

import (
	"sort"
	"time"
)

// The runtimes (ask_attempts.runtime).
const (
	RuntimeClaudeCode = "claude-code"
	RuntimeOpenCode   = "opencode"
)

// The doors a card is answered from (ask_attempts.door).
const (
	// DoorCard: the question card in a session's rendered view, in a browser.
	DoorCard = "card"
	// DoorAttention: the Attention list's own answer form (an opencode ask's
	// option buttons). A Claude question there links to its card.
	DoorAttention = "attention"
	// DoorPhone: the card, in the Android app's shell (the page's
	// window.musterApp bridge says so).
	DoorPhone = "phone"
)

// The actions (ask_attempts.action): a person's Submit or Dismiss.
const (
	ActionSubmit  = "submit"
	ActionDismiss = "dismiss"
)

// How the answer was to reach the session (ask_attempts.method).
const (
	// MethodTyped: keys typed into the Claude Code picker (internal/askdrive).
	MethodTyped = "typed"
	// MethodRelayed: an opencode ask answered through its host's API.
	MethodRelayed = "relayed"
	// MethodEscape: the picker dismissed with one Escape.
	MethodEscape = "escape"
	// MethodMessage: the choice sent to the session as an ordinary message
	// through the send queue ("Send as a message").
	MethodMessage = "message"
)

// The outcomes (ask_attempts.outcome): what a reason means for the attempt.
const (
	// OutcomeOK: it did what it was asked.
	OutcomeOK = "ok"
	// OutcomeRefused: refused before anything reached the host.
	OutcomeRefused = "refused"
	// OutcomeFailed: it reached the host (or was being sent) and did not
	// work.
	OutcomeFailed = "failed"
	// OutcomeNoop: nothing to do (the same answers again, already typed).
	// Not counted in a success rate.
	OutcomeNoop = "noop"
)

// How a card recovers from a reason (Reason.Recovery).
const (
	// RecoverRetry: the card offers Try again. Before its first key the host
	// re-reads the picker and, when it still shows this question, resets it
	// to the first option (askq.Progress.Reset), then types the answer again.
	RecoverRetry = "retry"
	// RecoverMessage: the picker is gone or cannot be read, so typing again
	// cannot work: the card offers "Send as a message" — the picker dismissed
	// if it is still there, then the choice queued as an ordinary message.
	RecoverMessage = "message"
	// RecoverNone: nothing muster can do — answer in the terminal (or there is
	// nothing to recover: it worked).
	RecoverNone = "none"
)

// Reason is one reason code's meaning.
type Reason struct {
	// Outcome is one of the Outcome constants.
	Outcome string
	// Recovery is one of the Recover constants.
	Recovery string
	// Text says what happened, as the Answers page shows it.
	Text string
}

// Reasons is EVERY reason an attempt can end with — THE LEDGER. A new path in
// any door must name one of these (the store refuses anything else, and
// internal/api's ledger test pins the set it can produce to this one).
var Reasons = map[string]Reason{
	// It worked.
	"typed":     {OutcomeOK, RecoverNone, "Typed into the terminal's picker."},
	"relayed":   {OutcomeOK, RecoverNone, "Answered through opencode's API."},
	"dismissed": {OutcomeOK, RecoverNone, "Dismissed with Esc."},
	"queued":    {OutcomeOK, RecoverNone, "Sent to the session as a message (queued)."},
	"duplicate": {OutcomeNoop, RecoverNone, "The same answers again: already typed, or being typed."},

	// Refused before anything reached the host.
	"too-large":      {OutcomeRefused, RecoverNone, "The form was too large to read."},
	"not-found":      {OutcomeRefused, RecoverNone, "Muster had no such card for this window (it may have restarted)."},
	"read-only":      {OutcomeRefused, RecoverNone, "The host is read-only (not started with -interactive)."},
	"step-up":        {OutcomeRefused, RecoverRetry, "The sign-in was stale: a fresh sign-in was asked for."},
	"invalid":        {OutcomeRefused, RecoverNone, "The answers were not ones the question takes."},
	"closed":         {OutcomeRefused, RecoverNone, "The question was no longer open."},
	"in-progress":    {OutcomeRefused, RecoverRetry, "Another answer was being typed into it."},
	"unsupported":    {OutcomeRefused, RecoverNone, "The host's muster-mirror cannot do this."},
	"not-waiting":    {OutcomeRefused, RecoverNone, "opencode was no longer waiting on that ask."},
	"unanswerable":   {OutcomeRefused, RecoverNone, "The card could not be answered here (the host is offline, or cannot relay answers)."},
	"not-offered":    {OutcomeRefused, RecoverNone, "Send as a message was not offered for this card."},
	"queue-refused":  {OutcomeRefused, RecoverNone, "The send queue refused the message (the window cannot be queued for, or the queue is full)."},
	"dismiss-failed": {OutcomeFailed, RecoverNone, "The picker could not be dismissed, so no message was sent."},

	// The host's outcome of typing (internal/api's askTypingResult, the
	// muster_mirror_ask_typing_total labels).
	"not-sent":     {OutcomeFailed, RecoverRetry, "The answer never reached the host."},
	"no-picker":    {OutcomeFailed, RecoverMessage, "The picker was not on the terminal's screen."},
	"unreadable":   {OutcomeFailed, RecoverMessage, "The question could not be read off the terminal's screen."},
	"touched":      {OutcomeFailed, RecoverNone, "Someone was answering it in the terminal."},
	"lost":         {OutcomeFailed, RecoverRetry, "The picker changed part-way through typing."},
	"unrecognised": {OutcomeFailed, RecoverMessage, "The picker's layout was not recognised (Claude Code may have changed)."},
	"busy":         {OutcomeFailed, RecoverRetry, "Another answer was being typed into that terminal."},
	"timeout":      {OutcomeFailed, RecoverRetry, "The host did not report back in time."},
	"error":        {OutcomeFailed, RecoverRetry, "Something else went wrong."},

	// An opencode relay's outcome.
	"gone":           {OutcomeFailed, RecoverNone, "opencode had moved on from that ask."},
	"unreachable":    {OutcomeFailed, RecoverRetry, "The host could not reach opencode."},
	"answer-unknown": {OutcomeFailed, RecoverNone, "opencode did not say whether it took the answer."},
	"answer-refused": {OutcomeFailed, RecoverNone, "opencode refused the answer."},
}

// ReasonCodes is every key of Reasons, sorted.
func ReasonCodes() []string {
	out := make([]string, 0, len(Reasons))
	for k := range Reasons {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Recovery is reason's recovery (RecoverNone for an unknown one).
func Recovery(reason string) string {
	if r, ok := Reasons[reason]; ok {
		return r.Recovery
	}
	return RecoverNone
}

// OutcomeOf is reason's outcome ("" for an unknown one).
func OutcomeOf(reason string) string { return Reasons[reason].Outcome }

// Shape is a question set's shape: never its text.
type Shape struct {
	// Options is each question's option count; Multi whether each is
	// multi-select. Both have one entry per question.
	Options []int
	Multi   []bool
	// Other: an answer of one's own was given to some question.
	Other bool
}

// Questions is how many questions the shape has.
func (s Shape) Questions() int { return len(s.Options) }

// Attempt is one row of ask_attempts.
type Attempt struct {
	ID    int64
	Owner int64
	At    time.Time
	// Runtime, Door, Action and Method are their constants.
	Runtime, Door, Action, Method string
	// Where: the mirrored window.
	Host, Session, Window string
	// CardID is the card's own id: a Claude call's tool_use id, an opencode
	// ask's prompt id.
	CardID string
	Shape  Shape
	// Outcome is Reasons[Reason].Outcome (the store sets it).
	Outcome, Reason string
	// Keys is how many keys of the answer reached the picker; Duration how
	// long the attempt took.
	Keys     int
	Duration time.Duration
	// Number is which attempt on CardID this is, from 1 (the store numbers it).
	Number int
	// Snapshot is the terminal pane at a failure the host reported: redacted
	// and capped by the store.
	Snapshot string
}

// The bounds of a row's columns (each also a CHECK in 0113).
const (
	MaxSnapshot  = 8000
	MaxNameRunes = 200
	MaxWindow    = 32
	MaxQuestions = 64
)

// Retention: a row is deleted RowTTL after it was written (Store.Prune, from
// the attention sweep).
const RowTTL = 30 * 24 * time.Hour

// cut bounds s to n runes.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
