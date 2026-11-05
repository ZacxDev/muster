package chiefnudge

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Step 3b-1 (docs/design/session-state.md, "Covering words"): a nudge's quote
// may come from the owner's typed Chief chat. These pin the pure half: the
// source the chief names is the ONLY one a quote is looked for in, the
// Chief-chat turns reach the prompt only with session state on, and a
// Chief-chat nudge says where the words were written.

var (
	sessionMsgs = []Msg{{Role: "user", ID: "line:10", Text: "keep going until merged, then report back", At: time.Unix(1_760_000_000, 0)}}
	chatMsgs    = []Msg{{Role: "user", ID: "chief:77", Text: "ship the release branch once CI is green", At: time.Unix(1_760_000_100, 0)}}
)

func chatProp(source, id, quote string) Proposal {
	return Proposal{Decision: DecisionNudge, QuoteSource: source, QuoteID: id, Quote: quote, CoveredAction: "merge the pull request"}
}

// TestCheckQuoteFromLooksOnlyInTheNamedSource: a Chief-chat quote passes only
// in the Chief chat; the same words named as a session quote do not; a session
// quote passes only in the session (and naming no source is a session quote);
// a source muster does not know is refused; and with the Chief chat off (nil)
// no Chief-chat quote passes, while an EMPTY Chief chat is merely "no such
// message".
func TestCheckQuoteFromLooksOnlyInTheNamedSource(t *testing.T) {
	for _, c := range []struct {
		name    string
		p       Proposal
		chat    []Msg
		wantSrc string
		wantErr error
	}{
		{"chief chat, found", chatProp(SourceChiefChat, "chief:77", "ship the release branch"), chatMsgs, SourceChiefChat, nil},
		{"chief chat words named as session", chatProp(SourceSession, "chief:77", "ship the release branch"), chatMsgs, SourceSession, ErrQuoteUnknown},
		{"session words named as chief chat", chatProp(SourceChiefChat, "line:10", "keep going until merged"), chatMsgs, SourceChiefChat, ErrQuoteUnknown},
		{"session, found", chatProp(SourceSession, "line:10", "keep going until merged"), chatMsgs, SourceSession, nil},
		{"no source is a session quote", chatProp("", "line:10", "keep going until merged"), chatMsgs, SourceSession, nil},
		{"an unknown source", chatProp("pane", "chief:77", "ship the release branch"), chatMsgs, "", ErrQuoteSource},
		{"the chief chat off", chatProp(SourceChiefChat, "chief:77", "ship the release branch"), nil, SourceChiefChat, ErrChiefChatOff},
		{"the chief chat empty", chatProp(SourceChiefChat, "chief:77", "ship the release branch"), []Msg{}, SourceChiefChat, ErrQuoteUnknown},
		{"chief chat, not verbatim", chatProp(SourceChiefChat, "chief:77", "ship the release branch now"), chatMsgs, SourceChiefChat, ErrQuoteNotIn},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, src, err := CheckQuoteFrom(c.p, sessionMsgs, c.chat)
			if src != c.wantSrc || !errors.Is(err, c.wantErr) || (err == nil && m.ID != c.p.QuoteID) {
				t.Fatalf("got %+v %q %v; want source %q, error %v", m, src, err, c.wantSrc, c.wantErr)
			}
		})
	}
}

// TestParseReadsTheQuoteSource: quote_source is read, trimmed and lowered.
func TestParseReadsTheQuoteSource(t *testing.T) {
	p, err := ParseNotifyProposal(`{"decision":"nudge","quote_source":" Chief_Chat ","quote_id":"chief:1","quote":"x"}`)
	if err != nil || p.QuoteSource != SourceChiefChat {
		t.Fatalf("%+v %v", p, err)
	}
}

// TestTheChiefChatIsInThePromptOnlyWithSessionStateOn: with Notify the prompt
// asks for quote_source and shows the typed Chief-chat turns with their
// "chief:<id>"; without it the prompt is byte-identical whatever ChiefChat
// holds (session state off is unchanged).
func TestTheChiefChatIsInThePromptOnlyWithSessionStateOn(t *testing.T) {
	const nonce = "0123456789abcdef"
	s, last := Session{Title: "Fix CI"}, Msg{Role: "assistant", Text: "done?"}
	on := Prompt(s, last, sessionMsgs, 15*time.Minute, Hint{Notify: true, ChiefChat: chatMsgs}, nonce)
	for _, want := range []string{`"quote_source":"session"|"chief_chat"`, "[id chief:77 ·", "ship the release branch once CI is green",
		"Chief chat (quote_source chief_chat)"} {
		if !strings.Contains(on, want) {
			t.Errorf("the state-on prompt lacks %q:\n%s", want, on)
		}
	}
	// The session HAS owner messages, so the one "none" line is the Chief chat's.
	if none := Prompt(s, last, sessionMsgs, 15*time.Minute, Hint{Notify: true}, nonce); strings.Count(none,
		"(none in the window muster can read)") != 1 || !strings.Contains(none, "Chief chat (quote_source chief_chat)") {
		t.Errorf("an empty Chief chat is not said to be empty:\n%s", none)
	}
	off := Prompt(s, last, sessionMsgs, 15*time.Minute, Hint{ChiefChat: chatMsgs}, nonce)
	if off != Prompt(s, last, sessionMsgs, 15*time.Minute, Hint{}, nonce) || strings.Contains(off, "chief") {
		t.Errorf("the state-off prompt reads the Chief chat:\n%s", off)
	}
}

// TestAChiefChatNudgeSaysItWasToldToTheChief: the pasted text names where the
// words were written; a session quote's text is NudgeText's, unchanged.
func TestAChiefChatNudgeSaysItWasToldToTheChief(t *testing.T) {
	m := Msg{At: time.Date(2026, 1, 2, 14, 2, 0, 0, time.UTC)}
	if got, want := NudgeTextFrom("Sam", m, "ship it", SourceChiefChat, time.UTC), `[Chief nudge] Sam told the chief at 14:02: "ship it". Continue.`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := NudgeTextFrom("Sam", m, "ship it", SourceSession, time.UTC); got != NudgeText("Sam", m, "ship it", time.UTC) {
		t.Fatalf("a session nudge changed: %q", got)
	}
}
