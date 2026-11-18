// Package asklog is the ask log: one row (table asks, migration 0089) for
// every question an agent put to its owner, whatever kind it was, with the
// answer the owner gave, who gave it and how long it took. It is phase 1 of
// the design "ask telemetry, review, and progressive auto-approval" (see
// docs/asks.md): telemetry only. Nothing here answers an ask, proposes an
// answer or evaluates a rule.
//
// The kinds:
//
//   - KindPlainText: a question in the last assistant message of a turn — a
//     mirrored Claude Code window going from busy to idle, or a dispatched
//     agent's turn ending — kept only when it passes the pre-filter
//     (Prefilter, in code, no model). Its answer is the first user message
//     after it, with what muster can prove of who wrote it (Provenance).
//   - KindAskUserQuestion: a Claude Code AskUserQuestion picker, from the
//     moment the question store accepts it to how it ended.
//   - KindAttention: an attention_ask (an ask, never a notice).
//   - KindOpenCodeQuestion, KindOpenCodePermission: an opencode form or
//     permission request a mirrored opencode window waits on.
//
// 🔴 TEXT IS REDACTED ON THE WAY IN, BY THE STORE. Every text column passes
// through redact.Secrets inside PG.Open and PG.Resolve — the one redactor, and
// the one writer — so no caller can store a raw secret by forgetting to.
//
// 🔴 CAPTURE NEVER BLOCKS OR FAILS THE FLOW IT WATCHES. Callers hand their
// writes to a Recorder, which runs them off the caller's goroutine, in order,
// and turns every failure (and a panic) into a log line and a count
// (muster_ask_capture_errors_total) — never an error returned to the agent or
// the owner.
package asklog

import (
	"encoding/json"
	"time"
)

// The kinds of ask (asks.kind, asks_kind_check).
const (
	KindPlainText          = "plain_text"
	KindAskUserQuestion    = "ask_user_question"
	KindAttention          = "attention_ask"
	KindOpenCodeQuestion   = "opencode_question"
	KindOpenCodePermission = "opencode_permission"
)

// Kinds is every kind, in the order the design lists them.
var Kinds = []string{KindPlainText, KindAskUserQuestion, KindAttention, KindOpenCodeQuestion, KindOpenCodePermission}

// OpenCodeKinds is the two opencode kinds: an opencode ask is resolved by its
// prompt id whichever it is.
var OpenCodeKinds = []string{KindOpenCodeQuestion, KindOpenCodePermission}

// The runtimes (asks.runtime).
const (
	RuntimeClaudeCode = "claude-code"
	RuntimeOpenCode   = "opencode"
	RuntimeAgent      = "agent"
)

// How an ask ended (asks.outcome).
const (
	OutcomeAnswered          = "answered"
	OutcomeAnsweredInSession = "answered_in_session"
	OutcomeDeclined          = "declined"
	OutcomeExpired           = "expired"
	OutcomeCancelled         = "cancelled"
	OutcomeSuperseded        = "superseded"
	// OutcomeMovedOn: the session went on without a recorded answer (a new
	// assistant turn with no user message in between; an opencode ask that
	// left the host's list unanswered from here).
	OutcomeMovedOn = "moved_on"
	// OutcomeGone: the window was gone before any reply (or now shows another
	// session).
	OutcomeGone = "gone"
	// OutcomeUnknown: muster restarted before it saw how the ask ended
	// (PG.CloseStale). An answer seen later replaces it.
	OutcomeUnknown = "unknown"
)

// Who answered (asks.answered_by). The later phases add "auto:<rule id>" and
// "shadow".
const (
	ByOwner = "owner"
	// ByChief: the chief — its answer to a question put to it, or a nudge or
	// relayed answer it queued into a session (provenance nudge). 🔴 Never
	// counted as the owner's approval.
	ByChief = "chief"
	// ByOther: neither the owner nor the chief — a routed message (a
	// plugin's, another session's).
	ByOther = "other"
)

// AnsweredByFor is who answered a plain-text question whose reply had
// provenance prov: a nudge is the chief's, a routed message another's, and a
// message muster pasted for the owner or one it cannot attribute (pane) the
// owner's.
func AnsweredByFor(prov string) string {
	switch prov {
	case ProvNudge:
		return ByChief
	case ProvRoute:
		return ByOther
	}
	return ByOwner
}

// Where a plain-text reply came from (asks.answer_provenance): what muster
// can prove, never a guess.
const (
	// ProvMusterSend: a body muster pasted from the owner's own send queue
	// (or, for a dispatched agent, a line the owner typed into muster's chat).
	ProvMusterSend = "muster_send"
	// ProvPane: a user message muster did not put there and cannot attribute
	// (the owner typing in the terminal — or anything else that can type).
	ProvPane = "pane"
	// ProvNudge: a chief nudge or relayed answer muster queued.
	ProvNudge = "nudge"
	// ProvRoute: a routed message (a plugin's, another session's).
	ProvRoute = "route"
)

