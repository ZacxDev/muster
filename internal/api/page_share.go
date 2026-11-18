package api

import (
	"context"
	"errors"
	"html"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/textplain"
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

// pageShareQuestion is the card's question: which page, by its title, in
// plain words, and who asks. The title is the agent's words, plained here
// (textplain.Title) and escaped by every surface that draws the question.
//
// 🔴 NO ADDRESS AND NO ID (the operator rule: none in the top layer). The
// page is one tap away on the card itself — its "Open the page" button
// (pageAskOpenHTML), found from the card's link — never a URL in the text.
func pageShareQuestion(asker string, p pages.Page, reason string) string {
	// 🔴 THE APPROVAL IS FOR THE PAGE, NOT FOR ONE VERSION (operator
	// decision, round 1): a live link shows whatever is published last, and a
	// republish needs no new approval — so the card must not say otherwise.
	q := "Share the page “" + truncate(textplain.Title(p.Title), 120) + "”? " + pageMakerName(asker, p.Creator.MirrorHost) + " asks to let anyone who has its link open it."
	if reason = strings.TrimSpace(reason); reason != "" {
		q += " Its reason: " + truncate(reason, 300)
	}
	return q + " The link always shows the page's latest version, including changes the agent makes after you approve. " +
		"Approving makes the link live; you can revoke it on the page at any time."
}

// pageQuietButton is a quiet button as the page card draws one (ui's
// pageButtonQuiet), for a link or a button on one line with others.
const pageQuietButton = "press inline-flex min-h-[44px] items-center justify-center gap-1.5 rounded-lg bg-bg px-3 text-sm font-semibold text-fg ring-1 ring-inset ring-edge"

// pageAskOpenLabel is the ask card's button to the page it asks about.
const pageAskOpenLabel = "Open the page"

// pageAskOpen is the owner's view of the page whose share link ask id asks for,
// "" when it is no page's card (or pages are not served): what the card's "Open the
// page" opens. ctx is the reader's: the link is read under their row-level
// security, so another owner's card finds none.
func (s *Server) pageAskOpen(ctx context.Context, id int64) string {
	if !s.pagesOn() || id <= 0 {
		return ""
	}
	l, err := s.ext.Pages.LinkByAsk(ctx, id)
	if err != nil || l.PageID <= 0 {
		return ""
	}
	return pageViewPath(l.PageID)
}

// pageAskOpenHTML is the card's "Open the page" button, "" for none.
func pageAskOpenHTML(href string) string {
	if href == "" {
		return ""
	}
	return `<div class="flex flex-wrap items-center gap-2" data-asks-controls><a href="` + html.EscapeString(href) +
		`" class="` + pageQuietButton + `" data-ask-page-open>` + pageAskOpenLabel + `</a></div>
`
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
