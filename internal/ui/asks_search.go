package ui

import "html"

// THE ASKS PAGE'S SEARCH BOX (internal/api asks_search.go; docs/attention.md
// "Searching your asks"): on the page heading's row, a GET to the page
// itself with ?q=, so a search is a link, and Back leaves it. Its classes live
// here because the stylesheet scans internal/ui, not internal/api.

const (
	// AsksSearchFormClass is the box: the field, then its button, at the END
	// OF THE PAGE HEADING'S ROW (beside "Asks · N open"), taking the rest of
	// it (on a wide screen at most max-w-sm). Its 44px controls overhang that
	// 24px row by 6px each way (-my-1.5): the row grows by 8px, so the feed
	// starts 8px lower than without it — e2e asks-feed's "2.5 cards above the
	// fold" at 390×844 still holds — and the overhang leaves 6px to the tabs.
	AsksSearchFormClass = "-my-1.5 ml-auto flex min-w-0 flex-1 items-center gap-2 sm:max-w-sm"
	// AsksSearchInputClass is the field.
	AsksSearchInputClass = "min-h-[44px] min-w-0 flex-1 rounded-xl bg-s1 px-3 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-fg2 focus:outline-none focus:ring-2 focus:ring-focus sm:text-sm"
	// AsksSearchButtonClass is its Search button, an icon (a 44px target).
	AsksSearchButtonClass = AsksIconButtonClass + " ring-1 ring-inset ring-edge"
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

// asksSearchIcon is the Search button's glyph.
const asksSearchIcon = `<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>`

// AsksSearchFormHTML is the search box over the Asks page (action), holding q.
// A plain GET with no boost: the server may answer it with a redirect to the
// one ask an id names, and a full navigation keeps that out of the history.
func AsksSearchFormHTML(action, q string) string {
	esc := html.EscapeString
	return `<form role="search" method="get" action="` + esc(action) + `" hx-boost="false" class="` + AsksSearchFormClass + `" data-asks-search>` +
		`<label for="asks-search-q" class="sr-only">Search asks by id or text</label>` +
		`<input type="search" id="asks-search-q" name="q" value="` + esc(q) + `" placeholder="` + AsksSearchPlaceholder + `" maxlength="200" autocomplete="off" autocapitalize="off" spellcheck="false" enterkeyhint="search" class="` + AsksSearchInputClass + `" data-asks-search-input>` +
		`<button type="submit" class="` + AsksSearchButtonClass + `" aria-label="Search" title="Search" data-asks-search-submit>` + asksSearchIcon + `</button></form>`
}
