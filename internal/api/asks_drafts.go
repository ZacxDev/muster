package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASKS PAGE'S TABS: "Ask" (everything /attention listed before, its
// Answered section — now the Done tab — included) and, with plugins served, "Reply drafts" — the
// drafts that used to sit at the bottom of /plugins, decided here.
//
// 🔴 THE ROUTE STAYS /attention, AND THAT IS THE DESIGN, NOT AN OVERSIGHT. A
// URL fragment never reaches the server, so a redirect from /attention to a
// new path could not carry `#ask-12` along in any way the server can see — it
// survives a 30x only by browser convention, and a link that already lives on
// a phone (a push notification's tap target, an agent card's badge, the chief's
// attention_list) is exactly the link that must not break. Keeping the path
// and renaming the page (heading, title, sidebar) costs nothing and breaks no
// link. The tab is in the QUERY (?tab=drafts, ?tab=archived), so it IS seen by
// the server and the page is drawn on the right tab with no script; the
// fragment then lands on the card.
//
// A DRAFT LEAVES "Awaiting decision" THE MOMENT IT IS DECIDED: archived means
// "not pending" (nothing is stored for it — no migration), and the Archived
// view draws every other draft with its LIVE status (draftStatusText, read on
// every render and refreshed on the queue's event), keeping Withdraw while an
// approval is still unclaimed.
//
// THE BADGE STILL COUNTS PENDING DRAFTS: attentionItems is unchanged and still
// returns them; only the Ask tab's list leaves them out (askTabItems). The
// Reply drafts tab's label carries their count ("Reply drafts (2)").
//
// 🔴 APPROVE WHAT YOU SAW: the Awaiting decision list is NEVER re-drawn in
// place when its SET of drafts changed (asksPageScript). A newer draft for the
// same message supersedes the one on screen; re-drawn under the owner's thumb,
// "Approve and send" would approve text they never read. Instead the stale
// cards stay — approving one is refused (409, it is superseded) — and a line
// offers to show the latest. Same set (an expiry moving): re-drawn in place.

const (
	asksTabParam = "tab"
	// asksTabAsk is the Open tab's OLD name: ?tab=ask still opens it (asksTab).
	asksTabAsk      = "ask"
	asksTabDrafts   = "drafts"
	asksTabArchived = "archived"

	// asksDraftsPath is the drafts tab's live fragment (JSON: both lists and
	// the pending count), what the page re-fetches on the queue's event.
	asksDraftsPath = attentionPath + "/drafts"

	// asksDraftsRead is how many drafts one render reads: DraftCards orders the
	// pending ones first, so both lists are cut at pluginDraftsShown from it.
	asksDraftsRead = 2 * pluginDraftsShown
)

// draftLink is a draft card's address on the Asks page: the push deep link,
// the attention item's link and where a decision lands. A decided draft is in
// the Archived view; the page's script follows the fragment to whichever view
// holds the card, so ?tab=drafts is right for both.
func draftLink(id int64) string {
	return attentionPath + "?" + asksTabParam + "=" + asksTabDrafts + "#" + draftCardAnchor(id)
}

// draftDecidedLink is where an approve, reject or withdraw lands: the card,
// now in Archived — named in ?draft= too, so it is drawn even when it is not
// among the pluginDraftsShown most recent (readDraftViews).
func draftDecidedLink(id int64) string {
	return attentionPath + "?" + asksTabParam + "=" + asksTabArchived + "&" + asksDraftParam + "=" +
		strconv.FormatInt(id, 10) + "#" + draftCardAnchor(id)
}

// asksDraftParam names a draft the page must draw whatever the views' cut.
const asksDraftParam = "draft"

