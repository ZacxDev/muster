package api

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// SEARCHING THE ASKS PAGE (operator decision: "search asks by ID as well as
// text content … make it searchable and also show it on the cards";
// docs/attention.md "Searching your asks").
//
// ONE BOX at the top of /attention. It searches the signed-in person's asks
// — open, answered, dismissed, expired, notices too — and plugin drafts, and
// lists what matches, each result linking to its ask's
// page (a draft: its card on the Drafts tab). The query is the page's ?q=,
// so a search is a link and Back works; an empty one is the page as before.
// It searches AS YOU TYPE (operator decision: debounced, no Search button):
// the box's own htmx request gets the results alone (writeAsksSearchLive),
// swapped in place; Enter is still the form's submit.
//
//   - AN ID ("859", "#859", "ask 859", "Ask #859", "draft 484") is looked up
//     by id ONLY — never as text, so the number in some question cannot get
//     between the owner and the ask an agent named. A bare number is either
//     kind's: the ask with that id and the draft with that id (and the
//     step-up cards that confirm it). "draft N" is the draft and its step-up
//     cards. When an id SUBMITTED (Enter, a link) finds exactly ONE thing,
//     the page goes straight to it (303, so the search is not a history
//     entry Back would bounce off). Typed, it is only listed.
//   - ANYTHING ELSE is text: a case-insensitive substring of the question,
//     the why line, an option, the answer, who asked, a step-up card's title,
//     why and post, and a draft's text. % _ and \ are themselves, never
//     wildcards (db.LikeEscape).
//   - TEXT IS SEARCHED IN EVERY ASK, WHATEVER ITS AGE (operator decision):
//     attention_asks keeps an ask's text as long as the ask, so an ask the
//     owner can still open is found by its words. The ask log's text TTL
//     (MUSTER_ASK_LOG_TEXT_TTL) is the separate log's, not search's.
//
// 🔴 WHAT EACH REACHES (operator decision). AN ID finds what the person can
// already OPEN where the search sends them: an ask the ask page opens (their
// own, never one put to the chief — handleAskPage's rule, which opens no team
// ask) and any draft the Drafts tab's card reads (row-level security, a
// team's draft its member reads included). Another owner's id finds nothing,
// drawn exactly as an id nobody has. TEXT is the owner's OWN, never a team's:
// both stores name the owner in the query (attention.Search,
// plugins.SearchDraftCards), since row-level security alone lets a team's
// member read the team's asks and drafts.
// ---------------------------------------------------------------------------

// asksSearchParam is the search's query parameter.
const asksSearchParam = "q"

// asksSearchShown is how many asks, and how many drafts, one search lists.
const asksSearchShown = 50

// askQuery is a typed search: the ids it names, or its text.
type askQuery struct {
	// AskID and DraftID are the ids an id-shaped query names (0: none). A
	// bare number names both.
	AskID, DraftID int64
	// Text is the words to find ("" for an id).
	Text string
}

// ByID reports whether q names an id (and is looked up by id only).
func (q askQuery) ByID() bool { return q.AskID > 0 || q.DraftID > 0 }

// askQueryID is an id-shaped query: an optional "ask" or "draft", an optional
// "#", then the number, spaces allowed between.
var askQueryID = regexp.MustCompile(`^(?i)(ask|draft)?\s*#?\s*([0-9]{1,18})$`)

// parseAskQuery is what the box was given, as a query.
func parseAskQuery(raw string) askQuery {
	raw = strings.TrimSpace(raw)
	if m := askQueryID.FindStringSubmatch(raw); m != nil {
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err == nil && n > 0 {
			switch strings.ToLower(m[1]) {
			case "ask":
				return askQuery{AskID: n}
			case "draft":
				return askQuery{DraftID: n}
			}
			return askQuery{AskID: n, DraftID: n}
		}
	}
	return askQuery{Text: truncateRunesTo(raw, attention.MaxSearchTextRunes)}
}