// The columns' bounds (each also a CHECK in 0089).
const (
	MaxQuestionRunes       = 4000
	MaxQuestionNormRunes   = 500
	MaxAnswerRunes         = 8000
	MaxProposedAnswerRunes = 500
	MaxCwdRunes            = 300
	MaxRefRunes            = 200
	MaxNameRunes           = 200
	MaxWindowRunes         = 32
)

// Retention: question and answer text is cleared after DefaultTextTTL
// (MUSTER_ASK_LOG_TEXT_TTL), the row deleted after RowTTL.
const (
	DefaultTextTTL = 30 * 24 * time.Hour
	RowTTL         = 180 * 24 * time.Hour
)

// Jev is the stall classifier's answer (chiefnudge.Result) as a row keeps it,
// as on session_episodes. The zero value: it did not run.
type Jev struct {
	Label                               string
	NeedsInput, AsksPermission, Neither float64
	AskScore                            float64
	Model                               string
	Err                                 string
}

// Ask is one row of asks.
type Ask struct {
	ID    int64
	Owner int64
	Kind  string
	// Runtime is RuntimeClaudeCode, RuntimeOpenCode or RuntimeAgent.
	Runtime string
	// Where it was asked: a mirrored window (Host, TmuxSession, Window), its
	// Claude Code or opencode session (ClaudeSession), or a dispatched agent's
	// conversation (AgentID, ChatSessionID).
	Host, TmuxSession, Window string
	ClaudeSession             string
	AgentID, ChatSessionID    int64
	// Repo is owner/name when known, else ""; Cwd the working directory the
	// host reported.
	Repo, Cwd string
	// EpisodeID is the session_episodes row the ask was raised in (0: none).
	EpisodeID int64
	// SourceRef is the source's own id ("" for plain text).
	SourceRef string
	// Question is the redacted question; QuestionNorm its normalised form
	// (Normalise). Both "" once purged.
	Question, QuestionNorm string
	// Options is the offered options as JSON (an array; nil stores []).
	Options json.RawMessage
	Jev     Jev
	// Answer is what it was answered with ("" once purged); AnsweredBy,
	// Provenance and Outcome as their constants.
	Answer, AnsweredBy, Provenance, Outcome string
	AskedAt                                 time.Time
	AnsweredAt                              time.Time // zero: not answered
	TextPurgedAt                            time.Time // zero: text held
}

// Latency is how long the ask waited for its answer; 0 when unanswered.
func (a Ask) Latency() time.Duration {
	if a.AnsweredAt.IsZero() || a.AskedAt.IsZero() {
		return 0
	}
	return a.AnsweredAt.Sub(a.AskedAt)
}

// Resolution is how an ask ended.
type Resolution struct {
	// Outcome is one of the Outcome constants.
	Outcome string
	// Answer is the answer's text (redacted by the store); AnsweredBy ByOwner,
	// ByChief or ""; Provenance one of the Prov constants or "".
	Answer, AnsweredBy, Provenance string
	// At is when it ended (the reply's own time when the source says).
	At time.Time
}

// Answered reports whether outcome is one the owner (or the chief) gave: the
// row's answered_at is set only for these.
func Answered(outcome string) bool {
	return outcome == OutcomeAnswered || outcome == OutcomeAnsweredInSession || outcome == OutcomeDeclined
}

// soft reports whether outcome may still be replaced by an answer: an ask that
// looked expired, superseded, moved on or gone can have its answer recorded
// later (a question store entry resolved after it expired; the host's next list
// arriving before the answer route finished).
func soft(outcome string) bool {
	return outcome == OutcomeMovedOn || outcome == OutcomeExpired || outcome == OutcomeGone || outcome == OutcomeSuperseded ||
		outcome == OutcomeUnknown
}

// StaleKinds are the kinds whose resolution lives only in the process that
// opened them (a plain-text question's reply link; an opencode ask's leaving
// the window): after a restart nothing can resolve them, so CloseStale closes
// them unknown. An AskUserQuestion and an attention ask resolve by their source
// id from any process, and are left open.
var StaleKinds = []string{KindPlainText, KindOpenCodeQuestion, KindOpenCodePermission}

// validOutcome reports whether o is an outcome.
func validOutcome(o string) bool {
	switch o {
	case OutcomeAnswered, OutcomeAnsweredInSession, OutcomeDeclined, OutcomeExpired, OutcomeCancelled, OutcomeSuperseded,
		OutcomeMovedOn, OutcomeGone, OutcomeUnknown:
		return true
	}
	return false
}

// validKind reports whether k is a kind.
func validKind(k string) bool {
	for _, x := range Kinds {
		if k == x {
			return true
		}
	}
	return false
}

// cut bounds s to n runes.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
