// Package chiefnudge is the chief's stall nudges (docs/chief-nudges.md): the
// rules that decide which of an owner's sessions has stalled, what the chief
// may answer about one, and which of its answers muster accepts — plus the
// decisions table (migration 0064, chief_nudges).
//
// It holds no I/O but the store. The sweep that reads windows, transcripts and
// the chief lives in internal/api (chief_nudge.go); everything here is a pure
// function of what that sweep read, so each guard is testable on its own and
// none of them lives in a prompt.
//
// 🔴 THE GUARDS ARE CODE, NOT INSTRUCTIONS. The chief is told the rules, but a
// nudge is accepted only when CheckQuote finds its quote, byte for byte, inside
// one of the OWNER's own messages to that session (OwnerMessages decides which
// those are); a session's own output can never authorise its own nudge, however
// it is worded.
package chiefnudge

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/ZacxDev/muster/internal/fence"
)

// Session kinds.
const (
	KindMirror = "mirror" // a mirrored Claude Code window; Key is its Claude session id
	KindAgent  = "agent"  // one of the owner's dispatched agents; Key is its id
)

// Decisions the chief may return, plus Invalid for an answer muster could not
// read.
const (
	DecisionNudge   = "nudge"
	DecisionAsk     = "ask"
	DecisionLeave   = "leave"
	DecisionInvalid = "invalid"
)

// Modes.
const (
	ModeShadow = "shadow"
	ModeLive   = "live"
)

// Outcomes: what became of a decision. A suppressed one is
// SuppressedPrefix + the guard's name.
const (
	OutcomeWouldSend   = "would_send"   // shadow: a nudge that passed every guard
	OutcomeWouldAsk    = "would_ask"    // shadow: an ask muster would have raised
	OutcomeLeft        = "left"         // the chief said leave it
	OutcomeQueued      = "queued"       // live: a mirrored nudge waits in the send queue
	OutcomeSent        = "sent"         // live: the nudge reached the session
	OutcomeAsked       = "asked"        // live: an attention ask is open
	OutcomeRelayQueued = "relay_queued" // live: the owner's answer waits in the send queue
	OutcomeRelayed     = "relayed"      // live: the owner's answer reached the session
	OutcomeFailed      = "failed"       // live: the send or relay did not happen (Detail says why)
	OutcomeAskClosed   = "ask_closed"   // live: the ask ended with nothing to relay (Detail says how)
	SuppressedPrefix   = "suppressed:"
)

// Guard names (the part after SuppressedPrefix).
const (
	GuardQuote      = "quote_unverified" // the nudge's quote is not in an owner message
	GuardHourlyCap  = "hourly_cap"       // the global hourly cap is reached
	GuardInvalid    = "invalid_answer"   // the chief's answer did not parse
	GuardNoQuestion = "no_question"      // an ask with no question
	GuardAskFailed  = "ask_not_created"  // the attention ask could not be raised
	GuardMovedOn    = "moved_on"         // the session left its idle streak before muster acted
	GuardAskCap     = "ask_cap"          // the owner's open asks are at attention's cap
	GuardChiefError = "chief_error"      // the chief's call failed (Detail says how); the next streak is judged again
	GuardSuperseded = "superseded"       // a queued nudge or relay was refused at its paste: the session had moved on
	GuardPartial    = "partial_read"     // reason text only: the transcript read was incomplete, so no owner message authorises a nudge
	// GuardLanesFull: the chief's ccd refused every check for this streak with
	// 503 capacity (every lane held by the owner's own sessions) for longer than
	// the sweep's bound; recorded so the streak stops being asked about.
	GuardLanesFull = "lanes_full"
	// GuardChiefNotReady: the same for a typed 503 not_ready (CCD_MAX_SESSIONS=0,
	// or a check CLI that never reaches its prompt).
	GuardChiefNotReady = "chief_not_ready"
)

// Suppressed is the outcome for guard.
func Suppressed(guard string) string { return SuppressedPrefix + guard }