// truncateRunesTo is s cut to n runes (no ellipsis: a query, not a label).
func truncateRunesTo(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// asksSearchResult is what one search found: the asks and the drafts, newest
// first each, and whether either list was cut at asksSearchShown.
type asksSearchResult struct {
	asks      []attention.Ask
	drafts    []plugins.DraftCard
	truncated bool
	failed    bool
}

// count is how many things the result lists.
func (r asksSearchResult) count() int { return len(r.asks) + len(r.drafts) }

// searchAsks runs q as ctx's principal: its asks (the queue on) and its drafts
// (plugins served).
func (s *Server) searchAsks(ctx context.Context, q askQuery) asksSearchResult {
	var out asksSearchResult
	if s.ext.Attention != nil {
		asks, err := s.ext.Attention.Search(ctx, attention.Search{AskID: q.AskID, DraftID: q.DraftID, Text: q.Text, Limit: asksSearchShown + 1})
		if err != nil {
			s.logger.Printf("asks: search: %v", err)
			out.failed = true
		}
		if len(asks) > asksSearchShown {
			asks, out.truncated = asks[:asksSearchShown], true
		}
		out.asks = asks
	}
	// A draft is found by its own id or its text; "ask N" names no draft.
	if s.ext.Plugins != nil && (q.DraftID > 0 || q.Text != "") {
		cards, err := s.ext.Plugins.SearchDraftCards(ctx, q.DraftID, q.Text, asksSearchShown+1)
		if err != nil {
			s.logger.Printf("asks: search drafts: %v", err)
			out.failed = true
		}
		if len(cards) > asksSearchShown {
			cards, out.truncated = cards[:asksSearchShown], true
		}
		s.withDraftAgentNames(ctx, cards)
		s.withDraftOfficerNames(cards)
		out.drafts = cards
	}
	return out
}

// draftFocusLink is draft id's card on the Asks page, wherever it is: named
// in ?draft= (readDraftViews draws it whatever the views' cut), the page's
// script following the fragment to the view that holds it.
func draftFocusLink(id int64) string {
	return attentionPath + "?" + asksTabParam + "=" + asksTabDrafts + "&" + asksDraftParam + "=" + strconv.FormatInt(id, 10) + "#" + draftCardAnchor(id)
}

// asksSearchJump is where an id search that found exactly one thing goes: the
// ask's page, or the draft's card; "" for any other search.
func asksSearchJump(q askQuery, res asksSearchResult) string {
	if !q.ByID() || res.failed || res.count() != 1 {
		return ""
	}
	if len(res.asks) == 1 {
		return askPagePath(res.asks[0].ID)
	}
	return draftFocusLink(res.drafts[0].Draft.ID)
}

// The search's words.
const (
	asksSearchNoneText   = "No asks match"
	asksSearchClearText  = "Clear"
	asksSearchFailedText = "Part of the search could not be read."
)

// asksSearchCountText is the result count's words: "1 result", "12 results",
// and when a list was cut, that only the newest are listed.
func asksSearchCountText(n int, truncated bool) string {
	word := "results"
	if n == 1 {
		word = "result"
	}
	t := strconv.Itoa(n) + " " + word
	if truncated {
		t += " (the newest; narrow the search to see older ones)"
	}
	return t
}

// asksSearchHTML is the search's results, in the page where its tabs would be:
// the count and Clear, then one card per ask and draft, newest first — or the
// one "No asks match" line.
func asksSearchHTML(raw string, res asksSearchResult, now time.Time) string {
	esc := html.EscapeString
	clear := `<a class="` + ui.AsksSearchClearClass + `" href="` + attentionPath + `" data-asks-search-clear>` + asksSearchClearText + `</a>`
	var b strings.Builder
	b.WriteString(`<section class="flex min-w-0 flex-col gap-2" aria-label="Search results" data-testid="asks-search-results">
`)
	if res.failed {
		b.WriteString(problemNode(asksSearchFailedText))
	}
	if res.count() == 0 {
		b.WriteString(`<p class="` + ui.AsksSearchStatusClass + `" role="status" data-asks-search-empty><span>` + asksSearchNoneText + ` “` + esc(raw) + `”.</span> ` + clear + `</p>
</section>`)
		return b.String()
	}
	b.WriteString(`<p class="` + ui.AsksSearchStatusClass + `" role="status" data-asks-search-count="` + strconv.Itoa(res.count()) + `"><span>` +
		esc(asksSearchCountText(res.count(), res.truncated)) + `</span> ` + clear + `</p>
<ul class="` + ui.AsksFeedClass + `" data-testid="asks-search-list">`)
	// Newest first across both kinds.
	type row struct {
		at   time.Time
		html string
	}
	rows := make([]row, 0, res.count())
	for _, a := range res.asks {
		rows = append(rows, row{a.CreatedAt, askSearchCardHTML(a, now)})
	}
	for _, c := range res.drafts {
		rows = append(rows, row{c.Draft.CreatedAt, draftSearchCardHTML(c, now)})
	}
	slices.SortStableFunc(rows, func(x, y row) int { return y.at.Compare(x.at) })
	for _, r := range rows {
		b.WriteString(r.html)
	}
	b.WriteString(`</ul>
</section>`)
	return b.String()
}

// askSearchState is an ask's state in a result's sub line.
func askSearchState(a attention.Ask) string {
	state := "Open"
	if a.Status != attention.Open {
		state = askClosedState(a.Status)
	}
	if a.Kind == attention.KindNotice {
		return "Notice · " + state
	}
	return state
}

// askSearchCardHTML is one ask as a search result: its head — who asked (a
// step-up card: what it asks to do), its state and age — linking to its page,
// its id chip, its question and its answer.
func askSearchCardHTML(a attention.Ask, now time.Time) string {
	esc := html.EscapeString
	id := strconv.FormatInt(a.ID, 10)
	title, text := askerLabel(a), strings.TrimSpace(a.Question)
	if a.StepUp != nil {
		title = textplain.Title(stepUpTitleText(a.StepUp))
		text = strings.TrimSpace(firstNonBlank(a.StepUp.Preview, a.StepUp.Why))
	}
	var b strings.Builder
	b.WriteString(`<li id="search-ask-` + id + `" class="` + ui.AsksCardClass + `"` + ui.AsksFixedCardAttrs + ` tabindex="-1" data-asks-search-result="ask" data-ask-id="` + id +
		`" data-status="` + esc(string(a.Status)) + `">
<div class="` + ui.AsksSearchTopClass + `"><a href="` + esc(askPagePath(a.ID)) + `" class="` + ui.AsksSearchOpenClass + `" data-asks-search-open>` +
		`<span class="` + ui.AsksTitleClass + `" data-asks-search-title>` + esc(title) + `</span><span class="` + ui.AsksSubClass + `">` + esc(askSearchState(a)) +
		` · <time datetime="` + a.CreatedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(a.CreatedAt))) + `</time></span></a>` +
		ui.AskIDChipHTML(a.ID) + `</div>
`)
	if text != "" {
		b.WriteString(`<p class="` + ui.AsksSearchTextClass + `" data-asks-search-text>` + esc(text) + `</p>
`)
	}
	if ans := strings.TrimSpace(a.Answer); ans != "" {
		b.WriteString(`<p class="` + ui.AsksSearchAnswerClass + `" data-asks-search-answer>` + answerLabel(a) + esc(ans) + `</p>
`)
	}
	b.WriteString("</li>\n")
	return b.String()
}