// asksFocus is the draft a request names in ?draft=, 0 for none.
func asksFocus(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.URL.Query().Get(asksDraftParam), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// asksTab is the tab a request names (asks_feed.go's mapping): drafts,
// archived, done, nudges and log as named; asksTabDone for "answered" (the
// Done tab's old name: a link already on a phone still lands there);
// asksTabOpen for none, for "ask" (the Open tab's old name), for "open" and
// for anything unknown.
func asksTab(r *http.Request) string {
	switch v := r.URL.Query().Get(asksTabParam); v {
	case asksTabDrafts, asksTabArchived, asksTabDone, asksTabNudges, asksTabLog:
		return v
	case asksTabAnswered:
		return asksTabDone
	}
	return asksTabOpen
}

// askTabItems is items without what the Drafts tab lists (draftsTabItem).
func askTabItems(items []attentionItem) []attentionItem {
	out := make([]attentionItem, 0, len(items))
	for _, it := range items {
		if !draftsTabItem(it) {
			out = append(out, it)
		}
	}
	return out
}

// draftsTabItem reports whether item it is one the Drafts tab LISTS: a reply
// or outbound draft (draftKey), an outreach batch (batchKey). 🔴 NOT every
// item of the plugin kind: a plugin's pairing request (pairKey) is that kind
// too, and the Drafts tab does not list it — counted as a draft it drew "1
// draft waits" over a tab that did not show it (audit of #787). It is the Open
// feed's own card.
func draftsTabItem(it attentionItem) bool {
	return it.Kind == attentionKindPlugin && (strings.HasPrefix(it.Key, "draft-") || strings.HasPrefix(it.Key, "outreach-"))
}

// askGroups is the Open feed's ORDER (askFeedHTML): each group's key, its
// label (the old group header; a card's badge now says it, askFeedBadge), and
// the item kinds it holds. A draft or outreach batch (attentionKindPlugin) is
// the Drafts tab's (askTabItems strips it first); the plugin kind's group here
// holds what is left of it, a pairing request.
// TestEveryAttentionKindHasAHome pins that every other kind the queue can hold
// is in one.
var askGroups = []struct {
	key, label string
	kinds      []string
}{
	{"questions", "Questions", []string{attentionKindAsk, attentionKindQuestion}},
	{"notices", "Notices", []string{attentionKindNotice}},
	{"waiting", "Waiting", []string{attentionKindSession}},
	{"tasks", "Tasks to route", []string{attentionKindTask}},
	{"pairing", "Pairing requests", []string{attentionKindPlugin}},
}

// splitDraftCards is cards as the two views: awaiting a decision (pending and
// not yet past its expiry) and archived (every other), each at most
// pluginDraftsShown.
func splitDraftCards(cards []plugins.DraftCard, now time.Time) (pending, archived []plugins.DraftCard) {
	for _, c := range cards {
		if c.Draft.Status == plugins.DraftPending && c.Draft.ExpiresAt.After(now) {
			// A pending follow or like is not drawn from this list: its whole
			// batch is (withBatches), so a cut can never draw one short.
			if c.Draft.IsOutbound() && plugins.IsBatchKind(c.Draft.Target().Kind) {
				continue
			}
			if len(pending) < pluginDraftsShown {
				pending = append(pending, c)
			}
			continue
		}
		if len(archived) < pluginDraftsShown {
			archived = append(archived, c)
		}
	}
	return pending, archived
}

// draftListHTML is one view's <li>s, or its empty line, in the zero place
// (no microphone: a page draws its cards with draftListHTMLIn).
func draftListHTML(cards []plugins.DraftCard, empty string) string {
	return draftListHTMLIn(cards, empty, cardPlace{})
}

// draftListHTMLIn is draftListHTML drawn in place p: each card is
// draftCardHTMLIn's, each batch outreachBatchCardHTMLIn's.
func draftListHTMLIn(cards []plugins.DraftCard, empty string, p cardPlace) string {
	if len(cards) == 0 {
		return ui.AsksEmptyHTML(` data-asks-empty="drafts-list" data-drafts-empty`, "", "", empty, "")
	}
	// A pending follow or like is an entry of its outreach batch's ONE card,
	// drawn where the batch's first entry falls (outreach_batch.go).
	_, batches, at := groupBatches(cards)
	var b strings.Builder
	for _, c := range cards {
		d := c.Draft
		if d.Status == plugins.DraftPending && d.IsOutbound() && plugins.IsBatchKind(d.Target().Kind) && d.Officer != nil {
			if i, first := at[d.ID]; first {
				b.WriteString(outreachBatchCardHTMLIn(batches[i], p))
			}
			continue
		}
		b.WriteString(draftCardHTMLIn(c, time.Now(), p))
	}
	return b.String()
}

const (
	draftsPendingEmpty  = "No reply drafts await your decision."
	draftsArchivedEmpty = "No decided drafts yet."
)

// draftsTabLabel is the Drafts tab's label: its pending count when there is
// one (the page's script keeps it current).
func draftsTabLabel(n int) string {
	if n <= 0 {
		return "Drafts"
	}
	return "Drafts (" + strconv.Itoa(n) + ")"
}

// readDraftViews is ctx's principal's drafts as the two views; ok false when
// they could not be read (logged). focus, when not 0, is a draft that must be
// drawn (a decision lands on it): when the views' cut left it out it is read
// on its own and put first in its view. Another owner's, or none, is not drawn.
func (s *Server) readDraftViews(ctx context.Context, focus int64) (pending, archived []plugins.DraftCard, ok bool) {
	cards, err := s.ext.Plugins.DraftCards(ctx, asksDraftsRead)
	if err != nil {
		s.logger.Printf("asks: reply drafts: %v", err)
		return nil, nil, false
	}
	now := time.Now()
	s.withDraftCardReads(ctx, cards)
	pending, archived = splitDraftCards(cards, now)
	pending = s.withBatches(ctx, pending)
	if focus <= 0 {
		return pending, archived, true
	}
	for _, c := range append(append([]plugins.DraftCard{}, pending...), archived...) {
		if c.Draft.ID == focus {
			return pending, archived, true
		}
	}
	c, err := s.ext.Plugins.DraftCard(ctx, focus)
	if err != nil {
		if !errors.Is(err, plugins.ErrNotFound) {
			s.logger.Printf("asks: reply draft %d: %v", focus, err)
		}
		return pending, archived, true
	}
	one := []plugins.DraftCard{c}
	s.withDraftCardReads(ctx, one)
	c = one[0]
	if p, _ := splitDraftCards([]plugins.DraftCard{c}, now); len(p) == 1 {
		return append([]plugins.DraftCard{c}, pending...), archived, true
	}
	return pending, append([]plugins.DraftCard{c}, archived...), true
}

// draftConfirmAsksShown is how many confirm-ask chips a draft card draws;
// the rest are a "+N".
const draftConfirmAsksShown = 3

// withConfirmAsks sets each card's confirm asks (display only): the reader's
// OWN asks whose step-up card is bound to the draft, at most
// draftConfirmAsksShown, open first then newest, and their count — and the
// newest OPEN session request among them (draftRequestOf), which the card
// draws in place of its own Open-feed card. ONE read for every card
// (attention.Store.ConfirmAsksForDrafts), never one per card. A read that
// fails draws each card with its draft id alone.
func (s *Server) withConfirmAsks(ctx context.Context, cards []plugins.DraftCard) {
	if s.ext.Attention == nil || len(cards) == 0 {
		return
	}
	ids := make([]int64, 0, len(cards))
	for _, c := range cards {
		ids = append(ids, c.Draft.ID)
	}
	got, err := s.ext.Attention.ConfirmAsksForDrafts(ctx, ids, draftConfirmAsksShown)
	if err != nil {
		s.logger.Printf("asks: drafts' confirm asks: %v", err)
		return
	}
	for i := range cards {
		c, ok := got[cards[i].Draft.ID]
		if !ok {
			continue
		}
		cards[i].ConfirmAsks = c.Total
		for _, a := range c.Asks {
			cards[i].ConfirmAskIDs = append(cards[i].ConfirmAskIDs, a.ID)
			if cards[i].Request == nil && cards[i].Draft.Status == plugins.DraftPending {
				cards[i].Request = draftRequestOf(a)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// ONE DRAFT, ONE CARD. A session that calls plugin_draft_decide on the
// owner's behalf files a step-up card (host_stepup_card.go) that only the
// owner's paired phone may answer. The owner saw that card on the Open feed
// AND the draft on the Drafts tab ("it feels redundant to have a draft and a
// confirm ask, i should just approve the draft directly").
//
// So the request is drawn AS ITS DRAFT'S OWN CARD (draftCardHTMLIn): the
// draft's preview and its web decision, a line naming what the session
// asked (draftRequestHTML) and the chips of its confirm asks. On the Drafts
// tab that is the draft's card; on the Open feed it is drawn in the
// request's place (askMergedDraftCardHTML) — the Open feed keeps a way to
// decide it ("i was on the main asks page, i could do it from the drafts
// page, but thats confusing"). Several open requests for one draft are ONE
// Open card, the newest's, carrying every chip (foldDraftRequests). THE ONE
// OPEN COUNT counts the draft once: a request is not counted beside its
// draft's own item in the same list (uncountedRequests).
//
// The owner's decision there is the draft's own web decision (decideDraftAs),
// which settles every open step-up card about the draft and tells the session
// that asked (settleDraftStepUps, wakeStepUpsSettledBy). 🔴 NOTHING ABOUT THE
// GATE CHANGES: the card is still answered only from the phone
// (hostDecisionForAnswer), and a session still cannot decide the draft
// without that signature. 🔴 NO REQUEST IS HIDDEN: a request is folded only
// into another request for the same draft, which is drawn; one whose draft
// the reader cannot decide is drawn as its step-up card, as before.
//
// A card that no longer matches its draft (StepUpCard.Changed: "Refuse") is
// not a request to decide it, and stays its own step-up card.
// ---------------------------------------------------------------------------

// stepUpRequestsDraft reports whether step-up card c is a session's request
// to decide its draft that still matches the draft: THE one rule of which
// cards the draft's own card draws.
func stepUpRequestsDraft(c *attention.StepUpCard) bool {
	return c != nil && c.Tool == pluginDraftDecideTool && c.DraftID > 0 && !c.Changed
}

// draftRequestOf is ask a as its draft's request, nil when it is not an open
// one (stepUpRequestsDraft, isStepUpCard).
func draftRequestOf(a attention.Ask) *plugins.DraftRequest {
	// An open ask past its expiry that no sweep has moved yet is closed, as
	// the store reads it (attention cols).
	if a.Status != attention.Open || !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(time.Now()) || !isStepUpCard(a) {
		return nil
	}
	return draftRequestFor(a.ID, a.StepUp)
}

// draftRequestFor is step-up card c of ask id as its draft's request, nil
// for a card that is not one (stepUpRequestsDraft).
func draftRequestFor(id int64, c *attention.StepUpCard) *plugins.DraftRequest {
	if !stepUpRequestsDraft(c) {
		return nil
	}
	r := &plugins.DraftRequest{AskID: id, Asker: strings.TrimSpace(c.Asker), Reject: stepUpRejects(c)}
	if r.Reject {
		why := strings.TrimPrefix(c.Why, stepUpRejectWhy)
		if why != c.Why {
			r.Reason = strings.TrimSpace(strings.TrimPrefix(why, ":"))
		}
	}
	return r
}

// draftRequestItem reports whether item it is a session's open request to
// decide a draft (stepUpRequestsDraft over a card muster filed).
func draftRequestItem(it attentionItem) bool {
	return it.Kind == attentionKindAsk && it.systemAsk && stepUpRequestsDraft(it.stepUp)
}

// foldDraftRequests is items with every draft's open requests folded into
// ONE item, the newest (the one the draft's card names: withConfirmAsks
// reads open first, newest first), which carries the others (folded) — so a
// feed that cannot draw that item as the draft's card (askMergedDraftCardHTML
// "": the draft unreadable to the reader, or no longer pending) draws every
// one of them as it was (askFeedCardHTML). Every other item is as it was, in
// its place.
func foldDraftRequests(items []attentionItem) []attentionItem {
	newest := map[int64]int64{}
	for _, it := range items {
		if draftRequestItem(it) && it.AskID > newest[it.stepUp.DraftID] {
			newest[it.stepUp.DraftID] = it.AskID
		}
	}
	if len(newest) == 0 {
		return items
	}
	folded := map[int64][]attentionItem{}
	for _, it := range items {
		if draftRequestItem(it) && it.AskID != newest[it.stepUp.DraftID] {
			folded[it.stepUp.DraftID] = append(folded[it.stepUp.DraftID], it)
		}
	}
	out := items[:0:0]
	for _, it := range items {
		if draftRequestItem(it) {
			if it.AskID != newest[it.stepUp.DraftID] {
				continue
			}
			it.folded = folded[it.stepUp.DraftID]
		}
		out = append(out, it)
	}
	return out
}

// uncountedRequests is items without each request whose draft's own item is
// in items too: THE ONE OPEN COUNT counts that draft once. Over the list
// being counted, so a filter that keeps the request and not its draft
// counts the request.
func uncountedRequests(items []attentionItem) []attentionItem {
	drafts := map[string]bool{}
	for _, it := range items {
		if it.Kind == attentionKindPlugin && strings.HasPrefix(it.Key, "draft-") {
			drafts[it.Key] = true
		}
	}
	if len(drafts) == 0 {
		return items
	}
	out := items[:0:0]
	for _, it := range items {
		if draftRequestItem(it) && drafts[draftKey(it.stepUp.DraftID)] {
			continue
		}
		out = append(out, it)
	}
	return out
}

// askMergedDraftCardHTML is request item it's card on the Open feed: its
// draft's own card (draftCardHTMLIn), keyed as the request, its forms
// returning here and carrying the request (cardPlace.card: a decision lands
// on it settled in place), naming it as the request. "" when it is not a
// request whose draft the reader may decide now (the feed then draws its
// step-up card).
func askMergedDraftCardHTML(it attentionItem, now time.Time) string {
	if !draftRequestItem(it) || it.stepUpDraft == nil || it.stepUpDraft.Draft.Status != plugins.DraftPending {
		return ""
	}
	c := *it.stepUpDraft
	c.Request = draftRequestFor(it.AskID, it.stepUp)
	if !slices.Contains(c.ConfirmAskIDs, it.AskID) && len(c.ConfirmAskIDs) < draftConfirmAsksShown {
		c.ConfirmAskIDs = append([]int64{it.AskID}, c.ConfirmAskIDs...)
		c.ConfirmAsks = max(c.ConfirmAsks, len(c.ConfirmAskIDs))
	}
	place := cardPlace{back: attentionPath, card: it.AskID, dictate: it.dictate, dictateOff: it.dictateOff, feedKey: it.Key}
	place.requestContext, place.requestFooter = askRequestPartsHTML(it, now)
	return draftCardHTMLIn(c, now, place)
}

// The words of a draft card's request line (draftRequestHTML).
const (
	draftRequestSession = "A session"
	draftRequestApprove = " asked to approve and send this."
	draftRequestReject  = " asked to reject this"
	draftRequestDecide  = " Decide it here, or confirm on your phone."
)

// draftRequestHTML is a draft card's request line: who asked (the session's
// name, flagged as untrusted text is: stepUpHiddenHTML), what, and the
// reason a rejection gives. "" for a card with none.
func draftRequestHTML(c plugins.DraftCard) string {
	r := c.Request
	if r == nil {
		return ""
	}
	who := html.EscapeString(draftRequestSession)
	if r.Asker != "" {
		who = stepUpHiddenHTML(r.Asker)
	}
	what := html.EscapeString(draftRequestApprove)
	if r.Reject {
		what = html.EscapeString(draftRequestReject)
		if r.Reason != "" {
			what += ": " + stepUpHiddenHTML(r.Reason)
		}
		what += "."
	}
	return `<p class="break-words text-sm text-fg2" data-draft-request data-ask-id="` + strconv.FormatInt(r.AskID, 10) + `">` + who + what +
		html.EscapeString(draftRequestDecide) + `</p>`
}

// draftConfirmAsksHTML is a draft card's confirm asks, beside its id chip:
// each a link chip to its ask's page ("Ask #940"), then "+N" for those not
// drawn. "" for a draft with none.
func draftConfirmAsksHTML(c plugins.DraftCard) string {
	if len(c.ConfirmAskIDs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="-mt-2 flex flex-wrap items-center justify-end gap-x-1" role="group" aria-label="Asks confirming this draft" data-draft-asks>`)
	for _, id := range c.ConfirmAskIDs {
		b.WriteString(ui.AskIDLinkChipHTML(id, askPagePath(id)))
	}
	b.WriteString(ui.IDMoreHTML(c.ConfirmAsks - len(c.ConfirmAskIDs)))
	b.WriteString(`</div>`)
	return b.String()
}

// withDraftCardReads is every read a draft's card is drawn from beyond its
// row, for cards together — each a batch over all of them, never a read per
// card: who drafted it (withDraftAgentNames, withDraftOfficerNames), whether
// a retry is open (withRetryOpen), where a decision went (withWakeLines) and
// its confirm asks (withConfirmAsks). 🔴 ONE READER FOR EVERY FACE: the Drafts
// tab's cards (readDraftViews) and a request's draft on the Open feed and its
// page (stepUpDraftsFor) both read through it, so the same draft draws the
// same head, wake line and chips wherever it is drawn.
func (s *Server) withDraftCardReads(ctx context.Context, cards []plugins.DraftCard) {
	s.withDraftAgentNames(ctx, cards)
	s.withDraftOfficerNames(cards)
	s.withRetryOpen(ctx, cards)
	s.withWakeLines(ctx, cards)
	s.withConfirmAsks(ctx, cards)
}

// withDraftAgentNames sets each card's AgentDisplay: the drafting agent's
// session name by the one naming rule (subjectName over one focus index read,
// what the sidebar names that agent), once per agent. A card whose agent is
// gone, or a read that fails, keeps drawing its stored AgentName.
func (s *Server) withDraftAgentNames(ctx context.Context, cards []plugins.DraftCard) {
	if s.ext.Agents == nil || len(cards) == 0 {
		return
	}
	fi := s.focusIndex(ctx)
	names := map[int64]string{}
	for i := range cards {
		a := cards[i].Draft.AgentID
		if a == nil || *a <= 0 {
			continue
		}
		n, ok := names[*a]
		if !ok {
			n = s.subjectName(ctx, fi, focus.AgentSubject(*a))
			names[*a] = n
		}
		cards[i].AgentDisplay = n
	}
}

// draftIDs is cards' ids, comma-joined in order: the Awaiting decision list's
// identity, which the page compares before re-drawing it (asksPageScript).
func draftIDs(cards []plugins.DraftCard) string {
	ids := make([]string, len(cards))
	for i, c := range cards {
		ids[i] = strconv.FormatInt(c.Draft.ID, 10)
	}
	return strings.Join(ids, ",")
}

// draftsPanel is the Drafts tab's panel: ONE list — the drafts awaiting a
// decision — and under it a "Show archived" toggle over the decided ones
// (round 0 of #768: no second tab strip inside the swipe area). sub
// asksTabArchived (?tab=archived, where a decision lands: draftDecidedLink)
// draws it with the archived list shown; the page's script keeps ?tab= in
// step with the toggle and opens it for a #draft-<id> inside it.
//
// p is the place the cards are drawn in (the Asks page's, its microphone
// set: Server.withDictation).
func draftsPanel(pending, archived []plugins.DraftCard, sub string, p cardPlace) g.Node {
	// No intro prose (the design pass): each pending card's reply box says
	// what approving sends, and an approved card's Withdraw is its own offer.
	pendingList := `<p id="asks-drafts-changed" role="status" class="flex flex-wrap items-center gap-2 rounded-md bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line" data-testid="drafts-changed" hidden>
<span>Drafts changed since you opened this list. The cards below are as you saw them.</span>
<button type="button" class="` + ui.AsksSecondaryButtonClass + `" data-drafts-show-latest>Show the latest</button></p>
<ul id="asks-drafts-pending" class="flex flex-col gap-2" data-testid="drafts-pending" data-draft-ids="` + draftIDs(pending) + `">` +
		draftListHTMLIn(pending, draftsPendingEmpty, p) + `</ul>`
	open := sub == asksTabArchived
	expanded, hidden, label := "false", " hidden", draftsShowArchived
	if open {
		expanded, hidden, label = "true", "", draftsHideArchived
	}
	toggle := `<button type="button" id="asks-drafts-archived-toggle" class="` + ui.AsksSecondaryButtonClass + ` self-start" aria-expanded="` + expanded +
		`" aria-controls="asks-drafts-archived-section" data-drafts-show-archived>` + label + `</button>`
	archivedSection := `<section id="asks-drafts-archived-section" class="flex flex-col gap-2" aria-labelledby="asks-drafts-archived-label" data-drafts-archived-section` + hidden + `>` +
		ui.AsksGroupLabel("asks-drafts-archived-label", "Archived", len(archived)) +
		`<ul id="asks-drafts-archived" class="flex flex-col gap-2" data-testid="drafts-archived">` + draftListHTMLIn(archived, draftsArchivedEmpty, p) + `</ul></section>`
	return g.Raw(`<div class="flex flex-col gap-2 pt-3">` + pendingList + toggle + archivedSection + `</div>`)
}

// The Drafts tab's archived toggle, shut and open.
const (
	draftsShowArchived = "Show archived"
	draftsHideArchived = "Hide archived"
)

// asksTabsHTML is the page's tabs (asks_feed.go): Open (askList, its feed's
// <ul>), Done (answered, its feed's <ul>: asks_done.go), Drafts when drafts (plugins are
// served), Nudges when nudges is not "" (its panel: the feature is on), and
// always Log (logPanel: the ask log's panel, or its placeholder —
// ask_log_read.go), the tab the request named selected (asksTab) — Open when
// that tab is not drawn. The panels sit in the swipe area (data-asks-swipe,
// the shared tab swipe's area over #asks-tabs): a sideways swipe there moves
// one tab (ui/tab_swipe.go).
func (s *Server) asksTabsHTML(askList, answered string, drafts bool, pending, archived []plugins.DraftCard, nudges, logPanel, tab string) string {
	top := asksTabOpen
	switch {
	case tab == asksTabDone, tab == asksTabLog && logPanel != "":
		top = tab
	case tab == asksTabNudges && nudges != "":
		top = asksTabNudges
	case (tab == asksTabDrafts || tab == asksTabArchived) && drafts:
		top = asksTabDrafts
	}
	panel := func(inner string) g.Node { return g.Raw(`<div class="flex flex-col gap-2 pt-2">` + inner + `</div>`) }
	tabs := []ui.Tab{
		{ID: asksTabOpen, Label: "Open", Panel: panel(askList)},
		{ID: asksTabDone, Label: "Done", Panel: panel(answered)},
	}
	if drafts {
		tabs = append(tabs, ui.Tab{ID: asksTabDrafts, Label: draftsTabLabel(len(pending)), Panel: draftsPanel(pending, archived, tab, s.withDictation(cardPlace{}))})
	}
	if nudges != "" {
		tabs = append(tabs, ui.Tab{ID: asksTabNudges, Label: "Nudges",
			Panel: g.Raw(`<div class="flex flex-col gap-2 pt-3" data-testid="nudges-panel">` + nudges + `</div>`)})
	}
	// The Log tab only with the ask log on (logPanel ""): off, it was a tab of
	// setup prose ("set MUSTER_ASK_LOG=1 and restart"), which is the
	// operator's docs' to say (docs/asks.md), not a tab's.
	if logPanel != "" {
		tabs = append(tabs, ui.Tab{ID: asksTabLog, Label: "Log", Panel: g.Raw(`<div class="flex flex-col gap-2 pt-3" data-testid="log-panel">` + logPanel + `</div>`)})
	}
	t := ui.TabsSelected("asks-tabs", "", "Asks", top, tabs...)
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col" data-asks-tabs><div class="flex overflow-x-auto border-b border-line">`)
	nodes := g.Group{t.List, g.Raw(`</div><div class="` + ui.AsksSwipeAreaClass + `" data-asks-swipe ` + ui.TabSwipeAttr + `="asks-tabs">`), t.Panels, g.Raw(`</div></div>`), ui.TabsScript()}
	if err := nodes.Render(&b); err != nil {
		s.logger.Printf("asks: render the tabs: %v", err)
	}
	return b.String()
}

// handleAsksDrafts is GET /attention/drafts[?draft=N]: the two lists, the
// pending list's ids and the tab's label as JSON, for the page's live refresh.
func (s *Server) handleAsksDrafts(w http.ResponseWriter, r *http.Request) {
	if s.ext.Plugins == nil {
		http.NotFound(w, r)
		return
	}
	pending, archived, ok := s.readDraftViews(r.Context(), asksFocus(r))
	if !ok {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the reply drafts could not be read"})
		return
	}
	// The page's filter (asks_filter.go): the same lists it drew.
	if flt := s.requestAsksFilter(r, nil); flt.on() {
		x := s.readAskSources(r.Context(), append(append([]plugins.DraftCard{}, pending...), archived...))
		pending, archived = flt.draftCards(x, pending), flt.draftCards(x, archived)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"pending":     draftListHTMLIn(pending, draftsPendingEmpty, s.withDictation(cardPlace{})),
		"pending_ids": draftIDs(pending),
		"archived":    draftListHTMLIn(archived, draftsArchivedEmpty, s.withDictation(cardPlace{})),
		"label":       draftsTabLabel(len(pending)),
	})
}

// asksPageScript is the page's own behaviour around the tabs (the cards' —
// expand, answer and dismiss, keys, swipe — is ui/asks_card.go's):
//
//   - the tab is kept in the URL (?tab=), replaced not pushed, so a reload or
//     a shared link opens the same tab. The PATH is always /attention: a
//     refusal drawn in answer to a decision's POST (/plugins/drafts/N/approve)
//     must not leave a POST-only address behind for a reload to 405 on. A tab
//     CHANGE also drops the fragment and lands at the top of the page;
//   - the live lists re-read with the address's filter (?type=, ?from=:
//     asks_filter.go), filterQuery;
//   - a fragment naming a card inside a hidden panel opens that panel's tabs
//     and scrolls to it (a push opened on ?tab=drafts for a draft that was
//     decided meanwhile lands on it in Archived);
//   - the drafts lists follow the queue's event (asks:changed, dispatched by
//     attentionPageScript, whose 60 s backstop fires it too) — never while
//     the confirm dialog is open, so the form it will submit is still in the
//     page. 🔴 Awaiting decision is re-drawn in place ONLY when its set of
//     drafts is unchanged (data-draft-ids); otherwise the cards on screen stay
//     and #asks-drafts-changed offers "Show the latest" (see the file header).
//     Archived is re-drawn, less any card still drawn in Awaiting decision (an
//     id names one element).
const asksPageScript = `<script>
(function () {
  var root = document.querySelector('[data-asks-tabs]');
  if (!root) return;
  // filterQuery: the address's filter (?type=, ?from=: asks_filter.go) as a
  // query to add after sep, "" for none — what every live list re-reads with.
  function filterQuery(sep) {
    var out = [];
    try {
      var u = new URL(window.location.href);
      ['` + asksTypeParam + `', '` + askerFromParam + `'].forEach(function (k) { var v = u.searchParams.get(k); if (v) out.push(k + '=' + encodeURIComponent(v)); });
    } catch (e) {}
    return out.length ? sep + out.join('&') : '';
  }
  function selected(id) {
    var list = document.getElementById(id);
    var t = list && list.querySelector('[role="tab"][aria-selected="true"]');
    return t ? t.getAttribute('data-tab') : '';
  }
  // shown: the tab on screen, so syncURL can tell a tab CHANGE from a
  // toggle inside one.
  var shown = selected('asks-tabs');
  function syncURL() {
    var arch = document.getElementById('asks-drafts-archived-section');
    var top = selected('asks-tabs'), v = top === '` + asksTabOpen + `' ? '' : (top === '` + asksTabNudges + `' || top === '` + asksTabLog + `' || top === '` + asksTabDone + `' ? top : (arch && !arch.hidden ? '` + asksTabArchived + `' : '` + asksTabDrafts + `'));
    var moved = top !== shown;
    shown = top;
    try {
      var u = new URL(window.location.href);
      u.pathname = '` + attentionPath + `';
      if (v) u.searchParams.set('` + asksTabParam + `', v); else u.searchParams.delete('` + asksTabParam + `');
      // The Done tab's page (?before=) is that tab's: another tab drops it.
      if (top !== '` + asksTabDone + `') u.searchParams.delete('` + asksBeforeParam + `');
      // 🔴 A TAB CHANGE DROPS THE FRAGMENT: it named a card of the tab left
      // (#draft-12, where a decision landed), and kept it re-opened THAT tab
      // and scrolled to that card on the next reload, whatever tab was chosen.
      if (moved) u.hash = '';
      if (u.href !== window.location.href) history.replaceState(history.state, '', u.href);
    } catch (e) {}
    // 🔴 A NEW TAB OPENS AT ITS TOP: the page kept the offset the last tab was
    // scrolled to, so a long tab opened part-way down its list.
    if (moved && window.scrollY > 0) window.scrollTo(0, 0);
    if (top === '` + asksTabLog + `') loadLog();
    if (window.muAsksChips) window.muAsksChips(v);
  }
  // The Log tab's panel is fetched the first time the tab is opened
  // (ask_log_read.go): the page's own render reads nothing from the log.
  function loadLog() {
    var lazy = root.querySelector('[data-asklog-lazy]');
    if (!lazy || lazy.hasAttribute('data-loading')) return;
    lazy.setAttribute('data-loading', '');
    // Drawn hidden (askLogLazyHTML): only a page that runs this says Loading….
    var shown = lazy.querySelector('[data-asklog-loading]');
    if (shown) shown.hidden = false;
    fetch(lazy.getAttribute('data-src'), { credentials: 'same-origin' }).then(function (r) {
      if (!r.ok) throw new Error(String(r.status));
      return r.text();
    }).then(function (h) {
      var t = document.createElement('template');
      t.innerHTML = h;
      lazy.replaceWith(t.content);
    }).catch(function () {
      lazy.removeAttribute('data-loading');
      var p = lazy.querySelector('[data-asklog-loading]');
      if (!p) return;
      // What failed, and a Retry: a link to the tab's own page (it works
      // without this script), which here loads the panel again.
      var a = document.createElement('a');
      a.className = 'underline';
      a.href = lazy.getAttribute('data-href') || '';
      a.textContent = 'Retry';
      a.setAttribute('data-asklog-retry', '');
      a.addEventListener('click', function (ev) {
        ev.preventDefault();
        p.textContent = "` + askLogLoadingText + `";
        loadLog();
      });
      p.textContent = "` + askLogFailedText + ` ";
      p.appendChild(a);
    });
  }
  if (window.location.pathname !== '` + attentionPath + `') syncURL();
  root.addEventListener('click', function (e) {
    if (e.target && e.target.closest && e.target.closest('[role="tab"]')) setTimeout(syncURL, 0);
  });
  root.addEventListener('keydown', function () { setTimeout(syncURL, 0); });
  // The Drafts tab's "Show archived" (draftsPanel): shows or hides the
  // decided drafts, and the address says which (?tab=archived / drafts).
  function showArchived(on) {
    var sec = document.getElementById('asks-drafts-archived-section'), b = document.getElementById('asks-drafts-archived-toggle');
    if (!sec || !b) return;
    sec.hidden = !on;
    b.setAttribute('aria-expanded', on ? 'true' : 'false');
    b.textContent = on ? '` + draftsHideArchived + `' : '` + draftsShowArchived + `';
  }
  root.addEventListener('click', function (e) {
    if (!e.target || !e.target.closest || !e.target.closest('[data-drafts-show-archived]')) return;
    var sec = document.getElementById('asks-drafts-archived-section');
    showArchived(!!(sec && sec.hidden));
    setTimeout(syncURL, 0);
  });
  function reveal() {
    var id = decodeURIComponent((window.location.hash || '').slice(1));
    var el = id && document.getElementById(id);
    if (!el || !root.contains(el) || !window.muTabs) return;
    if (el.closest('[data-drafts-archived-section]')) showArchived(true);
    for (var p = el.parentElement; p; p = p.parentElement) {
      if (p.getAttribute && p.getAttribute('role') === 'tabpanel' && p.hidden) {
        var tab = document.getElementById(p.getAttribute('aria-labelledby'));
        if (tab) window.muTabs.select(tab.closest('[data-tabs]'), tab, {});
      }
    }
    // The tab the jump opened is the tab on screen now (shown), as a tap
    // would make it: a later tap on the tab it left is a tab CHANGE (top of
    // the page, the fragment dropped). The address names the tab; the jump
    // keeps its fragment.
    shown = selected('asks-tabs');
    syncURL();
    el.scrollIntoView({ block: 'start' });
  }
  // A CARD A DECISION JUST CLOSED (?settled=, internal/api asks_done.go) is
  // drawn settled in its old spot: the page lands on it — with or without
  // its #ask-<id> (a boosted form's redirect loses the fragment) — and the
  // address drops ?settled=, so the next reload is the one that lets it go.
  (function () {
    try {
      var u = new URL(window.location.href);
      if (u.searchParams.has('` + asksSettledParam + `')) {
        u.searchParams.delete('` + asksSettledParam + `');
        history.replaceState(history.state, '', u.href);
      }
    } catch (e) {}
    var c = document.querySelector('#attention-items > [data-asks-landing]');
    if (!c) return;
    c.removeAttribute('data-asks-landing');
    if (window.location.hash) return;
    c.scrollIntoView({ block: 'center' });
    try { c.focus({ preventScroll: true }); } catch (e) {}
  })();
  reveal();
  window.addEventListener('hashchange', reveal);
  // The Done feed follows the queue's event too (it has no field to keep),
  // on the page the address names (?before=).
  var answered = document.getElementById('attention-answered');
  if (answered) document.addEventListener('asks:changed', function () {
    var before = '';
    try { var bv = new URL(window.location.href).searchParams.get('` + asksBeforeParam + `'); if (bv) before = '&` + asksBeforeParam + `=' + encodeURIComponent(bv); } catch (e) {}
    fetch('` + attentionPath + `/items?part=` + asksTabDone + `' + before + filterQuery('&'), { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.text() : null;
    }).then(function (h) {
      if (h === null) return;
      answered.innerHTML = h;
      // The card feed's script keeps expanded cards and focus (its reveal
      // handler serves every feed).
      try { answered.dispatchEvent(new Event(` + "'" + ui.DictateRevealEvent + "'" + `, { bubbles: true })); } catch (e) {}
    }).catch(function () {});
  });
  var pendingList = document.getElementById('asks-drafts-pending');
  var archivedList = document.getElementById('asks-drafts-archived');
  var changed = document.getElementById('asks-drafts-changed');
  var draftsTab = document.getElementById('asks-tabs-tab-drafts');
  if (!pendingList || !archivedList) return;
  // reasonBusy: the owner is writing a reason in list — a reason box focused
  // or holding text, or its mic recording (the answer boxes' rule,
  // ui.AttentionLiveScript's busy) — so a re-draw would eat it.
  function reasonBusy(list) {
    var a = document.activeElement;
    if (a && list.contains(a) && a.tagName === 'TEXTAREA') return true;
    if (list.querySelector('[data-dictate][data-state="opening"], [data-dictate][data-state="recording"], [data-dictate][data-state="busy"]')) return true;
    var fields = list.querySelectorAll('textarea');
    for (var i = 0; i < fields.length; i++) if (fields[i].value.trim() !== '') return true;
    return false;
  }
  // revealMics: the mics a re-draw drew (innerHTML: no htmx:load).
  function revealMics(list) { try { list.dispatchEvent(new Event('` + ui.DictateRevealEvent + `', { bubbles: true })); } catch (e) {} }
  function apply(j, latest) {
    if (latest || j.pending_ids === (pendingList.getAttribute('data-draft-ids') || '')) {
      // A reason being written holds the redraw; the banner stays until the
      // list on screen IS the latest (it says the cards are as first seen).
      if (!reasonBusy(pendingList)) {
        pendingList.innerHTML = j.pending;
        pendingList.setAttribute('data-draft-ids', j.pending_ids);
        revealMics(pendingList);
        if (changed) changed.hidden = true;
      } else if (changed && j.pending_ids !== (pendingList.getAttribute('data-draft-ids') || '')) {
        changed.hidden = false;
      }
    } else if (changed) {
      changed.hidden = false;
    }
    if (!reasonBusy(archivedList)) {
      var t = document.createElement('template');
      t.innerHTML = j.archived;
      Array.prototype.forEach.call(t.content.querySelectorAll('li[id]'), function (li) {
        if (pendingList.querySelector('[id="' + li.id + '"]')) li.remove();
      });
      archivedList.replaceChildren(t.content);
      revealMics(archivedList);
    }
    if (draftsTab) draftsTab.textContent = j.label;
  }
  function load(latest) {
    var dlg = document.getElementById('term-confirm');
    if (dlg && dlg.open) return;
    var q = '';
    try { var d = new URL(window.location.href).searchParams.get('` + asksDraftParam + `'); if (d) q = '?` + asksDraftParam + `=' + encodeURIComponent(d); } catch (e) {}
    q += filterQuery(q ? '&' : '?');
    fetch('` + asksDraftsPath + `' + q, { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.json() : null;
    }).then(function (j) {
      if (!j || (dlg && dlg.open)) return;
      apply(j, latest);
    }).catch(function () {});
  }
  document.addEventListener('asks:changed', function () { load(false); });
  if (changed) changed.addEventListener('click', function (e) {
    if (e.target && e.target.closest && e.target.closest('[data-drafts-show-latest]')) load(true);
  });
})();
</script>`

// stepUpSessionKey is the session that raised step-up card a (its host and
// session), "" for none: one wake line per session per decision.
func stepUpSessionKey(a attention.Ask) string {
	if a.StepUp == nil || a.StepUp.RaisedBySession == "" {
		return ""
	}
	return a.StepUp.RaisedByHost + "/" + a.StepUp.RaisedBySession
}

// dismissDraftRequestsWith closes, after the owner's Dismiss of request a, every
// other open request for a's draft — the requests its draft's Open card stood
// for (foldDraftRequests) — and tells each session that raised one, once, that
// it was dismissed (wakeDismissed). The draft itself is left pending.
func (s *Server) dismissDraftRequestsWith(ctx context.Context, a attention.Ask) {
	if s.ext.Attention == nil || a.Status != attention.Cancelled || !isStepUpCard(a) || !stepUpRequestsDraft(a.StepUp) {
		return
	}
	asks, err := s.ext.Attention.Search(ctx, attention.Search{DraftID: a.StepUp.DraftID, Limit: attention.MaxSearchResults})
	if err != nil {
		s.logger.Printf("asks: dismiss draft %d's requests: %v", a.StepUp.DraftID, err)
		return
	}
	told := map[string]bool{stepUpSessionKey(a): true}
	for _, b := range asks {
		if b.ID == a.ID || draftRequestOf(b) == nil || b.StepUp.DraftID != a.StepUp.DraftID {
			continue
		}
		c, err := s.cancelAsk(ctx, b.ID)
		if err != nil {
			continue
		}
		if k := stepUpSessionKey(c); k == "" || !told[k] {
			told[k] = true
			s.wakeDismissed(ctx, c)
		}
	}
}
