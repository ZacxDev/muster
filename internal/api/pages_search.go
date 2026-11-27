package api

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// SEARCHING AND FILTERING THE PAGES LIST (operator decision: "basic filters
// and search for pages … see the page ID on the page card … search by ID";
// docs/pages.md "Finding a page"). The form over /pages (ui/page_search.go)
// carries three parameters, so a filtered list is a link and works with no
// script:
//
//   - ?q= — AN ID ("12", "#12", "page 12") is looked up by id ONLY, never as
//     text (the Asks search's rule, idQueryPattern): a title that happens to
//     hold the digits is not listed beside the page that has that number.
//     ANYTHING ELSE is a case-insensitive substring of the page's title or
//     of its maker's name as its row shows it (pageMakerName).
//   - ?waiting=1 — only the pages waiting on the reader: their current
//     version has an answerable component nobody has answered at that version
//     (pages.Store.Unanswered). A page whose only interactive content is an
//     html block cannot be judged, and is never listed by it.
//   - ?by=<maker> — only the pages its row says were made "by <maker>".
//
// All three narrow the list the reader already reads (row-level security:
// their own pages and their teams'); together they must all hold.
// ---------------------------------------------------------------------------

// The list's query parameters.
const (
	pagesSearchParam  = "q"
	pagesWaitingParam = "waiting"
	pagesByParam      = "by"
)

// pagesWaitingAbout is the list's ⓘ on "Waiting on you": what it lists, and
// the pages it cannot judge.
const pagesWaitingAbout = "Search finds a page by its title, its maker or its number (#12). Waiting on you lists the pages whose current version has a question, choice, form, upload, approval or gallery pick nobody has answered yet. " +
	"A page that asks only through its own script can't be judged, so it is never listed there. Press / to search."

// pageQueryID is an id-shaped page search: an optional "page", an optional
// "#", then the number.
var pageQueryID = idQueryPattern("page")

// pagesFilter is the list's filter as a request names it.
type pagesFilter struct {
	// q is the box's text as typed (trimmed); id the page it names when it is
	// id-shaped (0: text), text its folded words otherwise.
	q    string
	id   int64
	text string
	// waiting and by are the toggle and the maker chosen.
	waiting bool
	by      string
}

// parsePagesFilter is the filter q names.
func parsePagesFilter(q url.Values) pagesFilter {
	f := pagesFilter{q: truncateRunesTo(strings.TrimSpace(q.Get(pagesSearchParam)), pages.MaxTitleSearchRunes)}
	if m := pageQueryID.FindStringSubmatch(f.q); m != nil {
		if n, err := strconv.ParseInt(m[2], 10, 64); err == nil && n > 0 {
			f.id = n
		}
	}
	if f.id == 0 {
		f.text = strings.ToLower(f.q)
	}
	f.waiting = q.Get(pagesWaitingParam) == "1"
	f.by = strings.TrimSpace(q.Get(pagesByParam))
	return f
}

// on reports whether any of the three is set.
func (f pagesFilter) on() bool { return f.q != "" || f.waiting || f.by != "" }

// keep reports whether f keeps page p, whose row names maker and which has
// unanswered components unanswered.
func (f pagesFilter) keep(p pages.Summary, maker string, unanswered int) bool {
	switch {
	case f.id > 0 && p.ID != f.id:
		return false
	case f.text != "" && !strings.Contains(strings.ToLower(p.Title), f.text) && !strings.Contains(strings.ToLower(maker), f.text):
		return false
	case f.waiting && unanswered == 0:
		return false
	case f.by != "" && maker != f.by:
		return false
	}
	return true
}

// query is f as the list's address's query ("" for none).
func (f pagesFilter) query() string {
	v := url.Values{}
	if f.q != "" {
		v.Set(pagesSearchParam, f.q)
	}
	if f.waiting {
		v.Set(pagesWaitingParam, "1")
	}
	if f.by != "" {
		v.Set(pagesByParam, f.by)
	}
	return v.Encode()
}

// href is the list's address with f.
func (f pagesFilter) href() string {
	if q := f.query(); q != "" {
		return pagesPath + "?" + q
	}
	return pagesPath
}

// pagesListLive reports whether r is the list form's own live request
// (ui/page_search.go): the list region alone is answered.
func pagesListLive(r *http.Request) bool { return hxTargets(r, ui.PagesListRegionID) }

// pageMakers is the distinct makers of list, as their rows name them, sorted
// case-folded (the "Made by" select's choices).
func pageMakers(list []pages.Summary) []string {
	var out []string
	for _, p := range list {
		if m := pageMakerName(p.Creator.Label, p.Creator.MirrorHost); m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return out
}
