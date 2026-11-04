package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/chiefnudge"
)

// The step-3b-1 tests (chief_chat_quotes_pg_test.go) reach what is NEW in
// 3b-1 only through these helpers, so the same test file runs against
// the base (origin/main before 3b-1) with this file replaced by no-op stubs —
// chiefChatQuotesLive does nothing, quoteSourceOf is "", withQuoteSource
// returns d unchanged, withChiefChatInView likewise, chiefChatInViewOf is
// false, failChiefChatRead does nothing, and nudgeAgents has no
// OwnerTypedChatMessages — to show
// each regression test RED there.

// chiefChatQuotesLive sets MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS in
// f's config to owners (none: the list is empty).
func chiefChatQuotesLive(f *nudgeFixture, owners ...int64) {
	m := map[int64]bool{}
	for _, o := range owners {
		m[o] = true
	}
	f.cfg.ChiefChatQuotesLive = m
}

// quoteSourceOf is where d's quote was checked (chief_nudges.quote_source).
func quoteSourceOf(d chiefnudge.Decision) string { return d.QuoteSource }

// withChiefChatInView is d recording that the owner's typed Chief-chat lines
// were in the chief's prompt (chief_nudges.chief_chat_in_view).
func withChiefChatInView(d chiefnudge.Decision) chiefnudge.Decision {
	d.ChiefChatInView = true
	return d
}

// chiefChatInViewOf is d's stored chief_chat_in_view.
func chiefChatInViewOf(d chiefnudge.Decision) bool { return d.ChiefChatInView }

// withQuoteSource is d recording src as its quote's source.
func withQuoteSource(d chiefnudge.Decision, src string) chiefnudge.Decision {
	d.QuoteSource = src
	return d
}

// chiefChatReadFails names the nudge fixtures whose Chief-chat read fails.
var chiefChatReadFails sync.Map // *nudgeAgents -> true

// failChiefChatRead makes f's Chief-chat read fail until the test ends.
func failChiefChatRead(t *testing.T, f *nudgeFixture) {
	a := f.srv.ext.Agents.(*nudgeAgents)
	chiefChatReadFails.Store(a, true)
	t.Cleanup(func() { chiefChatReadFails.Delete(a) })
}

// OwnerTypedChatMessages passes the Chief-chat read through the nudge
// fixture's agent store to the real one (chiefChatReader), or fails it.
func (s *nudgeAgents) OwnerTypedChatMessages(ctx context.Context, agentID, by int64, since time.Time, limit int) ([]agents.ChatMessage, error) {
	if _, fail := chiefChatReadFails.Load(s); fail {
		return nil, errors.New("nudgeAgents: the Chief-chat read failed (fixture)")
	}
	r, ok := s.Store.(chiefChatReader)
	if !ok {
		return nil, errors.New("nudgeAgents: the store cannot read typed turns")
	}
	return r.OwnerTypedChatMessages(ctx, agentID, by, since, limit)
}
