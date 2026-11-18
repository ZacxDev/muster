package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
)

// TestTheNudgesTabNamesWhereAQuoteCameFrom (step 3b-1): a decision's card says
// whether its quote was checked in the owner's Chief chat or in their messages
// to the session, so a shadow Chief-chat quote can be told apart; a decision
// with no recorded source (state off, or from before 0081) reads as before.
func TestTheNudgesTabNamesWhereAQuoteCameFrom(t *testing.T) {
	for _, c := range []struct{ source, want string }{
		{chiefnudge.SourceChiefChat, `data-quote-source="chief_chat">Quote from your Chief chat [chief:77]: "ship it now please"`},
		{chiefnudge.SourceSession, `data-quote-source="session">Quote from your messages to this session [chief:77]: "ship it now please"`},
		{"", `data-quote-source="">Quote [chief:77]: "ship it now please"`},
	} {
		d := chiefnudge.Decision{ID: 9, Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeShadow, Outcome: chiefnudge.OutcomeWouldSend,
			QuoteID: "chief:77", QuoteText: "ship it now please", QuoteSource: c.source}
		if got := chiefNudgeCardHTML(d, time.Now()); !strings.Contains(got, c.want) {
			t.Errorf("source %q: the card lacks %q:\n%s", c.source, c.want, got)
		}
	}
	// A suppressed Chief-chat quote says which source it was not found in; a
	// session one keeps its words.
	for src, want := range map[string]string{
		chiefnudge.SourceChiefChat: "Suppressed: the quote is not in a line you typed in your Chief chat",
		chiefnudge.SourceSession:   "Suppressed: the quote is not in one of your messages to this session",
	} {
		d := chiefnudge.Decision{ID: 9, Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeShadow,
			Outcome: chiefnudge.Suppressed(chiefnudge.GuardQuote), QuoteID: "chief:77", QuoteText: "ship it now please", QuoteSource: src}
		if got := chiefNudgeCardHTML(d, time.Now()); !strings.Contains(got, want) {
			t.Errorf("suppressed, source %q: the card lacks %q", src, want)
		}
	}
}
