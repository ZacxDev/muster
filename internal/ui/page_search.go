package ui

import (
	"html"
	"strconv"
)

// THE PAGES LIST'S SEARCH AND FILTERS (internal/api pages_search.go;
// docs/pages.md "Finding a page"): one form over the list — a search box, a
// "Waiting on you" toggle and, when the list holds pages by more than one
// maker, a "Made by" select. Its state is the address (?q=, ?waiting=1,
// ?by=), so a filtered list is a link, a reload keeps it and it works with no
// script: Enter submits the form, and with no script a button the stylesheet
// shows only then (PagesFilterApplyClass) applies the toggle and the select.
//
// THE ASKS SEARCH'S IDIOM (ui/asks_search.go), not a second one: with a script,
// any change asks the page for the list alone after PagesSearchDebounce (a
// newer request replacing one in flight: hx-sync this:replace), the server
// answers with the list region's contents and the address to show
// (HX-Replace-Url, which htmx applies with history.replaceState: no history
// entry per keystroke), and hx-history="false" keeps htmx from saving the page
// to the browser's storage on each address change. The rules that decide what
// matches live once, on the server: a script never filters rows itself.

// PagesListRegionID is the list region's id: the form's hx-target, and the
// HX-Target header by which the server tells the form's live request from a
// navigation (internal/api pagesListLive).
const PagesListRegionID = "pages-list-region"

// PagesSearchInputID is the search box's id ("/" focuses it: pagesSearchJS).
const PagesSearchInputID = "pages-search-q"

// PagesSearchDebounce is how long the form waits after the last change.
const PagesSearchDebounce = AsksSearchDebounce

// PagesSearchPlaceholder is the box's hint.
const PagesSearchPlaceholder = "Search pages"

// PagesWaitingLabel is the toggle's words.
const PagesWaitingLabel = "Waiting on you"

// PagesNoMatchText is the list's line when nothing matches.
const PagesNoMatchText = "No pages match."

// PagesClearText is the way back to the whole list.
const PagesClearText = "Clear filters"

const (
	// PagesFilterFormClass is the form: the box, then the toggle and the
	// select, wrapping on a phone.
	PagesFilterFormClass = "flex min-w-0 flex-wrap items-center gap-2"
	// PagesSearchBoxClass holds the field and its icon, taking the rest of
	// the row (all of it on a phone).
	PagesSearchBoxClass = "relative flex min-w-0 flex-[1_1_14rem] items-center"
	// PagesWaitingClass is the toggle: a chip (the Asks filter's look) whose
	// whole face is its checkbox, so the target is the chip, 44px tall.
	PagesWaitingClass = "press relative inline-flex min-h-[44px] shrink-0 cursor-pointer items-center gap-1.5 whitespace-nowrap rounded-full bg-s1 px-3 text-[13px] font-medium text-fg ring-1 ring-inset ring-line transition hover:bg-s2 " +
		"has-[:checked]:bg-accent has-[:checked]:text-on-accent has-[:checked]:ring-accent has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-focus"
	// PagesWaitingInputClass is its checkbox, invisible over the chip.
	PagesWaitingInputClass = "absolute inset-0 m-0 h-full w-full cursor-pointer appearance-none rounded-full opacity-0"
	// PagesMakerClass is the "Made by" select.
	PagesMakerClass = "min-h-[44px] min-w-0 max-w-[14rem] shrink rounded-xl bg-s1 px-3 text-sm text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"
	// PagesFilterApplyClass is the no-script submit: hidden wherever a script
	// runs (the live request applies every change), drawn only where none
	// does.
	PagesFilterApplyClass = "hidden min-h-[44px] items-center rounded-xl bg-s2 px-3 text-sm font-medium text-fg ring-1 ring-inset ring-line [@media(scripting:none)]:inline-flex"
	// PagesNumberClass is a row's page number (#12), small and muted.
	PagesNumberClass = "tabular-nums text-fg2"
)

// PagesFilterView is the form's state: the box's text, the toggle (and how
// many pages it would list: 0 draws no number), the maker chosen ("" for
// anyone) and the makers to choose from (none, or one, draws no select —
// unless one is chosen, so the chosen one can be undone).
type PagesFilterView struct {
	Q            string
	Waiting      bool
	WaitingCount int
	By           string
	Makers       []string
}

