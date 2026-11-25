package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE COMMAND PALETTE: a search box over the page that finds any session, as
// it is typed — by its tmux codename ("wheat", "mango:2") or its title (its
// display name and CURRENT focus title), then by a PREVIOUS focus title
// (operator ask: "find agents by their focus titles, either current or
// previous … search based on what it's working on instead of the exact name
// of the session"), and, ranked AFTER all of those, by its repository, its
// kind ("Claude Code", "opencode", "shell") and a window's raw tmux SESSION
// name ("scratch11"), each a plain word (operator ask: "Teach the palette
// repo+kind"; no field tokens). The rule is paletteMatchJS's.
//
// 🔴 ONE PALETTE, TWO WAYS IN (operator ask: "a search icon button [in the
// sidebar] that opens the same command palette UI that the bottom bar search
// opens"). It is drawn ONCE per document, in the sidebar's root (sidebarIn) —
// every page with the session switcher has the sidebar too — and opened by
// any button carrying paletteOpenAttr: the session switcher's magnifier and
// the sidebar's (sidebarSearchButton). "/" outside a text field opens it as
// well, wherever one of those buttons is on screen.
//
// WHAT IT SEARCHES is read from the page first, with no request: the session
// switcher's All row when the page has the bar (its entries' data-switcher-
// code, -title, -prev and -words), otherwise the sidebar's agent rows, which
// carry the same four attributes (navAgentRow, from the same switcherItems).
// The RECENT previous titles reach the page with it, from one history read
// for every entry (internal/api's withPreviousTitles). Then, once typing
// pauses on a long enough query, the server is asked for every title in the
// retention (PaletteHistoryRoute), and its hits are merged into entries on
// the page (owner answer: "Recent in-page + full on typing"). The values are
// attributes the server escapes, read with getAttribute, and a hit is JSON;
// all are drawn with textContent. The page's own session is left out either
// way. With the viewer's "Show tmux sessions" on, a named window's pill or row
// also carries data-switcher-tmux (entryTmuxAttr), which its result draws as
// a muted line ("mango:1 @ atlas"); it is drawn, never searched.
//
// 🔴 IT SEARCHES EVERYTHING, NOT ONLY SESSIONS (operator ask: "by default we
// should search everything and then we can use the pills to search for a
// specific type"). Four kinds of result, each labelled with its kind:
//
//   - SESSIONS, as above, ranked as before;
//   - SCREENS, the app's own pages (the nav list, nav_pages.go — the one list
//     the sidebar draws its links from), matched on the page by their label,
//     their route's words and their own words ("pages", "asks", "settings");
//   - ASKS and PAGES (the shareable documents), from the server once typing
//     pauses (PaletteFindRoute): the caller's own asks by id ("1001", "#1001")
//     or text, and the pages their Pages screen lists, by id or title.
//
// A row of TYPE PILLS (All, Sessions, Screens, Asks, Pages — a radio group,
// All each time it opens) narrows the list to one kind. "Screens" and
// "Pages" are the app's word for each: muster calls its shareable documents
// Pages (the sidebar's Pages link), so an app page is a screen.
//
// 🔴 A POPOVER (the top layer), so opening it moves nothing on the page. It
// sits in #sidebar-root, OUTSIDE the sliding #sidebar panel: the panel is
// moved by a transform, and the palette must not be part of what moves.
//
// 🔴 ON A PHONE THE BOX IS AT THE BOTTOM, THE INPUT LAST (operator ask: "move
// the search input to the bottom of the component instead of the top so it's
// less of a reach"): below the sm breakpoint the box sits on the screen's
// bottom edge — above a soft keyboard that only overlays the page, measured
// from the visual viewport (keyboardViewportJS) into --palette-kb — with the
// input at its foot, the pills above it and the results above those, the best
// match nearest the input. From sm up it is the box near the top it was.

// The palette's ids, and the attribute that makes a button open it.
const (
	paletteID        = "switcher-search"
	paletteInputID   = "switcher-search-input"
	paletteResultsID = "switcher-search-results"
	paletteOpenAttr  = "data-palette-open"
	// paletteTypesID is the type pills' group; palettePagesAttr marks the
	// <template> holding the screens the palette searches.
	paletteTypesID   = "switcher-search-types"
	palettePagesAttr = "data-palette-pages"
	// paletteWaitMarkAttr marks a switcher pill's <template> holding the
	// entry's waiting mark (entryWaitMark), which the palette's row copies.
	paletteWaitMarkAttr = "data-wait-mark"
)

// PaletteHistoryRoute is the palette's server search (internal/api's
// handlePaletteHistory): every focus title in the retention, asked for once
// typing pauses on a query of PaletteServerMinRunes or more.
const PaletteHistoryRoute = "/ui/palette/history"

// PaletteFindRoute is the palette's search of the caller's asks and Pages
// documents (internal/api's handlePaletteFind), asked for once typing pauses
// on any non-empty query: the server decides what it searches (an id, or text
// of PaletteServerMinRunes or more) and answers the rest empty without a read.
const PaletteFindRoute = "/ui/palette/find"

