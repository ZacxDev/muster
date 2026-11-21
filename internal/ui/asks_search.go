package ui

import "html"

// THE ASKS PAGE'S SEARCH BOX (internal/api asks_search.go; docs/attention.md
// "Searching your asks"): on the page heading's row, a GET to the page
// itself with ?q=, so a search is a link, and Back leaves it. Its classes live
// here because the stylesheet scans internal/ui, not internal/api.
//
// IT SEARCHES AS YOU TYPE (operator decision: "the asks search should auto
// apply (debounced), no search button"). The field asks the page for its
// results AsksSearchDebounce after the last input (its clear button's too),
// and they replace the results region in place: no navigation, so focus and
// caret stay in the field. A newer request replaces one still in flight
// (hx-sync this:replace), so a slow earlier answer never lands over a newer
// one. The server answers that request with the results alone — NEVER the
// redirect an id earns on a real submit (typing 12 on the way to 123 must not
// open ask 12) — and the address to show (HX-Replace-Url: typing adds no
// history entries). Enter is still the form's own submit, and stands the live
// search down (asksSearchJS): an id that finds one thing opens it, and with no
// script that is the whole search. NOT the `search` event as a trigger: Chrome
// fires it on Enter too, racing the submit.

const (
	// AsksSearchFormClass is the box: the field alone (no button: it searches
	// as you type), at the END OF THE PAGE HEADING'S ROW (beside "Asks · N
	// open"), taking the rest of it (on a wide screen at most max-w-sm). Its
	// 44px field overhangs that 24px row by 6px each way (-my-1.5): the row
	// grows by 8px, so the feed starts 8px lower than without it — e2e
	// asks-feed's "2.5 cards above the fold" at 390×844 still holds — and the
	// overhang leaves 6px to the tabs. Relative: the field's icon sits in it.
	AsksSearchFormClass = "relative -my-1.5 ml-auto flex min-w-0 flex-1 items-center sm:max-w-sm"
	// AsksSearchInputClass is the field, padded on the left for its icon.
	AsksSearchInputClass = "min-h-[44px] min-w-0 flex-1 rounded-xl bg-s1 pl-9 pr-3 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-fg2 focus:outline-none focus:ring-2 focus:ring-focus sm:text-sm"
	// AsksSearchIconClass is the field's icon, inside its left edge: the
	// magnifier, which a spinner replaces while a search is in flight (htmx
	// puts htmx-request on it then, and htmx's own htmx-indicator rule shows
	// the spinner). Decorative: the results' count line is what is announced.
	AsksSearchIconClass = "group pointer-events-none absolute inset-y-0 left-3 flex items-center text-fg2"
	// AsksSearchRegionClass is the results region: no box of its own (so,
	// empty, it adds no gap to the page's column). While it holds results,
	// the stylesheet hides the page's tabs after it (web/css/input.css,
	// [data-asks-search-region]:not(:empty) ~ [data-asks-main]); emptied, the
	// page is back as it was, with no reload.
	AsksSearchRegionClass = "contents"
	// AsksMainClass wraps the page's tabs (and what goes over them) after the
	// results region, with no box of its own either.
	AsksMainClass = "contents"
	// AsksSearchStatusClass is the line over the results: how many, and the
	// way back to the page.
	AsksSearchStatusClass = "flex min-w-0 flex-wrap items-center gap-x-2 px-1 text-sm text-fg2"
	// AsksSearchClearClass is that line's "Clear" link.
	AsksSearchClearClass = "inline-flex min-h-[44px] items-center font-medium text-accent underline-offset-2 hover:underline"
	// AsksSearchOpenClass is a result's head: the link to the ask (or the
	// draft), its title over its state and age.
	AsksSearchOpenClass = "press -m-1 flex min-h-[44px] min-w-0 flex-1 flex-col justify-center rounded-lg p-1 transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// AsksSearchTextClass is a result's text (the question, the post), clamped.
	AsksSearchTextClass = "asks-clamp whitespace-pre-wrap break-words text-sm leading-snug text-fg"
	// AsksSearchAnswerClass is a result's answer line.
	AsksSearchAnswerClass = "asks-clamp-1 break-words text-xs text-fg2"
	// AsksSearchTopClass is a result's top row: its head, then its id chip.
	AsksSearchTopClass = "flex min-w-0 items-center gap-2"
)

// AsksSearchPlaceholder is the box's hint.
const AsksSearchPlaceholder = "Id or words"

// AsksSearchRegionID is the results region's id: the live search's hx-target,
// and the HX-Target header by which the server tells that request from a
// navigation (internal/api asksSearchLive).
const AsksSearchRegionID = "asks-search-region"

// AsksSearchDebounce is how long the field waits after the last keystroke
// before it searches.
const AsksSearchDebounce = "300ms"

