package api

import (
	"context"
	"errors"
	"html"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
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
//     the card's owner (attention.IsOwnerDoor) — or, on a TEAM's card (a team
//     agent's request), any member of that team, from their own browser or
//     phone (teamMemberCardDoor; recorded by recordTeamPageCardAnswer). A mirrored session's
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
//
// byMember reports that a team MEMBER (not the card's owner) decided a TEAM's
// card: answerAskFrom then records the answer through
// recordTeamPageCardAnswer, the one door that records a viewer's answer too.
func (s *Server) pageShareForAnswer(ctx context.Context, id int64, answer string, src attention.AnswerSource) (byMember bool, err error) {
	if s.ext.Pages == nil || s.ext.Attention == nil {
		return false, nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.Status != attention.Open {
		return false, nil
	}
	l, err := s.ext.Pages.LinkByAsk(ctx, id)
	if err != nil {
		return false, nil
	}
	owner := attention.IsOwnerDoor(ctx, a.Owner)
	member := !owner && teamMemberCardDoor(ctx, a.Owner)
	if src.ViaSession() || !(owner || member) {
		return false, errReleaseRefused(pageShareNotOwnersDoor)
	}
	if l.State != pages.LinkPending {
		return false, errReleaseRefused(pageShareDecided)
	}
	var live bool
	switch strings.TrimSpace(answer) {
	case pageShareApproveLabel:
		live = true
	case pageShareDenyLabel:
	default:
		return false, errReleaseRefused(pageShareNotAnOption)
	}
	if _, err := s.ext.Pages.DecideLink(ctx, l.ID, live); err != nil {
		if errors.Is(err, pages.ErrNotPending) {
			return false, errReleaseRefused(pageShareDecided)
		}
		s.logger.Printf("pages: decide link %d: %v", l.ID, err)
		return false, errReleaseRefused("The link could not be decided. Try again.")
	}
	decision := "denied"
	if live {
		decision = "approved"
	}
	metrics.PageShareDecisions.WithLabelValues(decision).Inc()
	by := "its owner"
	if member {
		by = "a member of its team"
	}
	s.logger.Printf("pages: link %d of page %d %s by %s", l.ID, l.PageID, decision, by)
	return member, nil
}

// teamPageCardAnswerer is the attention store's record of a team member's
// answer to a team page share card (attention.PGStore.AnswerTeamPageCard).
type teamPageCardAnswerer interface {
	AnswerTeamPageCard(ctx context.Context, id int64, answer string) (attention.Ask, error)
}

// recordTeamPageCardAnswer records a team MEMBER's answer to a team agent's
// page share card, after pageShareForAnswer decided its link.
//
// 🔴 NOT THE ORDINARY ANSWER: 0127 writes attention_asks at OPERATOR, so a
// VIEWER's answer would decide the link and then not record — the card left
// open over a live link. 0165's muster_answer_team_page_card records it, for
// this one kind of row only (an open owner card of a team the caller is a
// member of, from their browser or phone, whose link the caller has already
// decided), the caller recorded as answered_by_user.
func (s *Server) recordTeamPageCardAnswer(ctx context.Context, id int64, answer string) (attention.Ask, error) {
	st, ok := s.ext.Attention.(teamPageCardAnswerer)
	if !ok {
		return attention.Ask{}, errReleaseRefused("This card cannot be answered by a team member on this server.")
	}
	return st.AnswerTeamPageCard(ctx, id, answer)
}

// teamMemberCardDoor reports whether ctx is a PERSON answering a TEAM's page
// share card (a team agent's page_share_request, owner team) from their own
// door — a signed-in browser or their paired phone, the doors that carry
// their memberships (authz.Principal.TeamScope) — as ANY member of that team,
// viewer included (operator decision D1: the card is the same act as Share,
// which every member has, and 0165's link guard lets every member decide a
// pending link). A team agent's link goes live only on such a tap: no person
// is the team, so the team's own door (attention.IsOwnerDoor) is never a
// person's. The answer is recorded by recordTeamPageCardAnswer, since 0127
// lets a viewer write no attention_asks row.
func teamMemberCardDoor(ctx context.Context, team int64) bool {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.Team || team <= 0 || p.UserID == team {
		return false
	}
	if p.Via != authz.ViaSession && p.Via != authz.ViaDevice {
		return false
	}
	_, in := p.TeamScope().RoleIn(team)
	return in
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
//
// On a TEAM's page revoked by a member (its creator or a manager, whatever
// their role), the card is the team's, which 0127 lets only an operator
// write: it is withdrawn through 0165's narrow door
// (attention.PGStore.CancelTeamPageCard), never the ordinary cancel, which
// would fail for a viewer-role creator and leave the card answerable.
func (s *Server) withdrawLinkCard(ctx context.Context, l pages.Link) {
	if l.AskID <= 0 || s.ext.Attention == nil {
		return
	}
	var err error
	if p, ok := authz.From(ctx); ok && !p.All() && !p.Team && l.OwnerID != p.UserID {
		if st, ok := s.ext.Attention.(teamPageCardCanceller); ok {
			var a attention.Ask
			if a, err = st.CancelTeamPageCard(ctx, l.AskID); err == nil {
				s.askSettled(a)
			}
		}
	} else {
		_, err = s.cancelAsk(ctx, l.AskID)
	}
	if err != nil && !errors.Is(err, attention.ErrNotOpen) && !errors.Is(err, attention.ErrNotFound) {
		s.logger.Printf("pages: link %d revoked; withdraw its card %d: %v", l.ID, l.AskID, err)
	}
}

// teamPageCardCanceller is the attention store's withdrawal of a team page
// link's card by a member (attention.PGStore.CancelTeamPageCard).
type teamPageCardCanceller interface {
	CancelTeamPageCard(ctx context.Context, id int64) (attention.Ask, error)
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
