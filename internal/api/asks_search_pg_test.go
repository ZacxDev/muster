package api

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	// No existence leak: another owner's ask id reads exactly as an id no ask
	// (and no draft) has.
	theirsQ, missingQ := "ask "+strconv.FormatInt(theirs.ID, 10), "ask "+strconv.FormatInt(theirs.ID+1_000_000, 10)
	c1, _, p1 := f.searchGet(t, theirsQ)
	c2, _, p2 := f.searchGet(t, missingQ)
	if a, b := searchResultsSection(p1, theirsQ), searchResultsSection(p2, missingQ); a == "" || a != b || c1 != c2 {
		t.Errorf("another owner's ask id reads differently from a missing one (%d, %d):\n%s\n---\n%s", c1, c2, a, b)
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
	// The box holds the query. The results fill the region, and the tabs are
	// drawn AFTER it, in the wrapper the stylesheet hides while the region
	// holds anything (so emptying the box shows them with no reload).
	if !strings.Contains(page, `name="q" value="zebra"`) {
		t.Errorf("the box does not hold the query")
	}
	region := strings.Index(page, `<div id="asks-search-region" class="contents" data-asks-search-region><section `)
	main := strings.Index(page, `<div class="contents" data-asks-main>`)
	tabs := strings.Index(page, "data-asks-tabs")
	if region < 0 || main < region || tabs < main {
		t.Errorf("not results in the region, then the tabs in their wrapper: region %d, wrapper %d, tabs %d", region, main, tabs)
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
		// The region is there, EMPTY to the byte (the stylesheet's :empty).
		if !strings.Contains(page, `<div id="asks-search-region" class="contents" data-asks-search-region></div><div class="contents" data-asks-main>`) {
			t.Errorf("%s: no empty results region before the page", path)
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
	// On the top row: the head (the link to the ask's page, holding no
	// button), the chevron toggle, and THEN the chip, all children of
	// data-asks-top, before the question.
	top := regexp.MustCompile(`<div class="[^"]*" data-asks-top><a href="/attention/asks/` + on + `"[^>]*data-asks-card-link>([^<]|<[^ab/]|</[^ab])*</a><button type="button"[^>]*data-asks-toggle><svg[^>]*><path[^>]*/></svg></button><span class="[^"]*" data-id-copy="Ask #` + on + `"[^>]*>Ask #` + on + `</span></div>\s*<div[^>]*data-attention-text>`)
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

// TestASearchFindsAnAskOfAnyAge: the page searches every ask the owner can
// still open by its words, whatever its age (operator decision): an ask
// created 400 days ago is listed beside a recent one. RED while the page
// passed the ask log's 30-day text window to the search.
func TestASearchFindsAnAskOfAnyAge(t *testing.T) {
	f := newAttnFixture(t)
	old := f.mirrorAsk(t, "Retire the osprey cluster?")
	recent := f.mirrorAsk(t, "Resize the osprey pool?")
	f.setCreated(t, "attention_asks", old.ID, time.Now().Add(-400*24*time.Hour))
	_, _, page := f.searchGet(t, "osprey")
	want := "ask:" + strconv.FormatInt(recent.ID, 10) + ",ask:" + strconv.FormatInt(old.ID, 10)
	if got := searchResults(page); strings.Join(got, ",") != want {
		t.Errorf("by its words: %v, want %s", got, want)
	}
}

// searchResultsSection is the page's search results, the query's own spelling
// replaced by Q: what two searches for different ids must draw identically.
func searchResultsSection(page, raw string) string {
	return strings.ReplaceAll(htmlRegion(page, `data-testid="asks-search-results"`, `</section>`), raw, "Q")
}

// TestAnIdSearchFindsATeamDraftTheMemberCanOpen: an id is looked up in every
// draft the signed-in person can already OPEN (operator decision) — so a
// team's operator, who reads the team's draft on the Drafts tab (the control),
// pasting its "Draft #N" chip goes straight to that card. Text is still the
// person's own: the team draft's words find nothing. Another person, outside
// the team, gets exactly what an id no draft has gets — no existence leak.
// RED at 2244c0e1 (an id search was the owner's own only): "No asks match".
func TestAnIdSearchFindsATeamDraftTheMemberCanOpen(t *testing.T) {
	w := newIsoWorld(t)
	f, _ := newFanTeam(t, w)
	dm, _, _ := fanDrafts(t, w, f.team)
	dn := strconv.FormatInt(dm, 10)
	search := func(uid int64, raw string) *httptest.ResponseRecorder {
		return w.do(w.h, http.MethodGet, "/attention?q="+url.QueryEscape(raw), "", cred{sessionUser: uid})
	}
	// Control: the member opens the team's draft on the Drafts tab.
	if rec := w.do(w.h, http.MethodGet, "/attention?tab=drafts&draft="+dn, "", cred{sessionUser: f.ben}); rec.Code != http.StatusOK || liByID(rec.Body.String(), "draft-"+dn) == "" {
		t.Fatalf("control: the member's Drafts tab does not draw the team's draft %s (%d)", dn, rec.Code)
	}
	for _, q := range []string{"draft " + dn, "Draft #" + dn} {
		if rec := search(f.ben, q); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/attention?tab=drafts&draft="+dn+"#draft-"+dn {
			t.Errorf("the member's ?q=%q: %d %q, want 303 to the team draft's card\n%.300s", q, rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
	}
	// Text stays the member's own: the team draft's words find nothing.
	if rec := search(f.ben, "synthetic hello"); rec.Code != http.StatusOK || len(searchResults(rec.Body.String())) != 0 || !strings.Contains(rec.Body.String(), "data-asks-search-empty") {
		t.Errorf("the member's text search listed the team's draft: %d %v", rec.Code, searchResults(rec.Body.String()))
	}
	// An outsider: the team draft's id reads exactly as an id nobody has.
	missing := strconv.FormatInt(dm+1_000_000, 10)
	theirs, none := search(f.dee, "draft "+dn), search(f.dee, "draft "+missing)
	if theirs.Code != http.StatusOK || theirs.Header().Get("Location") != "" || len(searchResults(theirs.Body.String())) != 0 {
		t.Fatalf("an outsider's search for the team draft: %d %q %v", theirs.Code, theirs.Header().Get("Location"), searchResults(theirs.Body.String()))
	}
	a, b := searchResultsSection(theirs.Body.String(), "draft "+dn), searchResultsSection(none.Body.String(), "draft "+missing)
	if a == "" || a != b || none.Code != theirs.Code {
		t.Errorf("another owner's draft id reads differently from a missing one:\n%s\n---\n%s", a, b)
	}
}

// liveSearch is the search box's own live request for raw: the field's htmx
// GET, which names the results region as its target (no redirect followed).
func (f *attnFixture) liveSearch(t *testing.T, raw string) *httptest.ResponseRecorder {
	t.Helper()
	h := f.operator()
	h["HX-Request"] = "true"
	h["HX-Target"] = "asks-search-region"
	return f.call(t, http.MethodGet, "/attention?q="+url.QueryEscape(raw), nil, h)
}

// TestTheLiveSearchListsAnIdAndNeverJumps: typing never navigates away
// (operator decision). The box's live request for an id — every spelling —
// is answered with the results alone, the ask among them, never the 303 a
// submitted id earns; and the address the page should show. The control: the
// same id SUBMITTED (no live target: a navigation, boosted — the fixture sends
// HX-Request — or not) still goes straight to the ask. RED at origin/main: the
// live request got the 303.
func TestTheLiveSearchListsAnIdAndNeverJumps(t *testing.T) {
	f := newAttnFixture(t)
	a := f.mirrorAsk(t, "Rotate the heron key?")
	id := strconv.FormatInt(a.ID, 10)
	if code, loc, _ := f.searchGet(t, id); code != http.StatusSeeOther || loc != "/attention/asks/"+id {
		t.Fatalf("control: the submitted id: %d %q, want 303 to the ask", code, loc)
	}
	for _, q := range []string{id, "#" + id, "Ask #" + id} {
		w := f.liveSearch(t, q)
		body := w.Body.String()
		if w.Code != http.StatusOK || w.Header().Get("Location") != "" {
			t.Errorf("live ?q=%q: %d %q, want 200 and no redirect", q, w.Code, w.Header().Get("Location"))
			continue
		}
		if got := searchResults(body); strings.Join(got, ",") != "ask:"+id {
			t.Errorf("live ?q=%q: results %v, want the ask", q, got)
		}
		// The region's contents only: the results section, no page around it.
		if !strings.HasPrefix(body, `<section class="flex min-w-0 flex-col gap-2" aria-label="Search results" data-testid="asks-search-results">`) ||
			!strings.HasSuffix(body, "</section>") || strings.Contains(body, "<html") || strings.Contains(body, "data-asks-tabs") {
			t.Errorf("live ?q=%q is not the results alone:\n%.300s", q, body)
		}
		if !strings.Contains(body, `data-asks-search-count="1"><span>1 result</span> <a `) {
			t.Errorf("live ?q=%q: the count line: %s", q, htmlRegion(body, `data-asks-search-count`, `</p>`))
		}
		if got, want := w.Header().Get("HX-Replace-Url"), "/attention?q="+url.QueryEscape(q); got != want {
			t.Errorf("live ?q=%q: HX-Replace-Url %q, want %q", q, got, want)
		}
		if v := w.Header().Values("Vary"); !strings.Contains(strings.Join(v, ","), "HX-Target") {
			t.Errorf("live ?q=%q: Vary %v does not name HX-Target", q, v)
		}
	}
	// Back to an address the typing set, which htmx holds no copy of: it asks
	// the server for the page (HX-History-Restore-Request), and gets the list
	// it left — not the ask again.
	h := f.operator()
	h["HX-History-Restore-Request"] = "true"
	w := f.call(t, http.MethodGet, "/attention?q="+id, nil, h)
	if w.Code != http.StatusOK || strings.Join(searchResults(w.Body.String()), ",") != "ask:"+id || !strings.Contains(w.Body.String(), "data-asks-tabs") {
		t.Errorf("history restore of ?q=%s: %d %q %v, want the page listing the ask", id, w.Code, w.Header().Get("Location"), searchResults(w.Body.String()))
	}
}

// TestTheLiveSearchEmptiedIsThePageAgain: the box emptied (or spaces) answers
// an empty region — which shows the page's tabs again — and the page's own
// address; words answer their results. RED at origin/main: the whole page.
func TestTheLiveSearchEmptiedIsThePageAgain(t *testing.T) {
	f := newAttnFixture(t)
	a := f.mirrorAsk(t, "Feed the pelican?")
	for _, q := range []string{"", "   "} {
		w := f.liveSearch(t, q)
		if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("HX-Replace-Url") != "/attention" {
			t.Errorf("live ?q=%q: %d %q (%d bytes), want 200, empty, /attention", q, w.Code, w.Header().Get("HX-Replace-Url"), w.Body.Len())
		}
	}
	w := f.liveSearch(t, "PELICAN")
	if got := searchResults(w.Body.String()); w.Code != http.StatusOK || strings.Join(got, ",") != "ask:"+strconv.FormatInt(a.ID, 10) {
		t.Errorf("live ?q=PELICAN: %d %v", w.Code, got)
	}
	if got := w.Header().Get("HX-Replace-Url"); got != "/attention?q=PELICAN" {
		t.Errorf("live ?q=PELICAN: HX-Replace-Url %q", got)
	}
}

// TestTheSearchBoxSearchesAsYouType: the box has NO button (operator
// decision); its field asks for results 300ms after the last input (its clear
// button's included), into the results region, a newer
// request replacing one in flight, with the in-flight spinner. RED at
// origin/main: a Search button and no request of its own.
func TestTheSearchBoxSearchesAsYouType(t *testing.T) {
	f := newAttnFixture(t)
	f.mirrorAsk(t, "Anything?")
	page := f.page(t, "/attention")
	form := htmlRegion(page, `<form role="search"`, `</form>`)
	if form == "" {
		t.Fatal("no search form")
	}
	if strings.Contains(form, "<button") || strings.Contains(form, `type="submit"`) || strings.Contains(form, "data-asks-search-submit") {
		t.Errorf("the box still has a button:\n%s", form)
	}
	// Typing updates the address; htmx would first save the whole page (the
	// owner's asks) to localStorage, but for this.
	if !strings.HasPrefix(form, `<form role="search" method="get" action="/attention" hx-boost="false" hx-history="false" `) {
		t.Errorf("the box does not keep the page out of htmx's history cache:\n%.200s", form)
	}
	input := htmlRegion(form, `<input type="search"`, `>`)
	for _, want := range []string{
		` hx-get="/attention" `,
		` hx-trigger="input changed delay:300ms" `,
		` hx-target="#asks-search-region" `,
		` hx-swap="innerHTML" `,
		` hx-sync="this:replace" `,
		` hx-indicator="#asks-search-busy" `,
		` name="q" `,
	} {
		if !strings.Contains(input+" ", want) {
			t.Errorf("the field lacks %q:\n%s", want, input)
		}
	}
	// The in-flight indicator is the element the field names, holding htmx's
	// htmx-indicator spinner.
	busy := htmlRegion(form, `<span id="asks-search-busy"`, `</span>`)
	if !regexp.MustCompile(`<svg class="htmx-indicator [^"]*"[^>]*data-asks-search-spinner>`).MatchString(busy) {
		t.Errorf("no spinner in the indicator:\n%s", busy)
	}
	// The region the field targets is on the page, empty, before the tabs.
	if !strings.Contains(page, `<div id="asks-search-region" class="contents" data-asks-search-region></div>`) {
		t.Errorf("no empty results region")
	}
	if n := strings.Count(page, "/static/js/asks-search"); n != 1 {
		t.Errorf("the search box's script is on the page %d times", n)
	}
}
