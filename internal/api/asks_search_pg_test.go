package api

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
)

// THE ASKS PAGE'S SEARCH (asks_search.go), end to end on a real Postgres: the
// box over the page, ?q= as an id (straight to the one ask it names) or as
// text (a list of results), the owner's asks only, and the id on every card.
//
// 🔴 EVERY EXPECTATION IS A LITERAL — the URLs, the data-* hooks, the words —
// and this file names no symbol the search added, so it compiles on the
// commit before it and is RED there by its assertions, not by a build error.

// searchGet is GET /attention?q=raw as the operator (no redirect followed).
func (f *attnFixture) searchGet(t *testing.T, raw string) (int, string, string) {
	t.Helper()
	w := f.call(t, http.MethodGet, "/attention?q="+url.QueryEscape(raw), nil, f.operator())
	return w.Code, w.Header().Get("Location"), w.Body.String()
}

// idChipRe is an id chip as the server draws it: its label, kind, id and text.
var idChipRe = regexp.MustCompile(`<span class="[^"]*" data-id-copy="([^"]*)" data-id-kind="(ask|draft)" data-id="(\d+)">([^<]*)</span>`)

// chipsIn is every id chip in h, as "label|kind|id|text".
func chipsIn(h string) []string {
	var out []string
	for _, m := range idChipRe.FindAllStringSubmatch(h, -1) {
		out = append(out, m[1]+"|"+m[2]+"|"+m[3]+"|"+m[4])
	}
	return out
}

