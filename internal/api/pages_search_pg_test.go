package api

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// THE PAGES LIST'S SEARCH AND FILTERS (pages_search.go; docs/pages.md
// "Finding a page") through the real server against a real Postgres. Every
// title and name is invented.

// pagesListIDs is the page ids the list's markup draws rows for, in order.
func pagesListIDs(t *testing.T, markup string) []int64 {
	t.Helper()
	var out []int64
	for _, m := range regexp.MustCompile(`<li data-page-id="(\d+)"`).FindAllStringSubmatch(markup, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		out = append(out, id)
	}
	return out
}

// sortedIDs is ids sorted (the list's order is its own; these tests pin WHICH
// pages are listed).
func sortedIDs(ids ...int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// TestThePagesListSearchesAndFilters: four pages whose every property the
// filter reads is pairwise distinct —
//
//   - venue: "Venue survey" by agent, its question unanswered (waiting);
//   - budget: "Budget review" by agent2, answered from the owner's view;
//   - notes: "Launch notes" by agent, an html block only (cannot be judged);
//   - room: "Room <venue's id> booking" by agent2 — its TITLE holds venue's
//     number, so an id search that matched titles would list it — answered
//     through a link at v1 only, then given a v2 (waiting again).
//
// ?q= by title (case-folded), by "#id", by a bare id and by "page <id>" (the
// id only, never room's title), by maker; ?waiting=1; ?by=; two together; a
// search that matches nothing ("No pages match." and the way back); and the
// form's live request (the region alone, the address to show).
//
// NEW FEATURE: at origin/main the list ignores every parameter and draws all
// four rows, so each filtered case below is red there (the matrix is in the
// PR); the unfiltered case is the control.
func TestThePagesListSearchesAndFilters(t *testing.T) {
	f := newPageFixture(t)
	const q = `[{"type":"markdown","text":"# Plan"},{"type":"question","id":"q1","label":"Which day suits you?"}]`
	venue := f.create(t, f.agent, "Venue survey", q)
	budget := f.create(t, f.agent2, "Budget review", q)
	if w := f.ownerSubmit(t, budget, 1, "q1", `{"answer":"Monday"}`, f.ownerHdr()); w.Code != 200 {
		t.Fatalf("the owner's answer: %d %s", w.Code, w.Body.String())
	}
	notes := f.create(t, f.agent, "Launch notes", `[{"type":"html","html":"<p>custom</p>"}]`)
	room := f.create(t, f.agent2, "Room "+strconv.FormatInt(venue, 10)+" booking", q)
	l := f.liveLink(t, f.owner, room)
	if w := f.submit(t, l.Token, "q1", `{"answer":"Friday"}`); w.Code != 200 {
		t.Fatalf("a reply through the link: %d %s", w.Code, w.Body.String())
	}
	f.tool(t, f.agent2, pageUpdateTool, map[string]any{"page_id": room, "blocks": json.RawMessage(q)})

	list := func(query string) string {
		t.Helper()
		w := f.get(t, "/pages?"+query, f.ownerHdr())
		if w.Code != 200 {
			t.Fatalf("/pages?%s: %d", query, w.Code)
		}
		return w.Body.String()
	}
	// CONTROL: unfiltered, every page is listed, each with its number, and
	// no status line is drawn.
	all := list("")
	if got := sortedIDs(pagesListIDs(t, all)...); !slices.Equal(got, sortedIDs(venue, budget, notes, room)) {
		t.Fatalf("CONTROL: the unfiltered list draws %v, want all four", got)
	}
	for _, id := range []int64{venue, budget, notes, room} {
		if !regexp.MustCompile(`<span data-page-number="" class="[^"]*">#` + strconv.FormatInt(id, 10) + `</span>`).MatchString(all) {
			t.Errorf("page %d's row does not show its number", id)
		}
	}
	if n := strings.Count(all, ui.PagesSearchScriptHTML()); n != 1 {
		t.Errorf("the list writes the search's script %d times, want once", n)
	}
	if strings.Contains(all, `data-pages-count`) || strings.Contains(all, `data-pages-none`) {
		t.Errorf("the unfiltered list draws a status line")
	}
	if !strings.Contains(all, `by `+f.agent.Name) || !strings.Contains(all, `by `+f.agent2.Name) {
		t.Fatalf("CONTROL: the rows do not name the makers %q and %q", f.agent.Name, f.agent2.Name)
	}
	// The form: its box, the toggle with how many wait (venue and room), and
	// a select of the two makers.
	for _, want := range []string{
		`id="` + ui.PagesSearchInputID + `" name="q" value=""`,
		`name="waiting" value="1"`,
		`data-pages-waiting-count>2</span>`,
		`<option value="` + f.agent.Name + `">By ` + f.agent.Name + `</option>`,
		`<option value="` + f.agent2.Name + `">By ` + f.agent2.Name + `</option>`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the list's form lacks %q", want)
		}
	}

	// The ⓘ says what "Waiting on you" cannot judge.
	if !strings.Contains(all, `data-pages-waiting-about>`+pagesWaitingAbout+`</p>`) || !strings.Contains(pagesWaitingAbout, "only through its own script can't be judged") {
		t.Errorf("the list's ⓘ does not say an html-only page cannot be judged")
	}

	sv := strconv.FormatInt(venue, 10)
	for _, c := range []struct {
		name  string
		query url.Values
		want  []int64
	}{
		{"a title, case-folded", url.Values{"q": {"VENUE"}}, []int64{venue}},
		{"a title's middle", url.Values{"q": {"et rev"}}, []int64{budget}},
		{"#id (never room, whose title holds the digits)", url.Values{"q": {"#" + sv}}, []int64{venue}},
		{"a bare id", url.Values{"q": {sv}}, []int64{venue}},
		{"page <id>", url.Values{"q": {"page " + sv}}, []int64{venue}},
		{"an id nobody has", url.Values{"q": {"#999999999"}}, nil},
		{"a maker's name", url.Values{"q": {f.agent2.Name}}, []int64{budget, room}},
		{"waiting on you", url.Values{"waiting": {"1"}}, []int64{venue, room}},
		{"made by", url.Values{"by": {f.agent.Name}}, []int64{venue, notes}},
		{"waiting, made by", url.Values{"waiting": {"1"}, "by": {f.agent2.Name}}, []int64{room}},
		{"words and waiting", url.Values{"q": {"booking"}, "waiting": {"1"}}, []int64{room}},
		{"nothing", url.Values{"q": {"zebra crossing"}}, nil},
	} {
		got := list(c.query.Encode())
		if ids := sortedIDs(pagesListIDs(t, got)...); !slices.Equal(ids, sortedIDs(c.want...)) {
			t.Errorf("%s (%s): lists %v, want %v", c.name, c.query.Encode(), ids, sortedIDs(c.want...))
		}
		if len(c.want) == 0 {
			if !strings.Contains(got, `data-pages-none><span>No pages match.</span>`) || !strings.Contains(got, `href="/pages" class="`) {
				t.Errorf("%s: no \"No pages match.\" with the way back", c.name)
			}
		} else if !strings.Contains(got, `data-pages-count="`+strconv.Itoa(len(c.want))+`"`) {
			t.Errorf("%s: no count line of %d", c.name, len(c.want))
		}
	}
	// The filter is drawn back into the form.
	kept := list(url.Values{"q": {"#" + sv}, "waiting": {"1"}, "by": {f.agent.Name}}.Encode())
	for _, want := range []string{`name="q" value="#` + sv + `"`, `data-pages-waiting-input`, `<option value="` + f.agent.Name + `" selected>`} {
		if !strings.Contains(kept, want) {
			t.Errorf("the filtered list's form lacks %q", want)
		}
	}
	if !regexp.MustCompile(`value="1" class="[^"]*" checked data-pages-waiting-input`).MatchString(kept) {
		t.Errorf("the toggle is not checked under ?waiting=1")
	}

	// The form's live request: the region alone, and the address to show.
	live := f.get(t, "/pages?q=venue&waiting=1", merge(f.ownerHdr(), map[string]string{"HX-Target": ui.PagesListRegionID}))
	body := live.Body.String()
	if got := pagesListIDs(t, body); !slices.Equal(got, []int64{venue}) {
		t.Errorf("the live request lists %v, want [%d]", got, venue)
	}
	if strings.Contains(body, "<section") || strings.Contains(body, "data-pages-filter") {
		t.Errorf("the live request drew more than the region: %s", body)
	}
	if got := live.Header().Get("HX-Replace-Url"); got != "/pages?q=venue&waiting=1" {
		t.Errorf("HX-Replace-Url %q, want /pages?q=venue&waiting=1", got)
	}
	if got := live.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the live list is cacheable: %q", got)
	}
	// A cleared box's live request: the whole list, at the bare address.
	if w := f.get(t, "/pages?q=", merge(f.ownerHdr(), map[string]string{"HX-Target": ui.PagesListRegionID})); w.Header().Get("HX-Replace-Url") != "/pages" ||
		len(pagesListIDs(t, w.Body.String())) != 4 {
		t.Errorf("a cleared search: %q, %v", w.Header().Get("HX-Replace-Url"), pagesListIDs(t, w.Body.String()))
	}
}