// Defaults and bounds.
const (
	// DefaultIdle is how long a session must have been idle to be a candidate
	// (MUSTER_CHIEF_NUDGE_IDLE).
	DefaultIdle = 10 * time.Minute
	MinIdle     = 2 * time.Minute
	MaxIdle     = 24 * time.Hour
	// DefaultHourlyCap is the global cap on nudges per hour, shadow and live
	// together (MUSTER_CHIEF_NUDGE_HOURLY_CAP).
	DefaultHourlyCap = 6
	// MaxNudgesPerAnchor: after this many nudges since the owner's latest
	// message to a session, the next "nudge" becomes an ask.
	MaxNudgesPerAnchor = 2
	// OwnerWindow is how far back the owner's own messages are read.
	OwnerWindow = 6 * time.Hour
	// MaxOwnerMessages is how many of them (the newest) the chief is shown.
	MaxOwnerMessages = 20
	// MaxOwnerMessageRunes cuts each one; MaxLastRunes the session's last message.
	MaxOwnerMessageRunes = 2000
	MaxLastRunes         = 4000
	// MinQuoteRunes and MinQuoteWords are the shortest quote accepted: a
	// fragment ("make", "ok go") matches almost any message and authorises
	// nothing in particular. A quote must be a phrase of the owner's.
	MinQuoteRunes = 12
	MinQuoteWords = 3
	// MaxQuoteRunes bounds a quote (the nudge carries it).
	MaxQuoteRunes = 500
	// MinOptions/MaxOptions bound an ask's options.
	MinOptions = 2
	MaxOptions = 4
	// RelayMaxAge bounds how long a claimed relay is retried (an agent that
	// stays busy): past it the relay is dropped and recorded failed.
	RelayMaxAge = 2 * time.Hour
	// QueueRepairAfter: a live nudge recorded queued whose send_queue row was
	// never written (a crash between the two) is failed after this.
	QueueRepairAfter = 10 * time.Minute
)

// The column bounds (0064's CHECKs).
const (
	maxTitleRunes    = 300
	maxMessageRunes  = 4000
	maxQuestionRunes = 4000
	maxReasonRunes   = 1000
	maxActionRunes   = 300
	maxOptionRunes   = 200
	maxDetailRunes   = 1000
)

// Session is one of an owner's sessions as the sweep saw it, before any
// transcript is read: everything candidate selection decides on.
type Session struct {
	Kind string
	Key  string // Claude session id (mirror) or agent id (agent)
	// Mirror address (KindMirror).
	Host, TmuxSession, Window, Label string
	// Index is the window's tmux index when the sweep saw it (mirror
	// Window.Index): display only, for the ask's text (mirror.Where) — tmux
	// renumbers windows, so nothing is addressed by it.
	Index int
	// AgentID and ChatSessionID (KindAgent): the agent and the ONE conversation
	// of it that stalled — an agent can hold several.
	AgentID       int64
	ChatSessionID int64
	Title         string // its focus title, "" for none

	// Status is "idle", "busy", "waiting" or "" (unknown).
	Status string
	// IdleFor is how long it has been idle, on muster's clock.
	IdleFor   time.Duration
	IdleSince time.Time
	// StreakKey names this idle streak: a new turn starts a new one.
	StreakKey string

	// Exclusion inputs.
	Shell     bool // a background shell or subagent of its own is running
	TurnError bool // its last turn ended in a usage limit, auth or API error
	Officer   bool
	Chief     bool
	OtherUser bool // not the owner's (never reaches here through row-level security; checked anyway)
	OpenAsk   bool // an open attention ask or chief question of its own, or an open nudge ask
	Decided   bool // this idle streak already has a decision
}

// Exclusion reasons (Exclude's answers).
const (
	ExclNotIdle    = "not_idle"
	ExclTooSoon    = "idle_too_short"
	ExclShell      = "background_work"
	ExclTurnError  = "turn_error"
	ExclOfficer    = "officer"
	ExclChief      = "chief"
	ExclOtherUser  = "other_user"
	ExclOpenAsk    = "open_ask"
	ExclDecided    = "already_decided"
	ExclNoStreak   = "no_streak"
	ExclLastOwner  = "last_message_owner"
	ExclNoMessages = "no_messages"
)