// The palette's server search waits for typing to pause this long, and asks
// only for a query this long (focus.MinTitleQueryRunes, the server's own floor).
// PaletteServerMinRunes must equal focus.MinTitleQueryRunes
// (internal/api's TestThePaletteAsksOnlyWhatTheServerAnswers), and
// paletteServerMinRunesJS spell it (TestThePaletteSpellsItsServerFloor): the
// script is a constant, so it holds the number as text.
const (
	paletteServerDebounceMS = "275"
	PaletteServerMinRunes   = 3
	paletteServerMinRunesJS = "3"
)

// SidebarSearchID is the sidebar's magnifier (sidebarSearchButton).
const SidebarSearchID = "sidebar-search"

// searchIconSVG is the magnifier (the 20px solid set the bar's other icon is from).
const searchIconSVG = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clip-rule="evenodd"/></svg>`

// paletteTypes are the type pills, in order: the value each filters to
// (data-palette-type; a result's data-type) and its label. "all" first, the
// default.
var paletteTypes = []struct{ Key, Label string }{
	{"all", "All"},
	{"session", "Sessions"},
	{"screen", "Screens"},
	{"ask", "Asks"},
	{"page", "Pages"},
}

// paletteBox is the palette: a box over the page with an input, the type
// pills and the list of what it finds, filled by paletteJS as the operator
// types. Sessions are ranked by paletteMatchJS: the codename, then the title
// (current focus), then a previous focus title, then the words (repository,
// kind, tmux session); folded; ties in the source's order — and the other
// kinds among them by musterPaletteAll. Arrows move, Enter opens the
// highlighted result (the best, until the arrows move), Escape closes.
//
// 🔴 THE DOM ORDER IS THE DESKTOP'S — input, pills, results, the empty line —
// and a phone's is its REVERSE (flex-col-reverse below sm, the results list
// reversed too), so the input is at the foot, the pills above it, and the best
// match just above the pills. One markup, so the combobox, the radio group and
// the listbox are the same elements at every width.
//
// pages are the screens it searches (navPages: the sidebar's own list).
func paletteBox(pages []navPage) g.Node {
	return Div(
		ID(paletteID),
		g.Attr("popover", ""),
		g.Attr("role", "dialog"),
		g.Attr("aria-label", "Search"),
		// A phone: on the bottom edge, above an overlaying keyboard
		// (--palette-kb, paletteJS). sm and up: near the top, as before.
		Class("inset-x-0 top-auto bottom-[calc(var(--palette-kb,0px)+max(0.5rem,env(safe-area-inset-bottom)))] sm:bottom-auto sm:top-[max(0.5rem,10vh)] mx-auto my-0 w-[min(36rem,calc(100vw-1rem))] overflow-hidden rounded-xl border-0 bg-s1 p-0 text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/40"),
		Div(
			Class("flex flex-col-reverse sm:flex-col"),
			Div(
				g.Attr("data-palette-field", ""),
				Class("flex items-center gap-2 border-t border-line px-3 text-muted sm:border-b sm:border-t-0"),
				g.Raw(searchIconSVG),
				Input(
					ID(paletteInputID),
					Type("text"),
					g.Attr("role", "combobox"),
					g.Attr("aria-expanded", "true"),
					g.Attr("aria-controls", paletteResultsID),
					g.Attr("aria-autocomplete", "list"),
					g.Attr("aria-label", "Search sessions, screens, asks and pages"),
					g.Attr("autocomplete", "off"),
					g.Attr("autocapitalize", "off"),
					g.Attr("autocorrect", "off"),
					g.Attr("spellcheck", "false"),
					g.Attr("enterkeyhint", "go"),
					g.Attr("placeholder", "Search…"),
					// text-base: 16px, so a phone does not zoom the page on focus.
					Class("h-12 min-w-0 flex-1 bg-transparent text-base text-fg outline-none placeholder:text-muted"),
				),
				Kbd(Class("hidden rounded border border-line px-1.5 text-[11px] text-muted sm:inline"), g.Text("Esc")),
			),
			// The type pills: a radio group, one tab stop (the checked pill),
			// the arrows moving between them (paletteJS).
			Div(
				ID(paletteTypesID),
				g.Attr("role", "radiogroup"),
				g.Attr("aria-label", "Search in"),
				Class("flex gap-1 overflow-x-auto px-2 [scrollbar-width:none]"),
				g.Map(paletteTypes, func(t struct{ Key, Label string }) g.Node {
					on := t.Key == "all"
					return Button(
						Type("button"),
						g.Attr("role", "radio"),
						g.Attr("aria-checked", boolAttr(on)),
						g.Attr("tabindex", map[bool]string{true: "0", false: "-1"}[on]),
						g.Attr("data-palette-type", t.Key),
						// A 44px target around a smaller pill.
						Class("group flex min-h-[44px] shrink-0 items-center px-0.5 outline-none"),
						Span(Class("rounded-full px-3 py-1 text-xs font-medium text-fg2 ring-1 ring-inset ring-line transition group-hover:text-fg group-focus-visible:ring-2 group-focus-visible:ring-accent group-aria-checked:bg-accent/15 group-aria-checked:text-fg group-aria-checked:ring-accent/40"), g.Text(t.Label)),
					)
				}),
			),
			Ul(
				ID(paletteResultsID),
				g.Attr("role", "listbox"),
				g.Attr("aria-label", "Results"),
				Class("flex max-h-[min(24rem,calc(100dvh-var(--palette-kb,0px)-9rem))] flex-col-reverse overflow-y-auto overscroll-contain py-1 sm:max-h-[min(24rem,60vh)] sm:flex-col"),
			),
			P(g.Attr("data-switcher-search-empty", ""), g.Attr("hidden", ""), Class("px-4 py-3 text-sm text-muted"), g.Text("No matches")),
		),
		palettePagesTemplate(pages),
	)
}

// palettePagesTemplate is the screens the palette searches, inert: one link
// per nav page — its route, label, icon and words — read by paletteJS.
func palettePagesTemplate(pages []navPage) g.Node {
	return Template(g.Attr(palettePagesAttr, ""), g.Map(pages, func(p navPage) g.Node {
		return A(Href(p.Href), g.Attr("data-palette-page", p.Entry), g.Attr("data-palette-icon", p.Icon), g.Attr("data-palette-words", p.Words), g.Text(p.Label))
	}))
}

// sidebarSearchButton is the sidebar header's magnifier: it opens the palette
// (paletteOpenAttr). A 44px target, beside the close button.
func sidebarSearchButton() g.Node {
	return Button(
		ID(SidebarSearchID),
		Type("button"),
		g.Attr(paletteOpenAttr, ""),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-controls", paletteID),
		g.Attr("aria-keyshortcuts", "/"),
		g.Attr("aria-label", "Search"),
		g.Attr("title", "Search (/)"),
		Class("press inline-flex h-11 w-11 min-h-[44px] min-w-[44px] items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clip-rule="evenodd"/></svg>`),
	)
}

