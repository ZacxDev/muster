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
	"unicode/utf8"
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
	// MinQuoteRunes is the shortest quote accepted: a two-letter "ok" matches
	// almost any message and authorises nothing in particular.
	MinQuoteRunes = 4
	// MaxQuoteRunes bounds a quote (the nudge carries it).
	MaxQuoteRunes = 500
	// MinOptions/MaxOptions bound an ask's options.
	MinOptions = 2
	MaxOptions = 4
	// StallerThreshold is "a repeat staller": this many decisions for one
	// session within StallerWindow.
	StallerThreshold = 3
	StallerWindow    = 24 * time.Hour
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
	// AgentID (KindAgent).
	AgentID int64
	Title   string // its focus title, "" for none

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
	// At is when it was written; zero when the source does not say (a mirrored
	// transcript carries no times on the wire).
	At time.Time
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
// or after since (a message with no time is kept: a mirrored transcript has
// none, and its read is bounded by the host's backfill instead), at most
// MaxOwnerMessages, newest last — leaving out:
//
//   - any message carrying a muster fence (fenceOpen): text muster injected,
//     whose words are an outside party's or muster's, never the owner's;
//   - any message equal (after trimming) to one of injected: what muster itself
//     pasted into this session (a routed message, a nudge, a relay — the
//     send_queue rows muster queued and the chief_nudges rows' texts);
//   - any message starting with one of prefixes: the openings muster's own
//     pastes are written with (the routed preface, NudgeLabel), passed in from
//     the constants the senders use — a second line behind injected, for a
//     paste whose row is gone;
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
		if !m.At.IsZero() && !since.IsZero() && m.At.Before(since) {
			continue
		}
		out = append(out, m)
	}
	if len(out) > MaxOwnerMessages {
		out = out[len(out)-MaxOwnerMessages:]
	}
	return out
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
	ErrQuoteShort   = fmt.Errorf("the quote is shorter than %d characters", MinQuoteRunes)
	ErrQuoteLong    = fmt.Errorf("the quote is longer than %d characters", MaxQuoteRunes)
	ErrNoCover      = errors.New("the nudge names no pending action its quote covers")
	ErrQuoteUnknown = errors.New("the quote names no owner message of this session")
	ErrQuoteNotIn   = errors.New("the quote is not, byte for byte, in the owner message it names")
)

// CheckQuote is the nudge guard: p's quote must be non-trivial, name a covered
// action, and appear BYTE FOR BYTE inside the owner message p.QuoteID names,
// which must be one of owner (OwnerMessages' set — never the session's own
// output, never injected text). It returns that message.
func CheckQuote(p Proposal, owner []Msg) (Msg, error) {
	q := p.Quote
	switch n := utf8.RuneCountInString(strings.TrimSpace(q)); {
	case n == 0:
		return Msg{}, ErrNoQuote
	case n < MinQuoteRunes:
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
		if strings.Contains(m.Text, q) {
			return m, nil
		}
		return Msg{}, ErrQuoteNotIn
	}
	return Msg{}, ErrQuoteUnknown
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
	Title       string
	StreakKey   string
	AnchorKey   string
	IdleSince   time.Time
	DecidedAt   time.Time
	Decision    string
	Message     string
	QuoteID     string
	QuoteAt     time.Time
	QuoteText   string
	Covered     string
	Question    string
	Options     []string
	Reason      string
	Mode        string
	Outcome     string
	Detail      string
	AskID       int64
	SendQueueID int64
	RelayText   string
	UpdatedAt   time.Time
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

// Staller is a session with StallerThreshold or more decisions in the window.
type Staller struct {
	Kind, Key, Title, Host string
	Count                  int
	Last                   time.Time
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
