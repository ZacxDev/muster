package api

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// NOTHING THE OWNER DECIDED IS EVER LOST (operator report: a card rejected
// with a reason vanished from the feed at once, leaving no way back to it or
// to its session).
//
// 🔴 A DECIDED CARD STAYS WHERE IT WAS, SETTLED. An answer, a dismissal, a
// step-up card's draft rejected or approved on the Open feed: the card does
// not leave the feed. It is drawn again, in its place, as THE closed card
// (closedCardHTML): what was decided, the owner's reason, "View in session"
// and the way to its own page. It leaves the Open feed on the next reload of
// the page, and from then on it is on the Done tab. Two doors bring it back:
//
//   - a card form's htmx post (asksCardRequest) is answered 200 with the
//     settled card itself, which the feed's script puts in the card's place
//     (ui/asks_card.go settle) and keeps there across the live refreshes —
//     the server no longer lists a closed ask, so a refresh would drop it;
//   - every plain POST that lands back on the Asks page (a step-up card's
//     draft Reject or Approve, a team card's step-up answer, a form posted
//     with no script) is sent to settledLink: /attention?settled=<id>#ask-<id>.
//     The page draws that one closed ask in its old spot on the Open feed
//     (settledFeedItem) — a request naming it, never a cookie or a session
//     store — and the page's script drops ?settled= from the address once
//     drawn, so a reload is the reload that lets it go.
//
// ONE CARD, BOTH PLACES: the settled card on the Open feed and a card on the
// Done tab are closedCardHTML's, so they say the same thing.
//
// 🔴 THE SETTLED LANDING DRAWS WHAT THE READER CAN ALREADY READ: their own
// ask, or a team's they are a member of (a team card a member confirmed with
// a fresh sign-in lands here too: teamCardStepUpWhy) — row-level security,
// and readerMayRead's explicit check again with it rolled back. Any other id
// — another owner's, a missing one, an open one — draws nothing, exactly
// alike: the address is no probe.
//
// THE DONE TAB (it was "Answered": the answers of the last day, at most 20)
// lists EVERY closed ask and notice the owner owns — answered, answered in
// the session, dismissed, expired — newest first, with no age window, paged
// by key (attention.Closed: "Show older" is the next page's link) — with an
// answer whose DELIVERY FAILED pinned above its first page however old
// (attention.Unconfirmed: it waits on the owner's Retry, which the Answered
// list it replaced always offered), and not repeated in the order below. The
// filter's chips (asks_filter.go) narrow it; a filtered page reads on through
// the store until it holds a page of cards or asksDoneScanPages store pages
// were read. ?tab=answered, the old name, opens it (asksTab), so a link that
// already lives on a phone still lands there. Its feed's DOM ids keep their
// old names (#attention-answered, #answered-<id>): a fragment already in a
// link still finds its card.

const (
	// asksTabDone is the Done tab: everything closed.
	asksTabDone = "done"
	// asksTabAnswered is the Done tab's OLD name: ?tab=answered opens Done.
	asksTabAnswered = "answered"

	// asksDoneShown is how many cards one page of the Done tab draws.
	asksDoneShown = 25
	// asksDoneScanPages bounds how many store pages one FILTERED Done page
	// reads looking for asksDoneShown cards the filter keeps.
	asksDoneScanPages = 8

	// asksBeforeParam is the Done tab's page: the cursor of the last card the
	// page before it drew (doneCursor).
	asksBeforeParam = "before"
	// asksSettledParam names the ask a decision just closed, drawn settled in
	// its old spot on the Open feed (settledFeedItem).
	asksSettledParam = "settled"
)

// attentionDonePath is the Asks page on its Done tab: where a Retry lands, and
// where its refusal is drawn.
const attentionDonePath = attentionPath + "?" + asksTabParam + "=" + asksTabDone

// settledLink is where a decision made on the Asks page lands: the page, its
// Open feed drawing ask id settled in its old spot, scrolled to it.
func settledLink(id int64) string {
	n := strconv.FormatInt(id, 10)
	return attentionPath + "?" + asksSettledParam + "=" + n + "#" + attentionAskKey(id)
}