// paletteMatchJS is the palette's RANK, window.musterPaletteMatch(entry, q):
// what entry {code, title, prev, words} is found by for query q, as
// {r, why, hit} — r the rank (lower first), why the field it matched
// ("code", "title", "previous", "words"), hit the previous title it matched —
// or null for no match. Pure (no DOM), so e2e/tests/focus.spec.ts runs it over
// a table as well as through the real palette.
//
// 🔴 THE RULE — the palette's original substring ranks, with ONE rank added
// for a previous focus title, between the title and the words (so a name
// always beats a focus title, and a CURRENT focus title a PREVIOUS one):
//
//	0  the codename starts with the query
//	1  the codename contains it
//	2  the title — display name and CURRENT focus title — contains it
//	3  a PREVIOUS focus title contains it (in-page, or the server search's)
//	4  the words (repository, kind, tmux session, host) contain it
//
// Case-folded (lower case), the query trimmed; ties keep the source's order
// (refresh sorts by r, then by position). An empty query matches everything
// at 0. hit is the NEWEST previous title that contains the query (prev is
// newest first).
const paletteMatchJS = `  window.musterPaletteMatch = function (e, q) {
    function low(s) { return String(s || '').toLowerCase(); }
    q = low(q).trim();
    if (!q) return { r: 0, why: '' };
    var c = low(e.code);
    if (c.indexOf(q) === 0) return { r: 0, why: 'code' };
    if (c.indexOf(q) > 0) return { r: 1, why: 'code' };
    if (low(e.title).indexOf(q) >= 0) return { r: 2, why: 'title' };
    var prev = e.prev || [];
    for (var i = 0; i < prev.length; i++) if (low(prev[i]).indexOf(q) >= 0) return { r: 3, why: 'previous', hit: prev[i] };
    if (low(e.words).indexOf(q) >= 0) return { r: 4, why: 'words' };
    return null;
  };
  // window.musterPaletteEntry is element a (a switcher pill, or a sidebar
  // agent row) as the entry musterPaletteMatch reads, n its place: what the
  // search finds it by, from the pill's data-switcher-* attributes. ONE
  // reading of them, for the palette and the switcher dock's filter alike
  // (switcher_dock.go), so the two match the same fields.
  window.musterPaletteEntry = function (a, n) {
    // data-switcher-name is the entry's NAME; its title is the tooltip,
    // which also carries the technical id a human name hides (SwitcherItem.Tip).
    var label = a.getAttribute('data-switcher-name') || a.getAttribute('title') || a.textContent;
    return { href: a.getAttribute('href'), label: label, code: a.getAttribute('data-switcher-code') || label,
      title: a.getAttribute('data-switcher-title') || '', words: a.getAttribute('data-switcher-words') || '',
      prev: (a.getAttribute('data-switcher-prev') || '').split('\n').filter(Boolean),
      age: a.getAttribute('data-switcher-focus-age') || '', stale: a.hasAttribute('data-switcher-focus-stale'),
      kind: a.getAttribute('data-switcher-entry') || a.getAttribute('data-nav-agent') || (a.hasAttribute('data-nav-officer') ? 'officer' : ''), n: n, mark: window.musterWaitMark(a),
      // "Show tmux sessions": the place the server set (entryTmuxAttr), drawn
      // by the palette's row; never searched (the codename is, as code).
      tmux: a.getAttribute('data-switcher-tmux') || '' };
  };
  // window.musterWaitMark is element a's WAITING MARK — the sidebar's
  // (entryWaitMark): a switcher pill keeps it inert in a <template>, a sidebar
  // row draws it — or null when the entry waits on nothing. The server decided
  // it (listEntry.waiting); nothing here re-derives who is waiting.
  window.musterWaitMark = function (a) {
    var t = a.querySelector('template[` + paletteWaitMarkAttr + `]');
    if (t) return t.content.firstElementChild;
    return a.querySelector('[data-nav-attention], [data-nav-wait-dot]');
  };
  // window.musterPaletteRank is the palette's whole list for query q: each of
  // entries (in page order) that musterPaletteMatch matches, plus the server
  // search's hits ({href, title}) for q merged in, best first, ties in page
  // order — as items {e, r, why, hit}. A hit names an entry by its href; one
  // whose entry is not in entries (the page's own session) is dropped, and an
  // entry is listed once. Pure (no DOM): e2e/tests/focus.spec.ts runs it over a
  // table.
  window.musterPaletteRank = function (entries, q, hits) {
    var out = [], byHref = {};
    for (var i = 0; i < entries.length; i++) {
      var e = entries[i], m = window.musterPaletteMatch(e, q);
      if (!byHref[e.href]) byHref[e.href] = { e: e, n: i, item: null };
      if (m && !byHref[e.href].item) {
        var it = { e: e, n: i, r: m.r, why: m.why, hit: m.hit };
        byHref[e.href].item = it;
        out.push(it);
      }
    }
    (hits || []).forEach(function (h) {
      var b = h && byHref[h.href];
      if (!b) return;
      var title = String(h.title || '');
      if (b.item) {
        // 🔴 A SERVER HIT CAN ONLY RAISE AN ENTRY: one the page matched less
        // well (by its words alone) moves up to the previous-title rank and
        // says why; one it matched as well or better keeps its own match.
        if (b.item.r > 3) { b.item.r = 3; b.item.why = 'previous'; b.item.hit = title; }
        return;
      }
      b.item = { e: b.e, n: b.n, r: 3, why: 'previous', hit: title };
      out.push(b.item);
    });
    out.sort(function (a, b) { return a.r - b.r || a.n - b.n; });
    return out;
  };
  // window.musterPaletteScreenMatch is what screen p {href, label, words}
  // (a nav page, nav_pages.go) is found by for query q: {r, why} or null.
  // EVERY word of the query must START a word of the screen's — its label,
  // its route's segments, its own words — so "pages" finds Pages, "set"
  // finds the settings screens, and the letters inside a word ("ge") find
  // nothing. Ranked among the sessions' ranks (musterPaletteMatch):
  //
  //	0.5  the label has them all (after a codename's prefix, before its
  //	     substring: "asks" puts Asks just under a session named asks-…)
  //	4.5  the route or the words have them (after a session's words)
  //
  // An empty query matches every screen at 0. Pure (no DOM).
  window.musterPaletteScreenMatch = function (p, q) {
    function words(s) { return String(s || '').toLowerCase().split(/[^\p{L}\p{N}]+/u).filter(Boolean); }
    function starts(ws, w) { for (var i = 0; i < ws.length; i++) if (ws[i].indexOf(w) === 0) return true; return false; }
    var qs = words(q);
    if (!qs.length) return { r: 0, why: '' };
    var label = words(p.label), all = label.concat(words(p.href), words(p.words)), inLabel = true;
    for (var i = 0; i < qs.length; i++) {
      if (!starts(all, qs[i])) return null;
      if (!starts(label, qs[i])) inLabel = false;
    }
    return inLabel ? { r: 0.5, why: 'label' } : { r: 4.5, why: 'route' };
  };
  // window.musterPaletteAll is the palette's whole list of every kind, best
  // first, as items {t, href, r, n, …}: t the kind ("session", "screen",
  // "ask", "page"), and its source — it (a session's musterPaletteRank item),
  // p (a screen), a (an ask), d (a page). sessions are musterPaletteRank's
  // items, screens the nav pages, found the server's {asks, pages} for q;
  // type is the pill ("all", or one kind: only that kind is listed).
  //
  // 🔴 THE RANKS ARE ONE SCALE: an ask named by its id (-2), then a page
  // named by its id (-1), then the sessions' and the screens' ranks
  // interleaved (musterPaletteScreenMatch), then asks found by their text (5),
  // then pages by their title (6); ties by kind (ask, page, session, screen),
  // then by each source's own order. Under All a screen is listed only for a
  // query, at most CAP of them (the server cuts asks and pages to its own
  // cap); under its own pill every screen that matches. Pure (no DOM).
  window.musterPaletteAll = function (sessions, screens, found, q, type) {
    var CAP = 5, KIND = { ask: 0, page: 1, session: 2, screen: 3 };
    var all = !type || type === 'all', qt = String(q || '').trim(), out = [];
    function want(t) { return all || type === t; }
    function order(x, y) { return x.r - y.r || KIND[x.t] - KIND[y.t] || x.n - y.n; }
    if (want('session')) (sessions || []).forEach(function (it) { out.push({ t: 'session', href: it.e.href, r: it.r, n: it.n, it: it }); });
    if (want('screen') && (qt || !all)) {
      var sc = [];
      (screens || []).forEach(function (p, i) {
        var m = window.musterPaletteScreenMatch(p, qt);
        if (m) sc.push({ t: 'screen', href: p.href, r: m.r, n: i, why: m.why, p: p });
      });
      sc.sort(order);
      out = out.concat(all ? sc.slice(0, CAP) : sc);
    }
    found = found || {};
    if (want('ask')) (found.asks || []).forEach(function (a, i) { out.push({ t: 'ask', href: a.href, r: a.byId ? -2 : 5, n: i, a: a }); });
    if (want('page')) (found.pages || []).forEach(function (d, i) { out.push({ t: 'page', href: d.href, r: d.byId ? -1 : 6, n: i, d: d }); });
    out.sort(order);
    return out;
  };
`