// PagesFilterFormHTML is the form over the Pages list (action, its own
// address).
func PagesFilterFormHTML(action string, v PagesFilterView) string {
	esc := html.EscapeString
	checked := ""
	if v.Waiting {
		checked = " checked"
	}
	count := ""
	if v.WaitingCount > 0 {
		count = `<span class="` + AsksChipCountClass + `" data-pages-waiting-count>` + strconv.Itoa(v.WaitingCount) + `</span>`
	}
	out := `<form role="search" method="get" action="` + esc(action) + `" hx-boost="false" hx-history="false" class="` + PagesFilterFormClass + `" data-pages-filter` +
		` hx-get="` + esc(action) + `" hx-trigger="input delay:` + PagesSearchDebounce + `" hx-target="#` + PagesListRegionID + `" hx-swap="innerHTML" hx-sync="this:replace" hx-indicator="#pages-search-busy">` +
		`<div class="` + PagesSearchBoxClass + `">` +
		`<label for="` + PagesSearchInputID + `" class="sr-only">Search pages</label>` +
		`<span id="pages-search-busy" class="` + AsksSearchIconClass + `" aria-hidden="true">` + asksSearchIcons + `</span>` +
		`<input type="search" id="` + PagesSearchInputID + `" name="q" value="` + esc(v.Q) + `" placeholder="` + PagesSearchPlaceholder + `" maxlength="200" autocomplete="off" autocapitalize="off" spellcheck="false" enterkeyhint="search"` +
		` class="` + AsksSearchInputClass + `" aria-controls="` + PagesListRegionID + `" aria-keyshortcuts="/" data-pages-search-input></div>` +
		`<label class="` + PagesWaitingClass + `" data-pages-waiting><input type="checkbox" name="waiting" value="1" class="` + PagesWaitingInputClass + `"` + checked + ` data-pages-waiting-input>` +
		`<span>` + PagesWaitingLabel + `</span>` + count + `</label>`
	if len(v.Makers) > 1 || v.By != "" {
		out += `<select name="by" class="` + PagesMakerClass + `" aria-label="Made by" data-pages-maker><option value="">By anyone</option>`
		found := false
		for _, m := range v.Makers {
			sel := ""
			if m == v.By {
				sel, found = " selected", true
			}
			out += `<option value="` + esc(m) + `"` + sel + `>By ` + esc(m) + `</option>`
		}
		if !found && v.By != "" {
			out += `<option value="` + esc(v.By) + `" selected>By ` + esc(v.By) + `</option>`
		}
		out += `</select>`
	}
	return out + `<button type="submit" class="` + PagesFilterApplyClass + `" data-pages-filter-apply>Apply</button></form>`
}

// PagesListRegionHTML is the list region, holding inner (the list, its count
// line, or the no-match line).
func PagesListRegionHTML(inner string) string {
	return `<div id="` + PagesListRegionID + `" class="flex min-w-0 flex-col gap-2" data-pages-list-region>` + inner + `</div>`
}

// PagesFilterStatusHTML is the line over a filtered list: how many pages it
// shows (n > 0) or that none matches, and the way back to the whole list
// (clear).
func PagesFilterStatusHTML(n int, clear string) string {
	link := `<a href="` + html.EscapeString(clear) + `" class="` + AsksSearchClearClass + `" data-pages-clear>` + PagesClearText + `</a>`
	if n == 0 {
		return `<p class="` + AsksSearchStatusClass + `" role="status" data-pages-none><span>` + PagesNoMatchText + `</span> ` + link + `</p>`
	}
	return `<p class="` + AsksSearchStatusClass + `" role="status" data-pages-count="` + strconv.Itoa(n) + `"><span>` + plural(n, "page", "pages") + `</span> ` + link + `</p>`
}

// pagesSearchJS: "/" focuses the search box when focus is not already in a
// field (an input, a textarea, a select, an editable element). It listens in
// the CAPTURE phase and takes the key (preventDefault), so the palette's own
// "/" (ui/palette.go, which skips a key already taken) does not open over the
// list: on this screen "/" searches the pages.
var pagesSearchJS = `(function () {
  if (window.__pagesSearchInit) return;
  window.__pagesSearchInit = true;
  document.addEventListener('keydown', function (e) {
    if (e.key !== '/' || e.defaultPrevented || e.isComposing || e.altKey || e.ctrlKey || e.metaKey) return;
    var t = e.target;
    if (t && (t.isContentEditable || (t.closest && t.closest('input, textarea, select, [contenteditable=""], [contenteditable="true"]')))) return;
    if (document.querySelector('dialog[open]')) return;
    var f = document.getElementById('` + PagesSearchInputID + `');
    if (!f || !f.getClientRects().length) return;
    e.preventDefault();
    f.focus();
    try { f.setSelectionRange(f.value.length, f.value.length); } catch (err) {}
  }, true);
})();
`

var pagesSearchScriptSrc = staticJS("pages-search", pagesSearchJS)

// PagesSearchScriptHTML is the search's script tag: the Pages list writes it
// once.
func PagesSearchScriptHTML() string { return nodeString(staticScript(pagesSearchScriptSrc)) }