// Exclude is why s is NOT a candidate, "" when it is: idle for at least idle,
// no background work, no turn error, not an officer, the chief or another
// user's, no open ask, and this idle streak not already decided. Coverage
// (officers, the chief, other users) is decided here as well as by what the
// sweep lists, so a new source of sessions cannot widen it by accident.
func Exclude(s Session, idle time.Duration) string {
	switch {
	case s.OtherUser:
		return ExclOtherUser
	case s.Chief:
		return ExclChief
	case s.Officer:
		return ExclOfficer
	case s.Status != "idle":
		return ExclNotIdle
	case s.StreakKey == "":
		return ExclNoStreak
	case s.IdleFor < idle:
		return ExclTooSoon
	case s.Shell:
		return ExclShell
	case s.TurnError:
		return ExclTurnError
	case s.OpenAsk:
		return ExclOpenAsk
	case s.Decided:
		return ExclDecided
	}
	return ""
}

// Msg is one transcript message, oldest first in a slice.
type Msg struct {
	Role string // "user" or "assistant" (anything else is ignored)
	Text string
	// ID names it within its session: a chat message id (agent) or a prompt's
	// byte offset in the transcript (mirror). "" for an assistant message.
	ID string
	// At is when it was written; zero when the source does not say (a
	// mirrored transcript from a muster-mirror older than the PromptAt field).
	At time.Time
}

// AgentKey is a dispatched agent's conversation as a Session.Key: the agent
// and the chat session, so each conversation of an agent stalls (and is
// decided, capped and relayed to) on its own.
func AgentKey(agentID, chatSessionID int64) string {
	return strconv.FormatInt(agentID, 10) + ":" + strconv.FormatInt(chatSessionID, 10)
}

// LastIsAssistant reports whether the newest user-or-assistant message of msgs
// is the assistant's, and returns it.
func LastIsAssistant(msgs []Msg) (Msg, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].Role {
		case "assistant":
			return msgs[i], strings.TrimSpace(msgs[i].Text) != ""
		case "user":
			return Msg{}, false
		}
	}
	return Msg{}, false
}

// fenceOpen matches the opening line internal/fence writes around every piece
// of outside text muster injects into a conversation (a routed message, a
// delivered answer, a forwarded task, a relayed instruction).
var fenceOpen = regexp.MustCompile(`<<<[a-z][a-z0-9-]* untrusted nonce=[0-9a-f]{16} count=[0-9]+>>>`)

// OwnerMessages is the owner's OWN messages among msgs — user-role, written at
// or after since, at most MaxOwnerMessages, newest last — leaving out:
//
//   - a message with NO time: its age cannot be checked against since, so it
//     authorises nothing (an older muster-mirror sends prompts without one;
//     Untimed counts them, so the caller can say why there is no nudge);
//
//   - any message carrying a muster fence (fenceOpen): text muster injected,
//     whose words are an outside party's or muster's, never the owner's;
//
//   - any message equal (after trimming) to one of injected: what muster itself
//     pasted into this session (a routed message, a nudge, a relay — the
//     send_queue rows muster queued and the chief_nudges rows' texts);
//
//   - any message starting with one of prefixes: the openings muster's own
//     pastes are written with (the routed preface, NudgeLabel), passed in from
//     the constants the senders use — a second line behind injected, for a
//     paste whose row is gone;
//
//   - skipFirst: the session's very first message, when the caller knows it is
//     a dispatched agent's kickoff (the task's text, which can be a plugin's).
//
// 🔴 THIS IS THE AUTHORITY SET FOR CheckQuote. A message that should not
// authorise a nudge must not get past here; a message wrongly left out only
// costs a nudge that becomes an ask.
func OwnerMessages(msgs []Msg, injected, prefixes []string, since time.Time, skipFirst bool) []Msg {
	inj := make(map[string]bool, len(injected))
	for _, t := range injected {
		if t = strings.TrimSpace(t); t != "" {
			inj[t] = true
		}
	}
	var out []Msg
	first := true
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		isFirst := first
		first = false
		if isFirst && skipFirst {
			continue
		}
		t := strings.TrimSpace(m.Text)
		if t == "" || m.ID == "" || fenceOpen.MatchString(m.Text) || inj[t] || hasAnyPrefix(t, prefixes) {
			continue
		}
		if m.At.IsZero() || (!since.IsZero() && m.At.Before(since)) {
			continue
		}
		out = append(out, m)
	}
	if len(out) > MaxOwnerMessages {
		out = out[len(out)-MaxOwnerMessages:]
	}
	return out
}

