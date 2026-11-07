package focus

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// A SESSION'S STABLE NAME (migration 0088; docs/focus.md#a-sessions-name).
//
// A session is headed by a name that does not move, with its live focus as the
// line under it. This file owns the two STORED halves of that name — the AUTO
// name frozen from the session's first good focus title, and a MIRRORED
// session's MANUAL name — and the quality bar that decides "good". Which name a
// surface shows is internal/sessionname.Resolve's one rule, not this package's.
//
// 🔴 THE BAR IS CODE, NOT A PROMPT. The summarizer is asked for a short, plain
// title; Accept already enforces the shape. Whether a title is fit to NAME the
// session for good is decided here, deterministically, from the title and the
// tail it was computed from — never by asking the model again.

// Name is a session's stored name halves: Auto (the frozen auto name, "" until
// a nameable focus is written) and Manual (a mirrored session's name the person
// gave it, "" for none — an agent's lives in agents.display_name).
type Name struct {
	Auto     string
	AutoAt   time.Time
	Manual   string
	ManualAt time.Time
	// DriftDismissed is the focus title the owner dismissed a rename offer
	// at, "" for none (drift.go, migration 0089).
	DriftDismissed string
}

// The name bar's bounds.
const (
	// NameMinWords is the fewest words a nameable title has: one word
	// ("Debugging", "Refactor") names an activity, not a piece of work.
	NameMinWords = 2
	// MaxNameRunes bounds a manual name: a title's display cap
	// (TitleCapRunes), so a name the person typed draws like an auto one.
	MaxNameRunes = TitleCapRunes
	// NameRetention is how much longer than the focus retention a mirrored
	// session's row is kept when it carries a name, auto or manual
	// (PGStore.PruneAll).
	NameRetention = 90 * 24 * time.Hour
)

// openerWords are the words a session's OPENING says nothing with: a greeting,
// "continue", an acknowledgement, and the words a summarizer wraps such an
// opening in ("Greeting the user", "Awaiting instructions", "New session").
// A title — or a person's turn — made of nothing else names no work.
//
// ⚠ A CLOSED VOCABULARY, ON PURPOSE. It is matched word for word after
// lower-casing (words), with the fillerWords allowed around it; it does not
// guess. A title with ONE word outside both sets ("Greeting the billing team")
// is a name. Growing it is a code change with a test (names_test.go).
var openerWords = setOf(
	// greetings and acknowledgements
	"hi", "hello", "hey", "heya", "hiya", "yo", "howdy", "greeting", "greetings", "greet", "greeted", "greets",
	"welcome", "welcoming", "thanks", "thank", "ok", "okay", "yes", "yep", "sure", "cool",
	"morning", "afternoon", "evening",
	// "carry on"
	"continue", "continued", "continuing", "continuation", "resume", "resumed", "resuming", "go", "proceed",
	"proceeding", "on", "again",
	// the summarizer's words for an opening that named no work
	"session", "sessions", "conversation", "chat", "new", "start", "started", "starting", "begin", "beginning",
	"waiting", "awaiting", "await", "wait", "instructions", "instruction", "input", "request", "requests",
	"task", "tasks", "idle", "ready", "standby", "nothing", "help", "assist", "assisting", "assistance",
	"initial", "user", "users", "unknown", "untitled", "introduction", "intro", "previous", "prior",
)

// fillerWords may appear around opener words without making a title a name
// ("Greeting THE user", "Waiting FOR instructions").
var fillerWords = setOf("the", "a", "an", "to", "for", "of", "with", "and", "or", "from", "by", "at", "this", "that",
	"my", "your", "their", "our", "its", "it", "some", "any", "no", "is", "are", "be", "s")

func setOf(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// isOpener reports whether text is an OPENER: no word of it outside the opener
// and filler vocabularies (an empty text is one — it names nothing).
func isOpener(text string) bool {
	for _, w := range words(text) {
		if !openerWords[w] && !fillerWords[w] {
			return false
		}
	}
	return true
}

// NameableTitle reports whether title passes the name bar's TITLE half: a
// title Accept could have stored and the summarizer may keep (FeedableTitle:
// within TitleCapRunes, plain — no markdown, no list marker, no backtick); at
// least NameMinWords words; and not an opener ("Greeting the user",
// "Continue session", "Hi").
func NameableTitle(title string) bool {
	return FeedableTitle(title) && len(words(title)) >= NameMinWords && !isOpener(title)
}

// RealTurn reports whether entries hold the session's first REAL turn: a
// person's prompt that is not an opener ("hi", "continue", "ok go on"),
// followed by a reply. A tail of only greetings — or a prompt nobody has
// answered yet — has not shown what the session is for.
func RealTurn(entries []Entry) bool {
	asked := false
	for _, e := range entries {
		switch e.Role {
		case "user":
			if !isOpener(e.Text) {
				asked = true
			}
		case "assistant":
			if asked && strings.TrimSpace(e.Text) != "" {
				return true
			}
		}
	}
	return false
}

// Nameable is THE NAME BAR: whether title, the focus just accepted for a
// session whose conversation is entries, may freeze the session's auto name.
// Both halves must hold: the conversation has had a real turn (RealTurn), and
// the title is fit to name it (NameableTitle). Deterministic: the same title
// and tail always give the same answer, and no model is asked.
func Nameable(title string, entries []Entry) bool {
	return RealTurn(entries) && NameableTitle(title)
}

// ErrBadName refuses a manual name that is empty after cleaning or longer than
// MaxNameRunes.
var ErrBadName = errors.New("focus: a name is 1 to 60 characters of plain text")

// CleanName is a typed name ready to store: one line, control and format
// characters dropped, whitespace folded (as a title is, oneLine) — or
// ErrBadName. An empty result is ErrBadName too; clearing a name is
// SetManualName with "".
func CleanName(name string) (string, error) {
	n := oneLine(name)
	if n == "" || utf8.RuneCountInString(n) > MaxNameRunes {
		return "", ErrBadName
	}
	return n, nil
}

// ErrAgentManualName refuses a manual name on an agent subject: an agent's
// name the person gave it is agents.display_name (agents.Store.SetDisplayName).
var ErrAgentManualName = errors.New("focus: an agent's name is its display name, not a focus row's")

// SetManualName stores name as the caller's manual name for the MIRRORED
// session sub ("" clears it), creating the session's row when it has none
// (a title-less row, which every focus reader skips — as RecordError's is). It
// never touches the focus, the auto name or the history.
func (s *PGStore) SetManualName(ctx context.Context, sub Subject, name string) (Name, error) {
	if err := sub.Valid(); err != nil {
		return Name{}, err
	}
	if sub.Kind != KindMirror {
		return Name{}, ErrAgentManualName
	}
	if name != "" {
		var err error
		if name, err = CleanName(name); err != nil {
			return Name{}, err
		}
	}
	f, err := scan(s.pool.QueryRow(ctx, `INSERT INTO agent_focus (subject_kind, subject_key, agent_id, name_manual, name_manual_at)
		VALUES ($1, $2, NULL, $3, CASE WHEN $3 = '' THEN NULL ELSE now() END)
		ON CONFLICT (owner_id, subject_kind, subject_key) DO UPDATE SET name_manual = EXCLUDED.name_manual,
			name_manual_at = EXCLUDED.name_manual_at, seen_at = now()
		RETURNING `+cols, string(sub.Kind), sub.Key, name))
	if err != nil {
		return Name{}, errf("set name: %w", err)
	}
	return f.Name, nil
}