// paletteJS is the palette's script, part of appScript's static file (every
// page with the sidebar loads it). It binds ONCE PER WINDOW, at the document:
// a boosted navigation swaps the body, palette and buttons with it, so every
// element is looked up when it is used, and the palette's own listeners are
// bound to each palette element once (bindPalette).
const paletteJS = `
(function () {
  if (window.__musterPalette) return;
  window.__musterPalette = true;
` + paletteMatchJS + `  var PAL = '` + paletteID + `', INPUT = '` + paletteInputID + `', RESULTS = '` + paletteResultsID + `';
  var OPEN = '[` + paletteOpenAttr + `]', TYPES = '` + paletteTypesID + `';
  // chosen: the operator moved the highlight (an arrow) since the list was
  // last started over (typing, a pill). Only then is it THEIR selection, for
  // a redraw to keep (render).
  var shown = [], active = -1, chosen = false, opener = null, type = 'all';
  function byId(id) { return document.getElementById(id); }
  function isOpen(p) { try { return !!p && p.matches(':popover-open'); } catch (e) { return false; } }
  function hide(p) { try { if (isOpen(p)) p.hidePopover(); } catch (e) {} }
  // The entries searched: the switcher's All row when the page has the bar
  // (a bfcache restore redraws it: restore.go), else the sidebar's agent
  // rows and its Officers rows (an officer's row stands in for its instance
  // there, nav_officers.go); the page's own session left out of both.
  function entries() {
    var es = document.querySelectorAll('#` + SwitcherID + ` [data-switcher-list="all"] a[data-switcher-entry]');
    if (!es.length) es = document.querySelectorAll('#` + NavAgentsListID + ` a[data-nav-agent]:not([data-nav-current]), #` + NavAgentsListID + ` a[data-nav-officer]:not([data-nav-current])');
    var out = [];
    for (var i = 0; i < es.length; i++) out.push(window.musterPaletteEntry(es[i], i));
    return out;
  }
  // The screens searched: the nav list's pages (nav_pages.go), from the
  // palette's own inert template — the sidebar's links, as the server gated
  // them for this viewer.
  function screens() {
    var t = document.querySelector('#' + PAL + ' template[` + palettePagesAttr + `]'), out = [];
    if (!t) return out;
    var as = t.content.querySelectorAll('a[data-palette-page]');
    for (var i = 0; i < as.length; i++) {
      out.push({ href: as[i].getAttribute('href'), label: as[i].textContent, icon: as[i].getAttribute('data-palette-icon') || '',
        words: as[i].getAttribute('data-palette-words') || '' });
    }
    return out;
  }
  function el(tag, cls, text) { var x = document.createElement(tag); if (cls) x.className = cls; if (text != null) x.textContent = text; return x; }
` + focusAgeLineJS + keyboardViewportJS + `
  // fitKeyboard keeps a phone's box above a keyboard that only overlays the
  // page (--palette-kb: the layout viewport below the visual one); 0 where
  // the keyboard resizes the page, or under a pinch zoom.
  function fitKeyboard() {
    var pal = byId(PAL);
    if (!pal) return;
    var m = kbMeasure(kbWin());
    pal.style.setProperty('--palette-kb', (m ? m.kb : 0) + 'px');
  }
  // flipped: the list runs bottom-up (a phone's, flex-col-reverse), so the
  // arrow that moves to the NEXT result is Up.
  function flipped() {
    try { return getComputedStyle(byId(RESULTS)).flexDirection === 'column-reverse'; } catch (e) { return false; }
  }
  function mark() {
    var input = byId(INPUT), opts = byId(RESULTS).children;
    for (var i = 0; i < opts.length; i++) {
      var on = i === active;
      opts[i].setAttribute('aria-selected', on ? 'true' : 'false');
      opts[i].classList.toggle('bg-s3', on);
    }
    if (active >= 0 && opts[active]) {
      input.setAttribute('aria-activedescendant', opts[active].id);
      opts[active].scrollIntoView({ block: 'nearest' });
    } else input.removeAttribute('aria-activedescendant');
  }
  // THE SERVER SEARCHES, once typing pauses for DEBOUNCE ms:
  //
  //   - the focus HISTORY (internal/api's handlePaletteHistory), for a query
  //     of MIN or more characters: every focus title in the retention, its
  //     hits merged as previous-title matches (rank 3) of entries ALREADY on
  //     the page, by window.musterPaletteRank — an entry is listed once (a hit
  //     raises one the page matched only by its words, and leaves one it
  //     matched better), and a hit whose entry is not on the page (the page's
  //     own session) is dropped;
  //   - the caller's ASKS AND PAGES (handlePaletteFind), for ANY non-empty
  //     query: {asks, pages}, each ranked and cut by the server. The server
  //     alone decides what is searchable (an id however short, text of its
  //     floor or more) and answers anything else empty without a read, so no
  //     copy of its rule lives here.
  //
  // srv is the query they answer and what each answered.
  //
  // 🔴 ONLY THE ANSWER TO THE CURRENT QUERY LANDS. Each new query cancels the
  // pending timer and aborts the requests in flight, AND an answer is applied
  // only if its sequence number is the latest and the input still holds its
  // query — so an out-of-order response can never overwrite newer results.
  var ROUTE = '` + PaletteHistoryRoute + `', FIND = '` + PaletteFindRoute + `', DEBOUNCE = ` + paletteServerDebounceMS + `, MIN = ` + paletteServerMinRunesJS + `;
  var srv = { q: '', hits: [], asks: [], pages: [] }, seq = 0, timer = 0, ctl = null;
  function ask(q) {
    clearTimeout(timer);
    seq++;
    if (ctl) { try { ctl.abort(); } catch (e) {} ctl = null; }
    if (!q) return;
    var hist = Array.from(q).length >= MIN;
    var mine = seq;
    timer = setTimeout(function () {
      if (mine !== seq) return;
      var c = typeof AbortController === 'function' ? new AbortController() : null;
      ctl = c;
      function get(route, apply) {
        fetch(route + '?q=' + encodeURIComponent(q), { credentials: 'same-origin', headers: { Accept: 'application/json' }, signal: c ? c.signal : undefined })
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (d) {
            var input = byId(INPUT);
            if (mine !== seq || !d || !input || input.value.trim() !== q) return;
            if (apply(d)) render(true);
          })['catch'](function () {});
      }
      if (hist) get(ROUTE, function (d) { if (!d.hits) return false; srv.hits = d.hits; return true; });
      get(FIND, function (d) { srv.asks = d.asks || []; srv.pages = d.pages || []; return true; });
    }, DEBOUNCE);
  }
  // refresh is the input's handler: a changed query drops the old query's
  // server answers and asks again; the list is redrawn from the page at once.
  // keep (=== true; the input's handler is passed an Event) holds the
  // highlighted result, as a server answer's redraw does: the sidebar's rows
  // landing is not the operator typing, and must not move their selection.
  function refresh(keep) {
    var input = byId(INPUT);
    if (!input) return;
    var q = input.value.trim();
    if (q !== srv.q) { srv = { q: q, hits: [], asks: [], pages: [] }; ask(q); }
    render(keep === true);
  }
  // row is one result's <li>: its first child the label, then a muted line
  // saying what it is.
  function row(it, i) {
    var li = el('li', 'relative flex min-h-[44px] shrink-0 cursor-pointer flex-col justify-center px-4 py-1.5');
    li.id = 'switcher-opt-' + i;
    li.setAttribute('role', 'option');
    li.setAttribute('data-index', String(i));
    li.setAttribute('data-href', it.href);
    li.setAttribute('data-type', it.t);
    if (it.t !== 'session') {
      // A screen, an ask or a page: its name, and its kind with what
      // identifies it ("Screen · /pages", "Ask #12 · Open", "Page #7").
      var label = it.t === 'screen' ? it.p.label : it.t === 'ask' ? it.a.title : it.d.title;
      var line = it.t === 'screen' ? 'Screen · ' + it.p.href : it.t === 'ask' ? 'Ask #' + it.a.id + ' · ' + it.a.state : 'Page #' + it.d.id;
      li.setAttribute('data-kind', it.t);
      if (it.t === 'ask') li.setAttribute('data-ask-id', String(it.a.id));
      if (it.t === 'page') li.setAttribute('data-page-id', String(it.d.id));
      li.appendChild(el('span', 'truncate text-sm font-semibold text-fg', label));
      var k = el('span', 'truncate text-xs text-muted', line);
      k.setAttribute('data-palette-kind', '');
      li.appendChild(k);
      return li;
    }
    var e = it.it.e;
    li.setAttribute('data-kind', e.kind);
    if (it.it.why) li.setAttribute('data-match', it.it.why);
    li.appendChild(el('span', 'truncate text-sm font-semibold text-fg', e.label));
    // The second line says WHY it matched when that is a previous focus
    // title ("Previously: …", marked data-match-previous); otherwise its
    // title (its display name and current focus), or — when the title is
    // what the first line already says (an agent's display name) — its
    // codename. The title carries the current focus, so its age follows it
    // (focusAgeLineJS: shown past a day, muted past four, as the server
    // decided); a previous title or a codename has none.
    if (it.it.why === 'previous') {
      var p = el('span', 'truncate text-xs text-muted');
      p.setAttribute('data-match-previous', '');
      p.appendChild(el('span', 'font-medium', 'Previously: '));
      p.appendChild(document.createTextNode(it.it.hit));
      li.appendChild(p);
    } else {
      var isTitle = !!e.title && e.title !== e.label;
      // With a tmux line (below) the bare codename is not drawn as well: the
      // tmux line starts with it.
      var sub = isTitle ? e.title : (e.code !== e.label && !e.tmux ? e.code : '');
      if (sub) li.appendChild(focusAgeLine(isTitle ? e.age : '', e.stale, el('span', 'truncate text-xs text-muted', sub)));
    }
    // "Show tmux sessions" on: the window's place, one muted line under the
    // rest ("mango:1 @ atlas", as the server set it on the pill or sidebar
    // row it came from: data-switcher-tmux). Text in the option, so part of
    // its accessible name.
    if (e.tmux) {
      var tl = el('span', 'truncate text-xs text-muted', e.tmux);
      tl.setAttribute('data-palette-tmux', '');
      li.appendChild(tl);
    }
    // Waiting on the operator: the sidebar's own mark, copied, at the row's
    // end (out of flow, so the label stays the row's first child).
    if (e.mark) {
      li.classList.add('pr-14');
      var w = el('span', 'absolute inset-y-0 right-4 flex items-center');
      w.setAttribute('data-palette-wait', '');
      w.appendChild(e.mark.cloneNode(true));
      li.appendChild(w);
    }
    return li;
  }
  // render draws the list: the page's sessions and screens, and the server's
  // answers for the same query merged in, narrowed to the pill's kind; keep
  // holds the highlighted result the operator CHOSE (a server answer landing
  // must not move the operator's selection).
  //
  // 🔴 A HIGHLIGHT NOBODY CHOSE IS NOT KEPT: it follows the best match. Left
  // where typing put it (the first row), it is only "the best match so far";
  // kept across an answer that lists a better one above it — an ask named by
  // its id, over a session whose codename merely holds the same digits — it
  // sat on the second row while the first was the one in view, and Enter
  // opened the session.
  function render(keep) {
    var pal = byId(PAL), input = byId(INPUT), list = byId(RESULTS);
    if (!pal || !input || !list) return;
    var q = input.value, mine = srv.q && srv.q === q.trim();
    var was = keep && chosen && active >= 0 && shown[active] ? shown[active].href : null;
    var sessions = window.musterPaletteRank(entries(), q, mine ? srv.hits : []).slice(0, 50);
    shown = window.musterPaletteAll(sessions, screens(), mine ? { asks: srv.asks, pages: srv.pages } : {}, q, type);
    list.textContent = '';
    shown.forEach(function (it, i) { list.appendChild(row(it, i)); });
    active = shown.length ? 0 : -1;
    chosen = false;
    if (was) for (var k = 0; k < shown.length; k++) if (shown[k].href === was) { active = k; chosen = true; break; }
    pal.querySelector('[data-switcher-search-empty]').hidden = shown.length > 0;
    mark();
  }
  function go(i) {
    var href = shown[i] && shown[i].href;
    if (!href) return;
    hide(byId(PAL));
    window.location.assign(href);
  }
  function expanded(on) {
    var bs = document.querySelectorAll(OPEN);
    for (var i = 0; i < bs.length; i++) bs[i].setAttribute('aria-expanded', on && bs[i] === opener ? 'true' : 'false');
  }
  // THE TYPE PILLS: a radio group (one checked; the checked one the group's
  // one tab stop), All each time the palette opens.
  function pills() { var g = byId(TYPES); return g ? Array.prototype.slice.call(g.querySelectorAll('[data-palette-type]')) : []; }
  function checkedPill() { var ps = pills(); for (var i = 0; i < ps.length; i++) if (ps[i].getAttribute('aria-checked') === 'true') return ps[i]; return ps[0] || null; }
  function setType(k) {
    type = k;
    pills().forEach(function (b) {
      var on = b.getAttribute('data-palette-type') === k;
      b.setAttribute('aria-checked', on ? 'true' : 'false');
      b.setAttribute('tabindex', on ? '0' : '-1');
    });
  }
  // bindPalette wires one palette element's own listeners, once.
  function bindPalette(pal) {
    if (pal.__musterPaletteBound) return;
    pal.__musterPaletteBound = true;
    var input = byId(INPUT), list = byId(RESULTS), types = byId(TYPES);
    pal.addEventListener('toggle', function (e) {
      var open = e.newState ? e.newState === 'open' : isOpen(pal);
      expanded(open);
      // A closed palette asks nothing more: its pending search is cancelled.
      if (!open) { srv = { q: '', hits: [], asks: [], pages: [] }; ask(''); }
      if (!open && pal.contains(document.activeElement)) {
        var back = opener && opener.isConnected && opener !== document.body ? opener : null;
        try { if (back) back.focus(); } catch (err) {}
      }
    });
    input.addEventListener('input', refresh);
    input.addEventListener('keydown', function (e) {
      if (e.isComposing) return;
      var n = shown.length, next = flipped() ? 'ArrowUp' : 'ArrowDown', prev = flipped() ? 'ArrowDown' : 'ArrowUp';
      if (e.key === next) { e.preventDefault(); if (n) { active = (active + 1) % n; chosen = true; mark(); } }
      else if (e.key === prev) { e.preventDefault(); if (n) { active = (active - 1 + n) % n; chosen = true; mark(); } }
      else if (e.key === 'Enter') { e.preventDefault(); go(active); }
    });
    // A pill picked: the list narrowed to its kind. A tap keeps the input's
    // focus (and a phone's keyboard up): its mousedown is kept from moving it.
    if (types) {
      types.addEventListener('mousedown', function (e) { if (document.activeElement === input) e.preventDefault(); });
      types.addEventListener('click', function (e) {
        var b = e.target && e.target.closest ? e.target.closest('[data-palette-type]') : null;
        if (!b) return;
        setType(b.getAttribute('data-palette-type'));
        render(false);
      });
      // The radio group's arrows: Left/Right (and Up/Down) move to and pick
      // the next pill, Home/End the first and last.
      types.addEventListener('keydown', function (e) {
        var ps = pills(), i = ps.indexOf(document.activeElement), j = -1;
        if (i < 0) return;
        if (e.key === 'ArrowRight' || e.key === 'ArrowDown') j = (i + 1) % ps.length;
        else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') j = (i - 1 + ps.length) % ps.length;
        else if (e.key === 'Home') j = 0;
        else if (e.key === 'End') j = ps.length - 1;
        if (j < 0) return;
        e.preventDefault();
        setType(ps[j].getAttribute('data-palette-type'));
        ps[j].focus();
        render(false);
      });
    }
    // 🔴 THE OPEN PALETTE KEEPS FOCUS AND OWNS ESCAPE, whatever in it has
    // focus: Tab moves between its two stops — the input and the checked
    // pill — and nowhere else (the options are reached by the arrows), and
    // Escape closes the palette ONLY — stopped here, on the palette, so it
    // never also reaches a document handler (the message index's sheet, the
    // chief panel) in the same keypress.
    pal.addEventListener('keydown', function (e) {
      if (e.key === 'Tab') {
        e.preventDefault();
        var pill = checkedPill();
        if (document.activeElement === input && pill) pill.focus(); else input.focus();
      }
      else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); hide(pal); }
    });
    // A tap opens the option; the mousedown is kept from moving focus, so a
    // phone's keyboard stays up until the page goes.
    list.addEventListener('mousedown', function (e) { e.preventDefault(); });
    list.addEventListener('click', function (e) {
      var li = e.target && e.target.closest ? e.target.closest('[role=option]') : null;
      if (li) go(Number(li.getAttribute('data-index')));
    });
    // A keyboard that overlays the page moves the phone's box up with it.
    var vv = kbWin().visualViewport;
    if (vv) {
      vv.addEventListener('resize', function () { if (isOpen(byId(PAL))) fitKeyboard(); });
      vv.addEventListener('scroll', function () { if (isOpen(byId(PAL))) fitKeyboard(); });
    }
  }
  function openSearch(btn) {
    var pal = byId(PAL), input = byId(INPUT);
    if (!pal || !input) return;
    bindPalette(pal);
    if (isOpen(pal)) { input.focus(); input.select(); return; }
    opener = btn || document.activeElement;
    // The switcher's view menu and the palette are never open together.
    hide(byId('` + switcherMenuID + `'));
    input.value = '';
    setType('all');
    refresh();
    fitKeyboard();
    pal.showPopover();
    expanded(true);
    // Focused in the same task as the tap, so a phone raises its keyboard.
    input.focus();
  }
  window.musterOpenSearch = openSearch;
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest ? e.target.closest(OPEN) : null;
    if (b) openSearch(b);
  });
  // The sidebar's rows arrive by htmx (nav_agents.go): an open palette that
  // reads them is refreshed when they land, its highlight kept.
  document.addEventListener('htmx:afterSwap', function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === '` + NavAgentsListID + `' && isOpen(byId(PAL))) refresh(true);
  });
  // "/" opens the palette when focus is not in a text field (nor in a
  // terminal, xterm, where it is the shell's), and one of its buttons is on
  // screen: the bar's, or the sidebar's (the desktop rail, an open drawer —
  // not a shut one, which is moved off the screen's left edge).
  // No Ctrl/Cmd+K: that is the browser's own.
  document.addEventListener('keydown', function (e) {
    if (e.key !== '/' || e.defaultPrevented || e.isComposing || e.altKey || e.ctrlKey || e.metaKey) return;
    // The open dock is modal: the palette behind it would be inert.
    var dock = byId('` + DockID + `');
    if (dock && dock.open) return;
    var t = e.target;
    if (t && t.closest && t.closest('.xterm')) return;
    if (t && (t.isContentEditable || (t.closest && t.closest('input, textarea, select, [contenteditable=""], [contenteditable="true"]')))) return;
    // The bar's button first: on a session's page it is the one in reach.
    var bs = [byId('` + switcherSearchOpenID + `')].concat(Array.prototype.slice.call(document.querySelectorAll(OPEN))), b = null;
    for (var i = 0; i < bs.length && !b; i++) {
      if (!bs[i]) continue;
      var r = bs[i].getBoundingClientRect();
      if (bs[i].getClientRects().length && r.right > 0 && r.left < window.innerWidth) b = bs[i];
    }
    if (!b) return;
    e.preventDefault();
    openSearch(b);
  });
})();
`