// Untimed counts msgs' user-role messages that carry no time.
func Untimed(msgs []Msg) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" && m.At.IsZero() && strings.TrimSpace(m.Text) != "" {
			n++
		}
	}
	return n
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Anchor is the owner's latest message's id among owner ("" for none): the
// nudge cap counts nudges per anchor, so a fresh instruction from the owner
// resets it.
func Anchor(owner []Msg) string {
	if len(owner) == 0 {
		return ""
	}
	return owner[len(owner)-1].ID
}

// Proposal is the chief's answer, as parsed.
type Proposal struct {
	Decision      string   `json:"decision"`
	Reason        string   `json:"reason"`
	QuoteID       string   `json:"quote_id"`
	Quote         string   `json:"quote"`
	CoveredAction string   `json:"covered_action"`
	Question      string   `json:"question"`
	Options       []string `json:"options"`
}

// ErrUnparseable: the chief's answer holds no JSON object with a known
// decision.
var ErrUnparseable = errors.New("chiefnudge: the chief's answer is not a decision")

// ParseProposal reads the first JSON object in raw (a model may wrap it in
// prose or a code fence) and checks its decision is one of nudge, ask, leave.
func ParseProposal(raw string) (Proposal, error) {
	i := strings.IndexByte(raw, '{')
	if i < 0 {
		return Proposal{}, ErrUnparseable
	}
	dec := json.NewDecoder(strings.NewReader(raw[i:]))
	var p Proposal
	if err := dec.Decode(&p); err != nil {
		return Proposal{}, fmt.Errorf("%w: %v", ErrUnparseable, err)
	}
	p.Decision = strings.ToLower(strings.TrimSpace(p.Decision))
	switch p.Decision {
	case DecisionNudge, DecisionAsk, DecisionLeave:
	default:
		return Proposal{}, fmt.Errorf("%w: decision %q", ErrUnparseable, p.Decision)
	}
	p.Reason = cut(oneLine(p.Reason), maxReasonRunes)
	p.CoveredAction = cut(oneLine(p.CoveredAction), maxActionRunes)
	p.Question = cut(strings.TrimSpace(p.Question), maxQuestionRunes)
	p.QuoteID = strings.TrimSpace(p.QuoteID)
	return p, nil
}

// Quote guard errors.
var (
	ErrNoQuote      = errors.New("the nudge quotes nothing")
	ErrQuoteShort   = fmt.Errorf("the quote is not a phrase: it needs at least %d words and %d characters", MinQuoteWords, MinQuoteRunes)
	ErrQuoteLong    = fmt.Errorf("the quote is longer than %d characters", MaxQuoteRunes)
	ErrNoCover      = errors.New("the nudge names no pending action its quote covers")
	ErrQuoteUnknown = errors.New("the quote names no owner message of this session")
	ErrQuoteNotIn   = errors.New("the quote is not, byte for byte and on word boundaries, in the owner message it names")
)

// Shown is text as the chief is shown it in the stall check's fenced block:
// each line through fence.Sanitize. A quote is matched against THIS form, so
// a tab or a control character the chief saw replaced does not make a true
// quote fail.
func Shown(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = fence.Sanitize(l)
	}
	return strings.Join(lines, "\n")
}