// asksSearchIcons are the field's icon: the magnifier, and the spinner drawn
// in its place while a search is in flight.
const asksSearchIcons = `<svg class="h-4 w-4 transition-opacity group-[.htmx-request]:opacity-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>` +
	`<svg class="htmx-indicator absolute h-4 w-4 animate-spin motion-reduce:animate-none" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true" focusable="false" data-asks-search-spinner><path d="M21 12a9 9 0 1 1-6.2-8.6"/></svg>`

// AsksSearchFormHTML is the search box over the Asks page (action), holding q.
// Enter is a plain GET with no boost: the server may answer it with a
// redirect to the one ask an id names, and a full navigation keeps that out of
// the history. Typing is the field's own debounced htmx request (the file's
// header), whose results replace the region AsksSearchRegionHTML draws.
func AsksSearchFormHTML(action, q string) string {
	esc := html.EscapeString
	return `<form role="search" method="get" action="` + esc(action) + `" hx-boost="false" class="` + AsksSearchFormClass + `" data-asks-search>` +
		`<label for="asks-search-q" class="sr-only">Search asks by id or text</label>` +
		`<span id="asks-search-busy" class="` + AsksSearchIconClass + `" aria-hidden="true" data-asks-search-busy>` + asksSearchIcons + `</span>` +
		`<input type="search" id="asks-search-q" name="q" value="` + esc(q) + `" placeholder="` + AsksSearchPlaceholder + `" maxlength="200" autocomplete="off" autocapitalize="off" spellcheck="false" enterkeyhint="search" class="` + AsksSearchInputClass + `"` +
		` hx-get="` + esc(action) + `" hx-trigger="input changed delay:` + AsksSearchDebounce + `" hx-target="#` + AsksSearchRegionID + `" hx-swap="innerHTML" hx-sync="this:replace" hx-indicator="#asks-search-busy" aria-controls="` + AsksSearchRegionID + `" data-asks-search-input></form>`
}

// AsksSearchRegionHTML is the results region, holding results (the server's
// search results; "" for none) — and NOTHING between its tags when empty,
// whitespace included: the stylesheet's :empty is what shows the tabs.
func AsksSearchRegionHTML(results string) string {
	return `<div id="` + AsksSearchRegionID + `" class="` + AsksSearchRegionClass + `" data-asks-search-region>` + results + `</div>`
}

// asksSearchJS is the box's two pieces of glue, delegated on the document.
//
//   - ENTER STANDS THE LIVE SEARCH DOWN. The form's submit navigates (and an
//     id that finds one thing redirects to it); a live request still pending
//     or in flight could otherwise land after the submit began and
//     replaceState the page's address to ?q=<the id> — and Back from the ask
//     would come back to that address, which redirects to the ask again.
//     So a submit aborts the request in flight, refuses any later one until
//     the page is shown again (pageshow: Back from the bfcache), and takes
//     ?q= off the entry it leaves (one the typing already set, the same way).
//   - htmx's HISTORY SNAPSHOT keeps the field's text. htmx saves the page's
//     HTML when a boosted link leaves it, and Back restores that HTML: the
//     results a live search drew are in it, but what was TYPED is the field's
//     value property, not its attribute, so without this Back would draw those
//     results under a box holding whatever the page was first loaded with.
var asksSearchJS = `(function () {
  if (window.__asksSearchInit) return;
  window.__asksSearchInit = true;
  var submitted = false;
  function field() { return document.getElementById('asks-search-q'); }
  document.addEventListener('submit', function (e) {
    var f = field();
    if (!f || e.target !== f.form) return;
    submitted = true;
    if (window.htmx) window.htmx.trigger(f, 'htmx:abort');
    // The entry the submit leaves is the page as it was, not the typed
    // search: typed, the address became ?q=<what was typed>, and an id there
    // redirects — Back from the ask it opens would open it again.
    try {
      var u = new URL(window.location.href);
      if (u.searchParams.has('q')) {
        u.searchParams.delete('q');
        history.replaceState(history.state, '', u.pathname + u.search + u.hash);
      }
    } catch (err) {}
  }, true);
  document.addEventListener('htmx:beforeRequest', function (e) {
    if (submitted && e.target === field()) e.preventDefault();
  });
  window.addEventListener('pageshow', function () { submitted = false; });
  document.addEventListener('htmx:beforeHistorySave', function () {
    var f = field();
    if (f) f.setAttribute('value', f.value);
  });
})();
`

var asksSearchScriptSrc = staticJS("asks-search", asksSearchJS)

// AsksSearchScriptHTML is the search box's script tag: the Asks page writes
// it once.
func AsksSearchScriptHTML() string { return nodeString(staticScript(asksSearchScriptSrc)) }