// draftSearchCardHTML is one plugin draft as a search result: who drafted it,
// its kind, state and age, linking to its card; its id chip; its text as the
// post it becomes (the venue preview).
func draftSearchCardHTML(c plugins.DraftCard, now time.Time) string {
	esc := html.EscapeString
	d := c.Draft
	id := strconv.FormatInt(d.ID, 10)
	sub := draftKindLabel(d) + " · " + draftStatusText(d)
	var b strings.Builder
	b.WriteString(`<li id="search-draft-` + id + `" class="` + ui.AsksCardClass + `"` + ui.AsksFixedCardAttrs + ` tabindex="-1" data-asks-search-result="draft" data-draft-id="` + id +
		`" data-draft-status="` + esc(string(d.Status)) + `">
<div class="` + ui.AsksSearchTopClass + `"><a href="` + esc(draftFocusLink(d.ID)) + `" class="` + ui.AsksSearchOpenClass + `" data-asks-search-open>` +
		`<span class="` + ui.AsksTitleClass + `" data-asks-search-title>` + stepUpHiddenHTML(draftWho(c)) + `</span><span class="` + ui.AsksSubClass + `">` + esc(sub))
	if !d.CreatedAt.IsZero() {
		b.WriteString(` · <time datetime="` + d.CreatedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(d.CreatedAt))) + `</time>`)
	}
	b.WriteString(`</span></a>` + ui.DraftIDChipHTML(d.ID) + `</div>
`)
	// Its text as the Drafts tab's card draws it: the post it becomes on its
	// venue (stepUpPostHTML over draftPostCard), here the feed's compact one
	// — the first part clamped, media as thumbnails — every hidden code point
	// a visible flag and every look-alike word marked.
	b.WriteString(`<div class="min-w-0" data-asks-search-text>` + stepUpPostHTML(draftPostCard(c), true) + `</div>
`)
	b.WriteString("</li>\n")
	return b.String()
}

// asksSearchLive reports whether r is the search box's own live request (the
// field's debounced htmx GET, ui/asks_search.go): htmx names the element it
// will swap in HX-Target, and only the box targets the results region. A
// boosted navigation (HX-Request too, but no such target) is a navigation, and
// keeps the id redirect.
func asksSearchLive(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == ui.AsksSearchRegionID
}

// writeAsksSearchLive answers the live request: the results region's contents
// alone — empty for an empty box, which shows the page again — and the address
// the page should now show (HX-Replace-Url: typing adds no history entry).
//
// 🔴 NEVER THE ID REDIRECT (operator decision): typing never navigates away.
// An id is listed like any other result (typing 12 on the way to 123 must not
// open ask 12); only a real submit (Enter) goes straight to the one ask.
func (s *Server) writeAsksSearchLive(w http.ResponseWriter, r *http.Request) {
	raw, q, ok := asksSearchRequest(r)
	to := attentionPath
	if ok {
		to += "?" + asksSearchParam + "=" + url.QueryEscape(raw)
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("HX-Replace-Url", to)
	w.WriteHeader(http.StatusOK)
	if !ok {
		return
	}
	_, _ = io.WriteString(w, asksSearchHTML(raw, s.searchAsks(r.Context(), q), s.now()))
}

// asksSearchRequest is the page request's search, ok false for none (no ?q=,
// or a blank one: the page as it was).
func asksSearchRequest(r *http.Request) (raw string, q askQuery, ok bool) {
	raw = strings.TrimSpace(r.URL.Query().Get(asksSearchParam))
	if raw == "" {
		return "", askQuery{}, false
	}
	return raw, parseAskQuery(raw), true
}