// CheckQuote is the nudge guard: p's quote must be a phrase (MinQuoteWords
// words, MinQuoteRunes characters), name a covered action, and appear BYTE FOR
// BYTE — starting and ending on word boundaries — inside the one owner message
// p.QuoteID names (as Shown), which must be one of owner (OwnerMessages' set:
// never the session's own output, never injected text, never older than the
// window). It returns that message.
//
// ⚠ WHAT IT GUARANTEES, AND WHAT IT DOES NOT: the quote is the owner's own
// recent words, verbatim. Whether those words COVER the pending action is the
// chief's judgement (covered_action is its claim), not something code checks.
func CheckQuote(p Proposal, owner []Msg) (Msg, error) {
	q := p.Quote
	trimmed := strings.TrimSpace(q)
	switch n := utf8.RuneCountInString(trimmed); {
	case n == 0:
		return Msg{}, ErrNoQuote
	case n < MinQuoteRunes || len(strings.Fields(trimmed)) < MinQuoteWords:
		return Msg{}, ErrQuoteShort
	case utf8.RuneCountInString(q) > MaxQuoteRunes:
		return Msg{}, ErrQuoteLong
	}
	if strings.TrimSpace(p.CoveredAction) == "" {
		return Msg{}, ErrNoCover
	}
	for _, m := range owner {
		if m.ID != p.QuoteID {
			continue
		}
		if containsPhrase(Shown(m.Text), q) {
			return m, nil
		}
		return Msg{}, ErrQuoteNotIn
	}
	return Msg{}, ErrQuoteUnknown
}

// containsPhrase reports whether q occurs in text with a word boundary on
// each side: no letter or digit right before it or right after it (an edge of
// q that is itself not a letter or digit needs none).
//
// Both sides are NFC-normalised first, so a composed and a decomposed spelling
// of the same word match — and a quote that ends where a decomposed text has a
// combining mark is not a whole word. Word characters are letters, combining
// marks (a Devanagari vowel sign or virama is part of its word), digits and
// apostrophes ("t go ahead" is not a phrase of "don't go ahead").
func containsPhrase(text, q string) bool {
	text, q = norm.NFC.String(text), norm.NFC.String(q)
	if q == "" {
		return false
	}
	word := isWordRune
	first, _ := utf8.DecodeRuneInString(q)
	last, _ := utf8.DecodeLastRuneInString(q)
	for from := 0; from <= len(text)-len(q); {
		i := strings.Index(text[from:], q)
		if i < 0 {
			return false
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[i+len(q):])
		okBefore := i == 0 || !word(first) || !word(before)
		okAfter := i+len(q) == len(text) || !word(last) || !word(after)
		if okBefore && okAfter {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		from = i + size
	}
	return false
}

// isWordRune is containsPhrase's word character.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r) || r == '\'' || r == '\u2019'
}

// EscalationPartial is the ask raised in place of a nudge when the
// transcript read was incomplete: no instruction of the owner's can be checked.
func EscalationPartial(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = "This session"
	}
	return t + " stopped. muster could not read enough of its conversation to check an instruction of yours " +
		"that covers the next step. Should it continue?"
}

// EscalationNoTimes is the ask raised in place of a nudge for a session whose
// host sends no prompt times: no instruction of the owner's can be checked.
func EscalationNoTimes(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = "This session"
	}
	return t + " stopped. Its host does not send when your messages were written, so muster cannot check an " +
		"instruction of yours that covers the next step. Should it continue?"
}

// CleanOptions is an ask's options: trimmed, one line, deduplicated, cut, at
// most MaxOptions; when fewer than MinOptions remain, the defaults fill in.
func CleanOptions(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, o := range in {
		o = cut(oneLine(o), maxOptionRunes)
		k := strings.ToLower(o)
		if o == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, o)
		if len(out) == MaxOptions {
			break
		}
	}
	if len(out) < MinOptions {
		for _, d := range []string{"Continue", "Leave it for now"} {
			if !seen[strings.ToLower(d)] && len(out) < MinOptions {
				out = append(out, d)
				seen[strings.ToLower(d)] = true
			}
		}
	}
	return out
}

