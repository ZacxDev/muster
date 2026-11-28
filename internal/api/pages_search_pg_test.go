package api

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestThePagesListSearchesAndFilters: five pages whose every property the
// filter reads is pairwise distinct, made by two agents whose names hold NO
// digit (so only a title can spell a number):
//
//   - venue: "Venue survey" by a1, its question unanswered (waiting);
//   - budget: "Budget review" by a2, answered from the owner's view;
//   - notes: "Launch notes" by a1, an html block only (cannot be judged);
//   - room: "Room <venue's id> booking" by a2 — its TITLE holds venue's
//     number — answered through a link at v1 only, then given a v2 (waiting
//     again). Made after venue, so the list's own order puts it ABOVE venue;
//   - ledger: "Ledger <ten copies of a digit venue's id lacks>" by a1, a
//     markdown block only: a number that is no page's id, held by its title.
//
// ?q= by title (case-folded); a number ("#id", a bare id, "page <id>") lists
// venue FIRST, then room (operator decision: "ID first, then titles"),
// never budget, notes or ledger; a number that is no page's id lists the
// title that holds it; by maker; ?waiting=1; ?by=; two together; a search
// that matches nothing ("No pages match." and the way back); and the form's
// live request (the region alone, the address to show).
//
// At origin/main the list ignores every parameter (red there); at bab5ee17f a
// number was the id only, so the "ID first, then titles" cases and the
// no-such-id number are red there. The unfiltered case is the control.
func TestThePagesListSearchesAndFilters(t *testing.T) {
	f := newPageFixture(t)
	// Two agents of the owner's whose names hold no digit.
	letters := strings.NewReplacer("0", "a", "1", "b", "2", "c", "3", "d", "4", "e", "5", "f", "6", "g", "7", "h", "8", "i", "9", "j").Replace(strconv.FormatInt(time.Now().UnixNano(), 36))
	a1, a2 := f.newAgent(t, f.owner, "search-one-"+letters), f.newAgent(t, f.owner, "search-two-"+letters)
	if strings.ContainsAny(a1.Name+a2.Name, "0123456789") {
		t.Fatalf("CONTROL: an agent's name holds a digit: %q %q", a1.Name, a2.Name)
	}
	const q = `[{"type":"markdown","text":"# Plan"},{"type":"question","id":"q1","label":"Which day suits you?"}]`
	venue := f.create(t, a1, "Venue survey", q)
	budget := f.create(t, a2, "Budget review", q)
	if w := f.ownerSubmit(t, budget, 1, "q1", `{"answer":"Monday"}`, f.ownerHdr()); w.Code != 200 {
		t.Fatalf("the owner's answer: %d %s", w.Code, w.Body.String())
	}
	notes := f.create(t, a1, "Launch notes", `[{"type":"html","html":"<p>custom</p>"}]`)
	room := f.create(t, a2, "Room "+strconv.FormatInt(venue, 10)+" booking", q)
	l := f.liveLink(t, f.owner, room)
	if w := f.submit(t, l.Token, "q1", `{"answer":"Friday"}`); w.Code != 200 {
		t.Fatalf("a reply through the link: %d %s", w.Code, w.Body.String())
	}
	f.tool(t, a2, pageUpdateTool, map[string]any{"page_id": room, "blocks": json.RawMessage(q)})
	// A digit venue's id does not hold, ten times: no page's id, and a title
	// that cannot hold venue's number.
	d := "0123456789"[strings.IndexFunc("0123456789", func(r rune) bool { return !strings.ContainsRune(strconv.FormatInt(venue, 10), r) })]
	other := strings.Repeat(string(d), 10)
	ledger := f.create(t, a1, "Ledger "+other, `[{"type":"markdown","text":"# Ledger"}]`)

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
	if got := sortedIDs(pagesListIDs(t, all)...); !slices.Equal(got, sortedIDs(venue, budget, notes, room, ledger)) {
		t.Fatalf("CONTROL: the unfiltered list draws %v, want all five", got)
	}
	// CONTROL: the list's own order puts room above venue, so "venue first"
	// below is the number's doing.
	if got := pagesListIDs(t, all); slices.Index(got, room) > slices.Index(got, venue) {
		t.Fatalf("CONTROL: the unfiltered list draws venue above room: %v", got)
	}
	for _, id := range []int64{venue, budget, notes, room, ledger} {
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
	if !strings.Contains(all, `by `+a1.Name) || !strings.Contains(all, `by `+a2.Name) {
		t.Fatalf("CONTROL: the rows do not name the makers %q and %q", a1.Name, a2.Name)
	}
	// The form: its box, the toggle with how many wait (venue and room), and
	// a select of the two makers.
	for _, want := range []string{
		`id="` + ui.PagesSearchInputID + `" name="q" value=""`,
		`name="waiting" value="1"`,
		`data-pages-waiting-count>2</span>`,
		`<option value="` + a1.Name + `">By ` + a1.Name + `</option>`,
		`<option value="` + a2.Name + `">By ` + a2.Name + `</option>`,
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
		// ordered: want is the list's exact order, not just its set.
		ordered bool
	}{
		{"a title, case-folded", url.Values{"q": {"VENUE"}}, []int64{venue}, false},
		{"a title's middle", url.Values{"q": {"et rev"}}, []int64{budget}, false},
		{"#id: the page first, then the title that holds the digits", url.Values{"q": {"#" + sv}}, []int64{venue, room}, true},
		{"a bare id: the same", url.Values{"q": {sv}}, []int64{venue, room}, true},
		{"page <id>: the same", url.Values{"q": {"page " + sv}}, []int64{venue, room}, true},
		{"a number that is no page's id: the title that holds it", url.Values{"q": {other}}, []int64{ledger}, false},
		{"#<a number that is no page's id>: the same", url.Values{"q": {"#" + other}}, []int64{ledger}, false},
		{"a number nothing holds", url.Values{"q": {"#" + strings.Repeat(string(d), 12)}}, nil, false},
		{"a maker's name", url.Values{"q": {a2.Name}}, []int64{budget, room}, false},
		{"waiting on you", url.Values{"waiting": {"1"}}, []int64{venue, room}, false},
		{"made by", url.Values{"by": {a1.Name}}, []int64{venue, notes, ledger}, false},
		{"waiting, made by", url.Values{"waiting": {"1"}, "by": {a2.Name}}, []int64{room}, false},
		{"words and waiting", url.Values{"q": {"booking"}, "waiting": {"1"}}, []int64{room}, false},
		{"nothing", url.Values{"q": {"zebra crossing"}}, nil, false},
	} {
		got := list(c.query.Encode())
		if ids := sortedIDs(pagesListIDs(t, got)...); !slices.Equal(ids, sortedIDs(c.want...)) {
			t.Errorf("%s (%s): lists %v, want %v", c.name, c.query.Encode(), ids, sortedIDs(c.want...))
		} else if ids := pagesListIDs(t, got); c.ordered && !slices.Equal(ids, c.want) {
			t.Errorf("%s (%s): lists in the order %v, want %v (the id's page first)", c.name, c.query.Encode(), ids, c.want)
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
	kept := list(url.Values{"q": {"#" + sv}, "waiting": {"1"}, "by": {a1.Name}}.Encode())
	for _, want := range []string{`name="q" value="#` + sv + `"`, `data-pages-waiting-input`, `<option value="` + a1.Name + `" selected>`} {
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
		len(pagesListIDs(t, w.Body.String())) != 5 {
		t.Errorf("a cleared search: %q, %v", w.Header().Get("HX-Replace-Url"), pagesListIDs(t, w.Body.String()))
	}
}

// TestParsePagesFilter: what the box's text names — a number-shaped query is
// its digits and the id they name (0 for "#0"; past 18 digits it is not
// number-shaped, idQueryPattern, and is text), anything else is folded text; ?waiting is on only as
// "1"; ?by= is trimmed. Pure: no database.
func TestParsePagesFilter(t *testing.T) {
	for _, c := range []struct {
		q    url.Values
		want pagesFilter
	}{
		{url.Values{"q": {"12"}}, pagesFilter{q: "12", id: 12, digits: "12"}},
		{url.Values{"q": {" #12 "}}, pagesFilter{q: "#12", id: 12, digits: "12"}},
		{url.Values{"q": {"Page #7"}}, pagesFilter{q: "Page #7", id: 7, digits: "7"}},
		{url.Values{"q": {"#0"}}, pagesFilter{q: "#0", digits: "0"}},
		{url.Values{"q": {"#007"}}, pagesFilter{q: "#007", id: 7, digits: "007"}},
		{url.Values{"q": {"99999999999999999999"}}, pagesFilter{q: "99999999999999999999", text: "99999999999999999999"}},
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
