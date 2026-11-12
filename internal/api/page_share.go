package api

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/push"
)

// ---------------------------------------------------------------------------
// THE OWNER'S APPROVAL OF A SHARE LINK (docs/design/pages.md#approval-flow).
//
// An agent's page_share_request files a PENDING link and an OWNER CARD (an
// attention ask with Approve link / Not now) from the caller. The owner sees it
// on the Asks page and their phone. Its answer runs pageShareForAnswer BEFORE
// it is recorded (answerAskFrom, beside the routing rules' card):
//
//   - 🔴 THE OWNER'S OWN DOOR ONLY: a signed-in browser or the paired phone, as
//     the card's owner (attention.IsOwnerDoor). A mirrored session's
//     attention_answer — the one door an agent could reach — is refused here,
//     before anything is decided, and the owner card refuses it again in SQL
//     (answerSQL). So no agent can approve its own (or another's) link.
//   - the answer must be one of the card's two options, spelled exactly;
//   - the link must still be pending (decided once).
//
// Approve link makes it live; Not now denies it. The asker learns the answer as
// from any ask, then reads the URL with page_get — a pending or denied link
// never shows it. A card settled by anything else (expired, withdrawn, the
// owner typing to the agent) decides nothing: the link stays pending and
// serves nothing.
// ---------------------------------------------------------------------------

// The card's two options.
const (
	pageShareApproveLabel = "Approve link"
	pageShareDenyLabel    = "Not now"
)

// The approval's refusals, as the owner reads them.
const (
	pageShareNotOwnersDoor = "Only you can approve a page's link, in muster or on your paired phone — not from a session."
	pageShareDecided       = "That link was already decided."
	pageShareNotAnOption   = "Answer with “" + pageShareApproveLabel + "” or “" + pageShareDenyLabel + "”."
)

// pushEventPage is the owner push of a page's response.
const pushEventPage = push.EventPage

// pageShareForAnswer is THE TAP on a page link's card (answerAskFrom, BEFORE
// the answer is recorded); nil when ask id is no link's card. See the file
// header for its three rules.
func (s *Server) pageShareForAnswer(ctx context.Context, id int64, answer string, src attention.AnswerSource) error {
	if s.ext.Pages == nil || s.ext.Attention == nil {
		return nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.Status != attention.Open {
		return nil
	}
	l, err := s.ext.Pages.LinkByAsk(ctx, id)
	if err != nil {
		return nil
	}
	if src.ViaSession() || !attention.IsOwnerDoor(ctx, a.Owner) {
		return errReleaseRefused(pageShareNotOwnersDoor)
	}
	if l.State != pages.LinkPending {
		return errReleaseRefused(pageShareDecided)
	}
	var live bool
	switch strings.TrimSpace(answer) {
	case pageShareApproveLabel:
		live = true
	case pageShareDenyLabel:
	default:
		return errReleaseRefused(pageShareNotAnOption)
	}
	if _, err := s.ext.Pages.DecideLink(ctx, l.ID, live); err != nil {
		if errors.Is(err, pages.ErrNotPending) {
			return errReleaseRefused(pageShareDecided)
		}
		s.logger.Printf("pages: decide link %d: %v", l.ID, err)
		return errReleaseRefused("The link could not be decided. Try again.")
	}
	decision := "denied"
	if live {
		decision = "approved"
	}
	metrics.PageShareDecisions.WithLabelValues(decision).Inc()
	s.logger.Printf("pages: link %d of page %d %s by its owner", l.ID, l.PageID, decision)
	return nil
}

// pageShareQuestion is the card's question: who asks, which page, where to
// preview it. The title is the agent's words (the Asks page escapes it).
func (s *Server) pageShareQuestion(asker string, p pages.Page, reason string) string {
	// 🔴 THE APPROVAL IS FOR THE PAGE, NOT FOR ONE VERSION (operator
	// decision, round 1): a live link shows whatever is published last, and a
	// republish needs no new approval — so the card must not say otherwise.
	q := asker + " asks to share your page “" + truncate(p.Title, 120) + "” with anyone who has its link. The link always shows " +
		"the page's latest published version, including versions the agent publishes after you approve. See the page as it is now: " +
		s.pagePreviewURL(p.ID) + "."
	if reason = strings.TrimSpace(reason); reason != "" {
		q += " Its reason: " + truncate(reason, 300)
	}
	return q + " Approving makes the link live; you can revoke it on the page at any time."
}

// withdrawLinkCard cancels a revoked link's still-open card, so a later tap
// finds nothing to approve.
func (s *Server) withdrawLinkCard(ctx context.Context, l pages.Link) {
	if l.AskID <= 0 || s.ext.Attention == nil {
		return
	}
	if _, err := s.cancelAsk(ctx, l.AskID); err != nil && !errors.Is(err, attention.ErrNotOpen) && !errors.Is(err, attention.ErrNotFound) {
		s.logger.Printf("pages: link %d revoked; withdraw its card %d: %v", l.ID, l.AskID, err)
	}
}

// pageWaits wakes page_responses calls blocked on a page.
type pageWaits struct {
	mu sync.Mutex
	ch map[int64]chan struct{}
	// in is each caller's waits in flight (page_responses' bound).
	in map[string]int
}

// enter counts one wait of caller key, refusing past max; leave ends it.
func (p *pageWaits) enter(key string, max int) (leave func(), ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.in == nil {
		p.in = map[string]int{}
	}
	if p.in[key] >= max {
		return nil, false
	}
	p.in[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.in[key]--; p.in[key] <= 0 {
				delete(p.in, key)
			}
		})
	}, true
}

// wait is a channel closed at the next wake of page id.
func (p *pageWaits) wait(id int64) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch == nil {
		p.ch = map[int64]chan struct{}{}
	}
	c, ok := p.ch[id]
	if !ok {
		c = make(chan struct{})
		p.ch[id] = c
	}
	return c
}

// wake releases every wait on page id.
func (p *pageWaits) wake(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.ch[id]; ok {
		close(c)
		delete(p.ch, id)
	}
}