// doneCursor is a Done page's key: the ClosedAt and id of the last card the
// page before it drew. The zero cursor is the first page.
type doneCursor struct {
	at time.Time
	id int64
}

func (c doneCursor) zero() bool { return c.id <= 0 || c.at.IsZero() }

// String is c as the address carries it: "<unix microseconds>.<id>" (the
// database keeps microseconds, so the key round-trips exactly).
func (c doneCursor) String() string {
	if c.zero() {
		return ""
	}
	return strconv.FormatInt(c.at.UnixMicro(), 10) + "." + strconv.FormatInt(c.id, 10)
}

// parseDoneCursor is the cursor v names, the zero cursor for none or a
// malformed one (the first page).
func parseDoneCursor(v string) doneCursor {
	at, id, ok := strings.Cut(strings.TrimSpace(v), ".")
	if !ok {
		return doneCursor{}
	}
	us, err1 := strconv.ParseInt(at, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil || us <= 0 || n <= 0 {
		return doneCursor{}
	}
	return doneCursor{at: time.UnixMicro(us).UTC(), id: n}
}

func doneCursorOf(a attention.Ask) doneCursor { return doneCursor{at: a.ClosedAt(), id: a.ID} }

// closedCard is one closed ask as its card draws it: the ask, it as a named
// attention item (closedItems; its page and "View in session" set by
// withClosedCards), whether it is a chief proposal's card, and — for a
// step-up card about a draft — that draft as the reader may read it (its
// decision and the owner's reason), nil for none.
type closedCard struct {
	a        attention.Ask
	it       attentionItem
	proposal bool
	draft    *plugins.Draft
}

// closedItems is asks as attention items — a notice as a notice — named as an
// Open card is (withNames), each with its own page where the reader may open
// it (withAskPages).
//
// hosts is the render's one snapshot of the owner's windows (liveHosts),
// read only when an ask needs them.
func (s *Server) closedItems(ctx context.Context, asks []attention.Ask, hostsOf func() []mirror.HostView) []attentionItem {
	notice := func(a attention.Ask) bool { return a.Kind == attention.KindNotice }
	var hosts []mirror.HostView
	if slices.ContainsFunc(asks, askNeedsHosts) || slices.ContainsFunc(asks, notice) {
		hosts = hostsOf()
	}
	var ds map[int64]chiefnudge.Decision
	if slices.ContainsFunc(asks, notice) {
		var err error
		if ds, err = s.noticeDecisions(ctx, asks); err != nil {
			s.logger.Printf("asks: the closed notices' decisions: %v", err)
		}
	}
	items := make([]attentionItem, len(asks))
	for i, a := range asks {
		if notice(a) {
			items[i] = noticeItem(a, ds, hosts)
		} else {
			items[i] = askItem(a, hosts...)
		}
	}
	s.withNames(ctx, items)
	withAskPages(ctx, items)
	return items
}

// closedCards is asks (items[i] is asks[i] as closedItems made it) as their
// cards: each with its session wake, whether it is a proposal's card, a
// step-up card's draft (one batch read, row-level security: another person's
// draft is absent) and its "View in session" (attentionSessionPath, the ask
// page's own rule; for a step-up card about a draft, the draft's session:
// draftSessionPath), over hosts, the render's one snapshot of the windows.
func (s *Server) closedCards(ctx context.Context, asks []attention.Ask, items []attentionItem, hosts func() []mirror.HostView) []closedCard {
	out := make([]closedCard, len(asks))
	ws := s.wakesForAsks(ctx, asks)
	var ids []int64
	for _, a := range asks {
		if a.StepUp != nil && a.StepUp.DraftID > 0 && !slices.Contains(ids, a.StepUp.DraftID) {
			ids = append(ids, a.StepUp.DraftID)
		}
	}
	drafts := map[int64]plugins.Draft{}
	if len(ids) > 0 && s.ext.Plugins != nil {
		got, err := s.ext.Plugins.DraftCardsByID(ctx, ids)
		if err != nil {
			s.logger.Printf("asks: the closed step-up cards' drafts: %v", err)
		}
		for _, c := range got {
			drafts[c.Draft.ID] = c.Draft
		}
	}
	owner := requestOwner(ctx)
	// An agent's page is read once per agent, not once per card: a page of
	// one agent's closed asks is one read.
	agentHref := map[int64]string{}
	for i, a := range asks {
		c := closedCard{a: a, it: items[i], proposal: s.isProposalCard(ctx, a)}
		if id := c.it.card.agentID; owner > 0 && c.it.Kind == attentionKindAsk && id > 0 {
			if _, ok := agentHref[id]; !ok {
				agentHref[id] = s.attentionSessionPathIn(ctx, owner, c.it, hosts)
			}
			c.it.sessionLink = agentHref[id]
		}
		if w, ok := ws[a.ID]; ok {
			c.it.wake = &w
		}
		if a.StepUp != nil {
			if d, ok := drafts[a.StepUp.DraftID]; ok {
				c.draft = &d
			}
		}
		if owner > 0 {
			if _, read := agentHref[c.it.card.agentID]; !read || c.it.Kind != attentionKindAsk {
				c.it.sessionLink = s.attentionSessionPathIn(ctx, owner, c.it, hosts)
			}
			if c.it.sessionLink == "" && c.draft != nil {
				c.it.sessionLink = s.draftSessionPath(ctx, owner, *c.draft)
			}
		}
		out[i] = c
	}
	return out
}

// draftSessionPath is "View in session" for a step-up card about draft d: the
// page of the agent that drafted it (askingAgentHref, owner-checked), "" for
// a draft no agent of owner's wrote.
func (s *Server) draftSessionPath(ctx context.Context, owner int64, d plugins.Draft) string {
	if d.AgentID == nil || *d.AgentID <= 0 {
		return ""
	}
	return s.askingAgentHref(ctx, owner, *d.AgentID, 0)
}

// donePage is one page of the Done tab: its cards, every item the page READ
// (kept by the filter or not: what the chips count), the next page's cursor
// (zero: no older page), and whether the store failed.
type donePage struct {
	cards  []closedCard
	read   []attentionItem
	older  doneCursor
	failed bool
}

// readDone is the Done tab's page before cursor before, filter f applied (see
// the file's header), as ctx's principal, over hosts (the render's one
// snapshot of the owner's windows: liveHosts).
//
// 🔴 A FAILED DELIVERY IS NEVER PAGED AWAY: the owner's unconfirmed answers
// (attention.Unconfirmed, however old) head the FIRST page, the filter
// applied, and are skipped wherever the closed-time order would draw them
// again — on every page, so none is drawn twice.
func (s *Server) readDone(ctx context.Context, before doneCursor, f asksFilter, hosts func() []mirror.HostView) donePage {
	var p donePage
	if s.ext.Attention == nil {
		return p
	}
	pinned, err := s.ext.Attention.Unconfirmed(ctx)
	if err != nil {
		s.logger.Printf("asks: the Done tab's unconfirmed answers: %v", err)
	}
	pin := map[int64]bool{}
	for _, a := range pinned {
		pin[a.ID] = true
	}
	var pinAsks []attention.Ask
	var pinItems []attentionItem
	if before.zero() && len(pinned) > 0 {
		items := s.closedItems(ctx, pinned, hosts)
		x := s.readAskSources(ctx, nil, items)
		for i, a := range pinned {
			p.read = append(p.read, items[i])
			if f.keepItem(x, items[i]) {
				pinAsks, pinItems = append(pinAsks, a), append(pinItems, items[i])
			}
		}
	}
	cur := before
	var keptAsks []attention.Ask
	var keptItems []attentionItem
	done := false
	for pass := 0; pass < asksDoneScanPages && !done; pass++ {
		// One more than a page: whether an older page exists is known
		// without a second read.
		asks, err := s.ext.Attention.Closed(ctx, attention.ClosedPage{BeforeAt: cur.at, BeforeID: cur.id, Limit: asksDoneShown + 1})
		if err != nil {
			s.logger.Printf("asks: the Done tab: %v", err)
			p.failed = true
			break
		}
		items := s.closedItems(ctx, asks, hosts)
		x := s.readAskSources(ctx, nil, items)
		for i, a := range asks {
			if pin[a.ID] {
				// Pinned above the first page: never drawn twice.
				cur = doneCursorOf(a)
				continue
			}
			if len(keptAsks) == asksDoneShown {
				// A page is full and another closed ask is older: the next
				// page starts after the last card drawn.
				p.older = doneCursorOf(keptAsks[len(keptAsks)-1])
				done = true
				break
			}
			cur = doneCursorOf(a)
			p.read = append(p.read, items[i])
			if f.keepItem(x, items[i]) {
				keptAsks, keptItems = append(keptAsks, a), append(keptItems, items[i])
			}
		}
		if !done && len(asks) <= asksDoneShown {
			// The store has nothing older.
			done = true
		}
	}
	if !done && !p.failed {
		// The scan's bound, not the store's end: the next page reads on from
		// the last ask read.
		p.older = cur
	}
	p.cards = s.closedCards(ctx, append(pinAsks, keptAsks...), append(pinItems, keptItems...), hosts)
	return p
}

// doneFeedHTML is the Done tab's feed's <li>s for page p: a card per closed
// ask, then "Show older" (the next page, q's filter and tab kept) when there
// is an older page; its empty line when there is none — the filter's when f
// is on. q is the request's query.
func doneFeedHTML(p donePage, f asksFilter, q url.Values, now time.Time) string {
	var b strings.Builder
	if len(p.cards) == 0 {
		switch {
		case f.on():
			b.WriteString(asksFilterEmptyHTML(` data-asks-empty="answered"`))
		case p.failed:
			b.WriteString(ui.AsksEmptyHTML(` data-asks-empty="answered" data-asks-done-failed`, "", "", "The closed asks could not be read.", ""))
		case !parseDoneCursor(q.Get(asksBeforeParam)).zero():
			b.WriteString(ui.AsksEmptyHTML(` data-asks-empty="answered" data-asks-answered-empty`, "", "", "Nothing older.", ""))
		default:
			b.WriteString(ui.AsksEmptyHTML(` data-asks-empty="answered" data-asks-answered-empty`, "", "", "Nothing has closed yet.", ""))
		}
	}
	b.WriteString(closedFeedHTML(p.cards, now))
	if !parseDoneCursor(q.Get(asksBeforeParam)).zero() || !p.older.zero() {
		b.WriteString(donePagerHTML(q, p.older))
	}
	return b.String()
}

// donePagerHTML is the Done feed's last row: "Newest" back to its first page
// when this is not it, "Show older" to the next page when there is one — plain
// links (the page works without its script), q's filter and tab kept.
func donePagerHTML(q url.Values, older doneCursor) string {
	link := func(c doneCursor) string {
		v := url.Values{}
		for _, k := range []string{asksTypeParam, askerFromParam} {
			if s := strings.TrimSpace(q.Get(k)); s != "" {
				v.Set(k, s)
			}
		}
		v.Set(asksTabParam, asksTabDone)
		if !c.zero() {
			v.Set(asksBeforeParam, c.String())
		}
		return attentionPath + "?" + v.Encode()
	}
	var b strings.Builder
	b.WriteString(`<li class="flex min-w-0 flex-wrap items-center justify-center gap-2 py-2" data-asks-done-pager>`)
	if !parseDoneCursor(q.Get(asksBeforeParam)).zero() {
		b.WriteString(`<a href="` + html.EscapeString(link(doneCursor{})) + `" class="` + ui.AsksCardLinkClass + `" data-asks-done-newest>Newest</a>`)
	}
	if !older.zero() {
		b.WriteString(`<a href="` + html.EscapeString(link(older)) + `" class="` + ui.AsksCardLinkClass + `" data-asks-done-older>Show older</a>`)
	}
	b.WriteString("</li>\n")
	return b.String()
}

// closedFeedHTML is cards as the Done tab draws them (closedCardHTML, each
// keyed answered-<id>), empty for none: the caller draws the empty line.
func closedFeedHTML(cards []closedCard, now time.Time) string {
	var b strings.Builder
	for _, c := range cards {
		b.WriteString(closedCardHTML(c, "answered-"+strconv.FormatInt(c.a.ID, 10), closedOnDone, now))
	}
	return b.String()
}

// closedPlace is where a closed card is drawn.
type closedPlace int

const (
	// closedOnDone: a card of the Done tab.
	closedOnDone closedPlace = iota
	// closedSettled: settled in place on the Open feed (data-asks-settled;
	// ui.AsksSettledCardClass) — the card a card form's htmx post answers.
	closedSettled
	// closedLanding: settled, and where a plain form's decision landed
	// (?settled=: data-asks-landing) — the page brings it into view, the
	// fragment that would have lost by a boosted form's redirect.
	closedLanding
)

// closedState is a closed card's state line: a step-up card's recorded answer
// as its state ("Rejected in the web UI", what settleDraftStepUps records,
// or the phone's Confirm), else the ask's state in words (askClosedState).
func closedState(a attention.Ask) string {
	if a.StepUp != nil && a.Status == attention.Answered && strings.TrimSpace(a.Answer) != "" {
		return a.Answer
	}
	return askClosedState(a.Status)
}

// closedCardHTML is THE CLOSED CARD (see the file's header), keyed key: the
// Open card's head and body, then its outcome — its state, the answer (who
// gave it: answerLabel) or the owner's reason with a draft's decision (a
// step-up card's, read from the draft), where an answer went — Retry on a
// failed delivery, and its footer: "View in session" when it has a session,
// its own page, the task. at is where it is drawn (closedPlace).
//
// 🔴 IT IS TITLED BY THE ONE NAMING RULE (attentionTitle over it, the ask as an
// attention item with its stored name: closedItems) — the session's human
// name, its tmux target small under it — exactly as its card was titled on
// the Open tab.
func closedCardHTML(c closedCard, key string, at closedPlace, now time.Time) string {
	esc := html.EscapeString
	a, it := c.a, c.it
	id := strconv.FormatInt(a.ID, 10)
	who, src, srcIsTitle := attentionTitle(it)
	answered := a.Status == attention.Answered
	// WHERE THE ANSWER WENT: an answer to an asker (not a step-up card's,
	// which muster itself raised and nothing delivers).
	var text, pill string
	var retry bool
	delivery := "none"
	if answered && a.StepUp == nil {
		text, retry = answerDeliveryText(a, who, c.proposal)
		pill = answerDeliveryPill(a, c.proposal)
		delivery = firstNonBlank(string(a.Delivery), "none")
	}
	wakeForm := ""
	if it.wake != nil {
		// A mirrored session's answer or dismissal: where its session wake
		// went (session_wake.go), with the wake's own action in place of Retry.
		var act wakeAction
		text, act = wakeDeliveryText(*it.wake, who)
		retry = false
		pill, delivery = wakeDeliveryPill(*it.wake), "wake-"+string(it.wake.State)
		wakeForm = wakeActionFormHTML(it.wake.ID, act, ui.AsksSecondaryButtonClass, ` data-attention-redeliver`)
	}
	var sub []string
	if at := a.ClosedAt(); !at.IsZero() {
		sub = append(sub, `<time datetime="`+at.UTC().Format(time.RFC3339)+`">`+esc(attentionAge(now.Sub(at)))+`</time>`)
	}
	if !srcIsTitle && src != "" && src != who {
		sub = append(sub, `<span class="`+ui.AsksTargetClass+`" data-attention-source>`+esc(src)+`</span>`)
	}
	age := ""
	if len(sub) > 0 {
		age = `<span class="` + ui.AsksSubClass + `">` + strings.Join(sub, ` · `) + `</span>`
	}
	titleAttrs := ` data-attention-title`
	if srcIsTitle || src == "" || src == who {
		titleAttrs += ` data-attention-source`
	}
	// THE MORE REGION: the sentence that says where the answer went, and
	// where it was given when it was not this view.
	more := ""
	if text != "" || answeredViaHTML(a) != "" {
		more = `<p class="break-words text-xs text-fg2" data-attention-delivery>` + esc(text) + `</p>` + answeredViaHTML(a)
	}
	moreID := ""
	if more != "" {
		moreID = asksCardMoreID(key)
	}
	badge := ""
	if it.Kind == attentionKindNotice {
		badge = ui.AsksKindBadge("notice", "Notice")
	}
	cls, attrs := ui.AsksFeedCardClass, ""
	switch at {
	case closedSettled:
		cls, attrs = ui.AsksSettledCardClass, ` data-asks-settled`
	case closedLanding:
		cls, attrs = ui.AsksSettledCardClass, ` data-asks-settled data-asks-landing`
	}
	if answered {
		attrs += ` data-attention-answered`
	}
	var b strings.Builder
	b.WriteString(`<li id="` + esc(key) + `" class="` + cls + `" data-asks-card data-asks-closed` + attrs + ` data-kind="` + esc(it.Kind) + `" data-status="` + esc(string(a.Status)) +
		`" data-expanded="false" tabindex="-1" data-ask-id="` + id + `" data-delivery="` + esc(delivery) + `">
` + ui.AsksHeadHTML(ui.AsksHead{ID: key, Badge: badge, Who: `<span class="` + ui.AsksTitleClass + `"` + titleAttrs + `>` + esc(who) + `</span>` + age, Toggle: true, More: moreID, Chip: ui.AskIDChipHTML(a.ID), Href: it.page}) + `
` + askCardQuestionHTML(it, askTextHTML(a.Question)) + closedOutcomeHTML(c))
	if pill != "" {
		b.WriteString(answeredDeliveryRowHTML(it, key, `<div class="`+ui.AsksPillsClass+`"><span class="`+ui.AsksPillClass+`" data-attention-delivery-pill>`+esc(pill)+`</span></div>`) + "\n")
	}
	b.WriteString(wakeForm)
	if retry {
		b.WriteString(`<form method="POST" action="` + attentionPath + `/asks/` + id + `/redeliver" data-asks-controls><button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-attention-redeliver>Retry delivery</button></form>
`)
	}
	if more != "" {
		b.WriteString(`<div id="` + esc(moreID) + `" class="` + ui.AsksCardMoreClass + `" data-asks-card-more><div class="` + ui.AsksCardMoreInnerClass + `">` + more + `</div></div>
`)
	}
	// THE FOOTER: the way to the session, the ask's own page, the task.
	var links []string
	if href := askFeedSessionHref(it); href != "" {
		links = append(links, `<a href="`+esc(href)+`" class="`+ui.AsksCardLinkClass+`" data-asks-view-session>`+askViewInSession+`</a>`)
	}
	if it.page != "" {
		links = append(links, `<a href="`+esc(it.page)+`" class="`+ui.AsksCardLinkClass+`" data-asks-ask-link>Open ask</a>`)
	}
	if it.taskID > 0 {
		links = append(links, `<a href="`+esc(canonicalTaskPath(it.taskID))+`" class="`+ui.AsksCardLinkClass+`" data-asks-task>Open task #`+strconv.FormatInt(it.taskID, 10)+`</a>`)
	}
	if len(links) > 0 {
		b.WriteString(`<div class="` + ui.AsksCardFooterClass + `" data-asks-footer>` + strings.Join(links, "") + `</div>
`)
	}
	b.WriteString("</li>\n")
	return b.String()
}

// closedOutcomeHTML is a closed card's outcome block: its state, then the
// answer (answerLabel's words before it; a step-up card's answer is its state
// already), then — for a step-up card whose draft was rejected with one — the
// owner's own reason, read from the draft (escaped, every hidden code point a
// visible flag: stepUpHiddenHTML, as the Drafts tab's Archived view draws it).
func closedOutcomeHTML(c closedCard) string {
	a := c.a
	var b strings.Builder
	b.WriteString(`<div class="` + ui.AsksOutcomeClass + `" data-asks-outcome>`)
	// A step-up card's state IS its recorded answer: marked as the answer.
	answerAttr := ""
	if a.StepUp != nil && a.Status == attention.Answered && strings.TrimSpace(a.Answer) != "" {
		answerAttr = " data-attention-answer-text"
	}
	b.WriteString(`<p class="` + ui.AsksOutcomeStateClass + `" data-asks-outcome-state` + answerAttr + `>` + html.EscapeString(closedState(a)) + `</p>`)
	if a.Answer != "" && a.StepUp == nil {
		b.WriteString(`<div class="` + ui.AsksAnswerTextClass + `" data-attention-answer-text>` + answerLabel(a) + askTextHTML(a.Answer) + `</div>`)
	}
	if d := c.draft; d != nil && d.DecisionReason != nil && strings.TrimSpace(*d.DecisionReason) != "" {
		b.WriteString(`<p class="` + ui.AsksReasonClass + `" data-asks-reason>Your reason: ` + stepUpHiddenHTML(*d.DecisionReason) + `</p>`)
	}
	b.WriteString("</div>\n")
	return b.String()
}

// settledCardHTML is closed ask a drawn settled in its place on the Open feed
// (closedCardHTML keyed ask-<id>, as its open card was), as ctx's principal.
func (s *Server) settledCardHTML(ctx context.Context, a attention.Ask) string {
	asks, hosts := []attention.Ask{a}, s.liveHosts(ctx)
	cards := s.closedCards(ctx, asks, s.closedItems(ctx, asks, hosts), hosts)
	return closedCardHTML(cards[0], attentionAskKey(a.ID), closedSettled, s.now())
}

// writeSettledCard answers a card form's htmx post (asksCardRequest) that
// closed a: the settled card, which the feed's script puts in the card's
// place.
func (s *Server) writeSettledCard(w http.ResponseWriter, ctx context.Context, a attention.Ask) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s.settledCardHTML(ctx, a)))
}