// NudgeLabel opens every nudge: the session reads a labelled message from the
// chief quoting its owner, never a message in the owner's voice.
const NudgeLabel = "[Chief nudge]"

// NudgeText is the nudge pasted into the session (operator decision: a
// labelled message quoting the owner). ownerName is who said it ("your owner"
// when unknown); m.At, when set, is when, in loc.
func NudgeText(ownerName string, m Msg, quote string, loc *time.Location) string {
	name := strings.TrimSpace(oneLine(ownerName))
	if name == "" {
		name = "Your owner"
	}
	when := " earlier in this session"
	if !m.At.IsZero() {
		if loc == nil {
			loc = time.UTC
		}
		when = " at " + m.At.In(loc).Format("15:04")
	}
	return NudgeLabel + " " + name + " said" + when + ": \"" + oneLine(quote) + "\". Continue."
}

// EscalationQuestion is the ask raised in place of a third nudge.
func EscalationQuestion(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = "This session"
	}
	return t + " stopped again after " + strconv.Itoa(MaxNudgesPerAnchor) +
		" nudges since your last message to it. Should it continue?"
}

// Decision is one row of chief_nudges.
type Decision struct {
	ID          int64
	Owner       int64
	Kind        string
	Key         string
	Host        string
	TmuxSession string
	Window      string
	AgentID     int64
	// ChatSessionID is the agent's conversation the decision is about (0 for a
	// mirrored session): where its nudge or relay goes.
	ChatSessionID int64
	Title         string
	StreakKey     string
	AnchorKey     string
	IdleSince     time.Time
	DecidedAt     time.Time
	Decision      string
	Message       string
	QuoteID       string
	QuoteAt       time.Time
	QuoteText     string
	Covered       string
	Question      string
	Options       []string
	Reason        string
	Mode          string
	Outcome       string
	Detail        string
	AskID         int64
	SendQueueID   int64
	RelayText     string
	UpdatedAt     time.Time
	// The stall classifier's result on the session's last message (jev.go):
	// zero values with JevError "" mean it was not run.
	JevLabel          string
	JevNeedsInput     float64
	JevAsksPermission float64
	JevNeither        float64
	JevAskScore       float64
	JevModel          string
	JevLatencyMS      int
	JevError          string
}

// WithJev is d carrying classification r.
func (d Decision) WithJev(r Result) Decision {
	d.JevLabel, d.JevModel, d.JevLatencyMS, d.JevError = r.Label, r.Model, r.LatencyMS, r.Err
	d.JevNeedsInput, d.JevAsksPermission, d.JevNeither = r.Scores[LabelNeedsInput], r.Scores[LabelAsksPermission], r.Scores[LabelNeither]
	d.JevAskScore = r.AskScore
	return d
}

// Clean bounds d's free-text fields to the table's CHECKs.
func Clean(d Decision) Decision {
	d.Title = cut(oneLine(d.Title), maxTitleRunes)
	d.Message = cut(d.Message, maxMessageRunes)
	d.QuoteText = cut(d.QuoteText, MaxQuoteRunes)
	d.Covered = cut(oneLine(d.Covered), maxActionRunes)
	d.Question = cut(d.Question, maxQuestionRunes)
	d.Reason = cut(oneLine(d.Reason), maxReasonRunes)
	d.Detail = cut(oneLine(d.Detail), maxDetailRunes)
	d.RelayText = cut(d.RelayText, 8000)
	d.JevModel = cut(oneLine(d.JevModel), 200)
	d.JevError = cut(oneLine(d.JevError), 100)
	if d.Options == nil {
		d.Options = []string{}
	}
	return d
}

// Suppressed reports whether d's outcome is a guard's.
func (d Decision) Suppressed() (string, bool) {
	g, ok := strings.CutPrefix(d.Outcome, SuppressedPrefix)
	return g, ok
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// Cut is the first n runes of s, with "…" when anything was dropped.
func Cut(s string, n int) string { return cut(s, n) }