// TestParsePagesFilter: what the box's text names — an id-shaped query is the
// id only (no text), anything else is folded text; ?waiting is on only as
// "1"; ?by= is trimmed. Pure: no database.
func TestParsePagesFilter(t *testing.T) {
	for _, c := range []struct {
		q    url.Values
		want pagesFilter
	}{
		{url.Values{"q": {"12"}}, pagesFilter{q: "12", id: 12}},
		{url.Values{"q": {" #12 "}}, pagesFilter{q: "#12", id: 12}},
		{url.Values{"q": {"Page #7"}}, pagesFilter{q: "Page #7", id: 7}},
		{url.Values{"q": {"#0"}}, pagesFilter{q: "#0", text: "#0"}},
		{url.Values{"q": {"12 apples"}}, pagesFilter{q: "12 apples", text: "12 apples"}},
		{url.Values{"q": {"Budget"}}, pagesFilter{q: "Budget", text: "budget"}},
		{url.Values{"waiting": {"1"}}, pagesFilter{waiting: true}},
		{url.Values{"waiting": {"yes"}}, pagesFilter{}},
		{url.Values{"by": {"  Release helper "}}, pagesFilter{by: "Release helper"}},
	} {
		if got := parsePagesFilter(c.q); got != c.want {
			t.Errorf("parsePagesFilter(%s) = %+v, want %+v", c.q.Encode(), got, c.want)
		}
	}
}