// liByID is the <li id="id"> in page, to its </li> ("" for none).
func liByID(page, id string) string {
	i := strings.Index(page, `<li id="`+id+`"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(page[i:], "</li>")
	if j < 0 {
		return ""
	}
	return page[i : i+j+len("</li>")]
}

// searchResults is the results' ids in order, "ask:N" / "draft:N".
var searchResultRe = regexp.MustCompile(`data-asks-search-result="(ask|draft)" data-(?:ask|draft)-id="(\d+)"`)

func searchResults(page string) []string {
	var out []string
	for _, m := range searchResultRe.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1]+":"+m[2])
	}
	return out
}

// tagless is h's text: tags dropped, entities for quotes undone, whitespace
// folded.
func tagless(h string) string {
	h = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(h, " ")
	h = strings.NewReplacer("&#34;", `"`, "&#39;", "'", "&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(h)
	return strings.Join(strings.Fields(h), " ")
}

// TestAnIdSearchGoesStraightToTheAsk: every way an agent names an ask —
// "N", "#N", "ask N", "Ask #N", "ask#N" — answers 303 to that ask's page. A
// search is the page's ?q=, never a separate route. RED before: ?q= was
// ignored (200, the page as it was).
func TestAnIdSearchGoesStraightToTheAsk(t *testing.T) {
	f := newAttnFixture(t)
	a := f.mirrorAsk(t, "Ship the release build?")
	id := strconv.FormatInt(a.ID, 10)
	for _, q := range []string{id, "#" + id, "ask " + id, "Ask #" + id, " ask#" + id + " "} {
		code, loc, body := f.searchGet(t, q)
		if code != http.StatusSeeOther || loc != "/attention/asks/"+id {
			t.Errorf("?q=%q: %d %q, want 303 to /attention/asks/%s\n%.400s", q, code, loc, id, body)
		}
	}
}

// TestAnIdSearchFindsOnlyTheOwnersOwnAsk: another owner's ask, named by its
// id, is not found — no redirect, and the one "No asks match" line. The
// control: the same request for the operator's own ask goes straight to it.
func TestAnIdSearchFindsOnlyTheOwnersOwnAsk(t *testing.T) {
	f := newAttnFixture(t)
	mine := f.mirrorAsk(t, "Mine?")
	theirs, err := f.attn.Create(asUser(f.member), attention.New{Asker: attention.Asker{AgentID: f.chief.ID, Label: "chief"}, Question: "the shared heron word, theirs"})
	if err != nil {
		t.Fatal(err)
	}
	if code, loc, _ := f.searchGet(t, strconv.FormatInt(mine.ID, 10)); code != http.StatusSeeOther || loc != "/attention/asks/"+strconv.FormatInt(mine.ID, 10) {
		t.Fatalf("control: the operator's own ask: %d %q", code, loc)
	}
	for _, q := range []string{strconv.FormatInt(theirs.ID, 10), "heron"} {
		code, loc, body := f.searchGet(t, q)
		if code != http.StatusOK || loc != "" || len(searchResults(body)) != 0 || !strings.Contains(body, "data-asks-search-empty") {
			t.Errorf("?q=%q (another owner's ask): %d %q %v, want 200 and no result", q, code, loc, searchResults(body))
		}
	}
}

// TestATextSearchListsTheOwnersAsksNewestFirst: words are searched in every
// state — open, answered, dismissed — case folded, newest first, each result
// linking to its ask's page with its id chip; the count says how many. One
// match is still a LIST (only an id jumps). The page draws the results where
// its tabs were, under the box, which holds the query.
func TestATextSearchListsTheOwnersAsksNewestFirst(t *testing.T) {
	f := newAttnFixture(t)
	open := f.mirrorAsk(t, "Deploy the Zebra build?")
	answered := f.mirrorAsk(t, "Name the zebra release?")
	if _, err := f.attn.Answer(asUser(1), answered.ID, "Stripes"); err != nil {
		t.Fatal(err)
	}
	dismissed := f.mirrorAsk(t, "Retire ZEBRA-1?")
	if _, err := f.attn.Cancel(asUser(1), dismissed.ID); err != nil {
		t.Fatal(err)
	}
	f.mirrorAsk(t, "Something else entirely?")
	f.setCreated(t, "attention_asks", open.ID, time.Now().Add(-3*time.Minute))
	f.setCreated(t, "attention_asks", answered.ID, time.Now().Add(-2*time.Minute))
	f.setCreated(t, "attention_asks", dismissed.ID, time.Now().Add(-1*time.Minute))

	code, loc, page := f.searchGet(t, "zebra")
	if code != http.StatusOK || loc != "" {
		t.Fatalf("?q=zebra: %d %q", code, loc)
	}
	ids := func(as ...attention.Ask) []string {
		var out []string
		for _, a := range as {
			out = append(out, "ask:"+strconv.FormatInt(a.ID, 10))
		}
		return out
	}
	if got, want := searchResults(page), ids(dismissed, answered, open); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("results %v, want %v", got, want)
	}
	if !strings.Contains(page, `data-asks-search-count="3"><span>3 results</span> <a `) {
		t.Errorf("the count line: %s", htmlRegion(page, `data-asks-search-count`, `</p>`))
	}
	for _, a := range []attention.Ask{open, answered, dismissed} {
		n := strconv.FormatInt(a.ID, 10)
		li := liByID(page, "search-ask-"+n)
		if !strings.Contains(li, `<a href="/attention/asks/`+n+`"`) {
			t.Errorf("result %s does not link to its page:\n%s", n, li)
		}
		if got := chipsIn(li); strings.Join(got, ",") != "Ask #"+n+"|ask|"+n+"|Ask #"+n {
			t.Errorf("result %s's id chip: %v", n, got)
		}
	}
	// Its state and its answer are on it.
	if got := tagless(liByID(page, "search-ask-"+strconv.FormatInt(answered.ID, 10))); !strings.Contains(got, "Answered · ") || !strings.Contains(got, "Stripes") {
		t.Errorf("the answered result reads %q", got)
	}
	if got := tagless(liByID(page, "search-ask-"+strconv.FormatInt(dismissed.ID, 10))); !strings.Contains(got, "Dismissed · ") {
		t.Errorf("the dismissed result reads %q", got)
	}
	// The box holds the query; no tabs under it.
	if !strings.Contains(page, `name="q" value="zebra"`) || strings.Contains(page, "data-asks-tabs") {
		t.Errorf("the box does not hold the query, or the tabs are still drawn")
	}
	// One match is a list too.
	code, loc, page = f.searchGet(t, "something ELSE")
	if code != http.StatusOK || loc != "" || !strings.Contains(page, `data-asks-search-count="1"><span>1 result</span> <a `) {
		t.Errorf("a one-result text search: %d %q", code, loc)
	}
}