// settledFeedItem is the ask r names in ?settled= as an Open feed item drawn
// settled (settledHTML), over hosts (the render's snapshot), ok false for
// none: an ask the reader cannot read (row-level security, and readerMayRead
// again with it rolled back) or that is not there, a question to the chief
// (no page draws it open), or one still open (its open card is on the feed)
// — every one of them alike.
func (s *Server) settledFeedItem(r *http.Request, hosts func() []mirror.HostView) (attentionItem, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get(asksSettledParam), 10, 64)
	if err != nil || id <= 0 || s.ext.Attention == nil {
		return attentionItem{}, false
	}
	ctx := r.Context()
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.ToChief || a.Status == attention.Open || !readerMayRead(ctx, a.Owner) {
		return attentionItem{}, false
	}
	asks := []attention.Ask{a}
	items := s.closedItems(ctx, asks, hosts)
	c := s.closedCards(ctx, asks, items, hosts)[0]
	it := c.it
	it.settledHTML = closedCardHTML(c, attentionAskKey(a.ID), closedLanding, s.now())
	return it, true
}

// readerMayRead reports whether ctx's person may read an ask owner owns:
// their own, or a team's they are a member of (any role: row-level security's
// rule for attention_asks, 0127). The explicit check behind RLS.
func readerMayRead(ctx context.Context, owner int64) bool {
	viewer := requestOwner(ctx)
	if owner <= 0 || viewer <= 0 {
		return false
	}
	if owner == viewer {
		return true
	}
	p, ok := authz.From(ctx)
	if !ok {
		return false
	}
	_, in := p.TeamScope().RoleIn(owner)
	return in
}

// withoutSettled is items less a settled card (withSettled): what is still
// open, as the chips count it.
func withoutSettled(items []attentionItem) []attentionItem {
	out := make([]attentionItem, 0, len(items))
	for _, it := range items {
		if it.settledHTML == "" {
			out = append(out, it)
		}
	}
	return out
}

// withSettled is items with settled put where its open card stood: before
// the first item that started waiting before it (the feed is newest first
// within each kind: askFeedHTML).
func withSettled(items []attentionItem, settled attentionItem) []attentionItem {
	out := make([]attentionItem, 0, len(items)+1)
	placed := false
	for _, it := range items {
		if !placed && it.Kind == settled.Kind && it.Since.Before(settled.Since) {
			out = append(out, settled)
			placed = true
		}
		out = append(out, it)
	}
	if !placed {
		out = append(out, settled)
	}
	return out
}
