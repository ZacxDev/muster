package api

import (
	"context"
	"net/url"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
)

// closedCardsOf is asks as cards with nothing read: items[i] is asks[i] as an
// item, proposals the proposal cards. What a test's fixture draws.
func closedCardsOf(asks []attention.Ask, items []attentionItem, proposals map[int64]bool) []closedCard {
	out := make([]closedCard, len(asks))
	for i, a := range asks {
		out[i] = closedCard{a: a, it: items[i], proposal: proposals[a.ID]}
	}
	return out
}

// doneAsks is the Done tab's first page's asks, unfiltered.
func (s *Server) doneAsks(ctx context.Context) []attention.Ask {
	p := s.readDone(ctx, doneCursor{}, asksFilter{}, s.liveHosts(ctx))
	out := make([]attention.Ask, len(p.cards))
	for i, c := range p.cards {
		out[i] = c.a
	}
	return out
}

// doneFeedNow is the Done tab's first page's feed, unfiltered.
func (s *Server) doneFeedNow(ctx context.Context, now time.Time) string {
	p := s.readDone(ctx, doneCursor{}, asksFilter{}, s.liveHosts(ctx))
	return doneFeedHTML(p, asksFilter{}, url.Values{}, now)
}