// TestASearchThatFindsNothingSaysSoInOneLine: the whole line, normalised —
// the query quoted, and Clear back to the page.
func TestASearchThatFindsNothingSaysSoInOneLine(t *testing.T) {
	f := newAttnFixture(t)
	f.mirrorAsk(t, "Anything?")
	code, _, page := f.searchGet(t, `no such "thing"`)
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	line := htmlRegion(page, `data-asks-search-empty`, `</p>`)
	if got := tagless(`<p ` + line); got != `No asks match “no such "thing"”. Clear` {
		t.Errorf("the empty line reads %q", got)
	}
	if !strings.Contains(line, `<a class="`) || !strings.Contains(line, `href="/attention" data-asks-search-clear>Clear</a>`) {
		t.Errorf("Clear does not go back to the page: %s", line)
	}
}

// TestAnEmptySearchIsThePageAsBefore: no ?q=, a blank one, or spaces: the
// tabs, no results — with the box over them, empty.
func TestAnEmptySearchIsThePageAsBefore(t *testing.T) {
	f := newAttnFixture(t)
	f.mirrorAsk(t, "Anything?")
	for _, path := range []string{"/attention", "/attention?q=", "/attention?q=%20%20"} {
		page := f.page(t, path)
		if !strings.Contains(page, "data-asks-tabs") || strings.Contains(page, "asks-search-results") {
			t.Errorf("%s: not the page as before", path)
		}
		if !strings.Contains(page, `<form role="search" method="get" action="/attention" hx-boost="false"`) || !strings.Contains(page, `name="q" value=""`) {
			t.Errorf("%s: no empty search box", path)
		}
	}
}

// TestADraftIdFindsTheDraftAndItsStepUpCards: "draft N" lists the draft and
// every step-up card confirming it — each labelled "Draft #N" or "Ask #M",
// never a bare number — and a bare N finds the draft too. A draft with no
// card goes straight to its card on the Drafts tab.
func TestADraftIdFindsTheDraftAndItsStepUpCards(t *testing.T) {
	f := newAttnFixture(t)
	_, d := f.pendingDraft(t, "Thanks, the walrus ships Friday.")
	dn := strconv.FormatInt(d.ID, 10)
	code, loc, _ := f.searchGet(t, "draft "+dn)
	if code != http.StatusSeeOther || loc != "/attention?tab=drafts&draft="+dn+"#draft-"+dn {
		t.Fatalf("a draft alone: %d %q, want 303 to its card", code, loc)
	}
	card, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, StepUp: &attention.StepUpCard{
		Tool: "plugin_draft_decide", Title: "Send this reply on Attention inbox? · Draft #" + dn, Why: "W", Preview: d.Body, Details: "D", DraftID: d.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE id = $1`, card.ID)
	})
	cn := strconv.FormatInt(card.ID, 10)
	for _, q := range []string{"draft " + dn, "Draft #" + dn, "draft#" + dn} {
		code, loc, page := f.searchGet(t, q)
		got := searchResults(page)
		if code != http.StatusOK || loc != "" || !(len(got) == 2 && strings.Contains(strings.Join(got, ","), "draft:"+dn) && strings.Contains(strings.Join(got, ","), "ask:"+cn)) {
			t.Errorf("?q=%q: %d %q %v, want the draft and its card", q, code, loc, got)
			continue
		}
		if c := chipsIn(liByID(page, "search-draft-"+dn)); strings.Join(c, ",") != "Draft #"+dn+"|draft|"+dn+"|Draft #"+dn {
			t.Errorf("the draft's chip: %v", c)
		}
		if c := chipsIn(liByID(page, "search-ask-"+cn)); strings.Join(c, ",") != "Ask #"+cn+"|ask|"+cn+"|Ask #"+cn {
			t.Errorf("the card's chip: %v", c)
		}
		if title := strings.TrimPrefix(htmlRegion(liByID(page, "search-ask-"+cn), `data-asks-search-title>`, `</span>`), `data-asks-search-title>`); title != "Send this reply on Attention inbox? · Draft #"+dn {
			t.Errorf("the card's title %q", title)
		}
	}
	// The draft by its text, too.
	if _, _, page := f.searchGet(t, "WALRUS ships"); !strings.Contains(strings.Join(searchResults(page), ","), "draft:"+dn) {
		t.Errorf("the draft by its text: %v", searchResults(page))
	}
}

// TestEveryAskCardShowsItsLabelledId: the Open feed's card, the Answered
// card, the ask page's header and the Drafts tab's card each draw their id,
// labelled ("Ask #N", "Draft #N"), as plain text the id script makes a
// button of — on the card's top row, OUTSIDE its head toggle. RED before: no
// card drew an id.
func TestEveryAskCardShowsItsLabelledId(t *testing.T) {
	f := newAttnFixture(t)
	open := f.mirrorAsk(t, "Rotate the key?")
	answered := f.mirrorAsk(t, "Which region?")
	if _, err := f.attn.Answer(asUser(1), answered.ID, "us-east"); err != nil {
		t.Fatal(err)
	}
	_, d := f.pendingDraft(t, "Sounds good.")
	on, an, dn := strconv.FormatInt(open.ID, 10), strconv.FormatInt(answered.ID, 10), strconv.FormatInt(d.ID, 10)

	page := f.page(t, "/attention")
	li := liByID(page, "ask-"+on)
	if got := chipsIn(li); strings.Join(got, ",") != "Ask #"+on+"|ask|"+on+"|Ask #"+on {
		t.Errorf("the Open card's chip: %v\n%s", got, li)
	}
	// On the top row: the toggle (no button inside it) and THEN the chip,
	// both children of data-asks-top, before the question.
	top := regexp.MustCompile(`<div class="[^"]*" data-asks-top><button type="button"[^>]*data-asks-toggle>([^<]|<[^b/]|</[^b])*</button><span class="[^"]*" data-id-copy="Ask #` + on + `"[^>]*>Ask #` + on + `</span></div>\s*<p[^>]*data-attention-text>`)
	if !top.MatchString(li) {
		t.Errorf("the Open card's chip is not on its top row after its toggle:\n%s", li)
	}
	if got := chipsIn(liByID(page, "answered-"+an)); strings.Join(got, ",") != "Ask #"+an+"|ask|"+an+"|Ask #"+an {
		t.Errorf("the Answered card's chip: %v", got)
	}
	if got := chipsIn(liByID(page, "draft-"+dn)); strings.Join(got, ",") != "Draft #"+dn+"|draft|"+dn+"|Draft #"+dn {
		t.Errorf("the draft card's chip: %v", got)
	}
	// The id script is on the page, once.
	if n := strings.Count(page, "/static/js/id-copy"); n != 1 {
		t.Errorf("the id script is on the page %d times", n)
	}

	ask := f.page(t, "/attention/asks/"+on)
	head := htmlRegion(ask, `data-asks-page-card`, `</div>`)
	if got := chipsIn(head); strings.Join(got, ",") != "Ask #"+on+"|ask|"+on+"|Ask #"+on {
		t.Errorf("the ask page's header chip: %v\n%s", got, head)
	}
	if n := strings.Count(ask, "/static/js/id-copy"); n != 1 {
		t.Errorf("the id script is on the ask page %d times", n)
	}
}

// TestASearchKeepsToTheAskLogsTextWindow: the page passes the ask log's text
// window (30 days, its default with the log off) to the search: an ask older
// than it is not found by its words, while its id still goes straight to it;
// an ask a day inside it is found by its words (the control).
func TestASearchKeepsToTheAskLogsTextWindow(t *testing.T) {
	f := newAttnFixture(t)
	old := f.mirrorAsk(t, "Retire the osprey cluster?")
	recent := f.mirrorAsk(t, "Resize the osprey pool?")
	f.setCreated(t, "attention_asks", old.ID, time.Now().Add(-31*24*time.Hour))
	f.setCreated(t, "attention_asks", recent.ID, time.Now().Add(-29*24*time.Hour))
	_, _, page := f.searchGet(t, "osprey")
	if got := searchResults(page); strings.Join(got, ",") != "ask:"+strconv.FormatInt(recent.ID, 10) {
		t.Errorf("by its words: %v, want only the ask inside the window (%d)", got, recent.ID)
	}
	if code, loc, _ := f.searchGet(t, strconv.FormatInt(old.ID, 10)); code != http.StatusSeeOther || loc != "/attention/asks/"+strconv.FormatInt(old.ID, 10) {
		t.Errorf("the old ask by its id: %d %q, want 303 to its page", code, loc)
	}
}
