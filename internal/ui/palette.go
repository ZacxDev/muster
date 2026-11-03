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
// way.
//
// 🔴 A POPOVER (the top layer), so opening it moves nothing on the page. It
// sits in #sidebar-root, OUTSIDE the sliding #sidebar panel: the panel is
// moved by a transform, and the palette must not be part of what moves.

// The palette's ids, and the attribute that makes a button open it.
const (
	paletteID        = "switcher-search"
	paletteInputID   = "switcher-search-input"
	paletteResultsID = "switcher-search-results"
	paletteOpenAttr  = "data-palette-open"
)

// PaletteHistoryRoute is the palette's server search (internal/api's
// handlePaletteHistory): every focus title in the retention, asked for once
// typing pauses on a query of PaletteServerMinRunes or more.
const PaletteHistoryRoute = "/ui/palette/history"

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

// paletteBox is the palette: a box over the page, near its top (a phone's
// keyboard takes the bottom), with an input and the list of what it finds,
// filled by paletteJS as the operator types. Ranked by paletteMatchJS: the
// codename, then the title (current focus), then a previous focus title, then
// the words (repository, kind, tmux session); folded; ties in the source's
// order. Arrows move, Enter opens, Escape closes.
func paletteBox() g.Node {
	return Div(
		ID(paletteID),
		g.Attr("popover", ""),
		g.Attr("role", "dialog"),
		g.Attr("aria-label", "Search sessions"),
		Class("inset-x-0 bottom-auto top-[max(0.5rem,10vh)] mx-auto my-0 w-[min(36rem,calc(100vw-1rem))] overflow-hidden rounded-xl border-0 bg-s1 p-0 text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/40"),
		Div(
			Class("flex items-center gap-2 border-b border-line px-3 text-muted"),
			g.Raw(searchIconSVG),
			Input(
				ID(paletteInputID),
				Type("text"),
				g.Attr("role", "combobox"),
				g.Attr("aria-expanded", "true"),
				g.Attr("aria-controls", paletteResultsID),
				g.Attr("aria-autocomplete", "list"),
				g.Attr("aria-label", "Search sessions by codename or title"),
				g.Attr("autocomplete", "off"),
				g.Attr("autocapitalize", "off"),
				g.Attr("autocorrect", "off"),
				g.Attr("spellcheck", "false"),
				g.Attr("enterkeyhint", "go"),
				g.Attr("placeholder", "Search sessions…"),
				// text-base: 16px, so a phone does not zoom the page on focus.
				Class("h-12 min-w-0 flex-1 bg-transparent text-base text-fg outline-none placeholder:text-muted"),
			),
			Kbd(Class("hidden rounded border border-line px-1.5 text-[11px] text-muted sm:inline"), g.Text("Esc")),
		),
		Ul(
			ID(paletteResultsID),
			g.Attr("role", "listbox"),
			g.Attr("aria-label", "Sessions"),
			Class("max-h-[min(24rem,60vh)] overflow-y-auto overscroll-contain py-1"),
		),
		P(g.Attr("data-switcher-search-empty", ""), g.Attr("hidden", ""), Class("px-4 py-3 text-sm text-muted"), g.Text("No session matches")),
	)
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
		g.Attr("aria-label", "Search sessions"),
		g.Attr("title", "Search sessions (/)"),
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
  var OPEN = '[` + paletteOpenAttr + `]';
  var shown = [], active = -1, opener = null;
  function byId(id) { return document.getElementById(id); }
  function isOpen(p) { try { return !!p && p.matches(':popover-open'); } catch (e) { return false; } }
  function hide(p) { try { if (isOpen(p)) p.hidePopover(); } catch (e) {} }
  // The entries searched: the switcher's All row when the page has the bar
  // (a bfcache restore redraws it: restore.go), else the sidebar's agent
  // rows; the page's own session left out of both.
  function entries() {
    var es = document.querySelectorAll('#` + SwitcherID + ` [data-switcher-list="all"] a[data-switcher-entry]');
    if (!es.length) es = document.querySelectorAll('#` + NavAgentsListID + ` a[data-nav-agent]:not([data-nav-current])');
    var out = [];
    for (var i = 0; i < es.length; i++) {
      var a = es[i];
      var label = a.getAttribute('title') || a.textContent;
      out.push({ href: a.getAttribute('href'), label: label, code: a.getAttribute('data-switcher-code') || label,
        title: a.getAttribute('data-switcher-title') || '', words: a.getAttribute('data-switcher-words') || '',
        prev: (a.getAttribute('data-switcher-prev') || '').split('\n').filter(Boolean),
        kind: a.getAttribute('data-switcher-entry') || a.getAttribute('data-nav-agent'), n: i });
    }
    return out;
  }
  // match is what an entry is found by for query q (window.musterPaletteMatch,
  // paletteMatchJS), or null.
  var match = window.musterPaletteMatch;
  function el(tag, cls, text) { var x = document.createElement(tag); if (cls) x.className = cls; if (text != null) x.textContent = text; return x; }
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
  // THE SERVER SEARCH (internal/api's handlePaletteHistory): once typing
  // pauses for DEBOUNCE ms on a query of MIN or more characters, the server
  // is asked for every focus title in the retention; its hits are merged as
  // previous-title matches (rank 3) of entries ALREADY on the page — an entry
  // the page already matched is not listed twice, and a hit whose entry is not
  // on the page (the page's own session) is dropped. srv is the query the
  // hits answer and the hits.
  //
  // 🔴 ONLY THE ANSWER TO THE CURRENT QUERY LANDS. Each new query cancels the
  // pending timer and aborts the request in flight, AND an answer is applied
  // only if its sequence number is the latest and the input still holds its
  // query — so an out-of-order response can never overwrite newer results.
  var ROUTE = '` + PaletteHistoryRoute + `', DEBOUNCE = ` + paletteServerDebounceMS + `, MIN = ` + paletteServerMinRunesJS + `;
  var srv = { q: '', hits: [] }, seq = 0, timer = 0, ctl = null;
  function ask(q) {
    clearTimeout(timer);
    seq++;
    if (ctl) { try { ctl.abort(); } catch (e) {} ctl = null; }
    if (Array.from(q).length < MIN) return;
    var mine = seq;
    timer = setTimeout(function () {
      if (mine !== seq) return;
      var c = typeof AbortController === 'function' ? new AbortController() : null;
      ctl = c;
      fetch(ROUTE + '?q=' + encodeURIComponent(q), { credentials: 'same-origin', headers: { Accept: 'application/json' }, signal: c ? c.signal : undefined })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) {
          var input = byId(INPUT);
          if (mine !== seq || !d || !d.hits || !input || input.value.trim() !== q) return;
          srv = { q: q, hits: d.hits };
          render(true);
        })['catch'](function () {});
    }, DEBOUNCE);
  }
  // refresh is the input's handler: a changed query drops the old query's
  // server hits and asks again; the list is redrawn from the page at once.
  function refresh() {
    var input = byId(INPUT);
    if (!input) return;
    var q = input.value.trim();
    if (q !== srv.q) { srv = { q: q, hits: [] }; ask(q); }
    render(false);
  }
  // render draws the list: the page's matches, then the server's hits for the
  // same query merged in; keep holds the highlighted entry (a server answer
  // landing must not move the operator's selection).
  function render(keep) {
    var pal = byId(PAL), input = byId(INPUT), list = byId(RESULTS);
    if (!pal || !input || !list) return;
    var q = input.value;
    var was = keep && active >= 0 && shown[active] ? shown[active].href : null;
    var found = [], at = {}, byHref = {};
    entries().forEach(function (e) {
      byHref[e.href] = e;
      var m = match(e, q);
      if (m) { e.r = m.r; e.why = m.why; e.hit = m.hit; found.push(e); at[e.href] = true; }
    });
    if (srv.q && srv.q === q.trim()) {
      srv.hits.forEach(function (h) {
        var e = h && byHref[h.href];
        if (!e || at[h.href]) return;
        at[h.href] = true;
        e.r = 3; e.why = 'previous'; e.hit = String(h.title || '');
        found.push(e);
      });
    }
    found.sort(function (a, b) { return a.r - b.r || a.n - b.n; });
    shown = found.slice(0, 50);
    list.textContent = '';
    shown.forEach(function (e, i) {
      var li = el('li', 'flex min-h-[44px] cursor-pointer flex-col justify-center px-4 py-1.5');
      li.id = 'switcher-opt-' + i;
      li.setAttribute('role', 'option');
      li.setAttribute('data-index', String(i));
      li.setAttribute('data-href', e.href);
      li.setAttribute('data-kind', e.kind);
      if (e.why) li.setAttribute('data-match', e.why);
      li.appendChild(el('span', 'truncate text-sm font-semibold text-fg', e.label));
      // The second line says WHY it matched when that is a previous focus
      // title ("Previously: …", marked data-match-previous); otherwise its
      // title (its display name and current focus), or — when the title is
      // what the first line already says (an agent's display name) — its
      // codename.
      if (e.why === 'previous') {
        var p = el('span', 'truncate text-xs text-muted');
        p.setAttribute('data-match-previous', '');
        p.appendChild(el('span', 'font-medium', 'Previously: '));
        p.appendChild(document.createTextNode(e.hit));
        li.appendChild(p);
      } else {
        var sub = e.title && e.title !== e.label ? e.title : (e.code !== e.label ? e.code : '');
        if (sub) li.appendChild(el('span', 'truncate text-xs text-muted', sub));
      }
      list.appendChild(li);
    });
    active = shown.length ? 0 : -1;
    if (was) for (var k = 0; k < shown.length; k++) if (shown[k].href === was) { active = k; break; }
    pal.querySelector('[data-switcher-search-empty]').hidden = shown.length > 0;
    mark();
  }
  function go(i) {
    var e = shown[i];
    if (!e || !e.href) return;
    hide(byId(PAL));
    window.location.assign(e.href);
  }
  function expanded(on) {
    var bs = document.querySelectorAll(OPEN);
    for (var i = 0; i < bs.length; i++) bs[i].setAttribute('aria-expanded', on && bs[i] === opener ? 'true' : 'false');
  }
  // bindPalette wires one palette element's own listeners, once.
  function bindPalette(pal) {
    if (pal.__musterPaletteBound) return;
    pal.__musterPaletteBound = true;
    var input = byId(INPUT), list = byId(RESULTS);
    pal.addEventListener('toggle', function (e) {
      var open = e.newState ? e.newState === 'open' : isOpen(pal);
      expanded(open);
      // A closed palette asks nothing more: its pending search is cancelled.
      if (!open) { srv = { q: '', hits: [] }; ask(''); }
      if (!open && pal.contains(document.activeElement)) {
        var back = opener && opener.isConnected && opener !== document.body ? opener : null;
        try { if (back) back.focus(); } catch (err) {}
      }
    });
    input.addEventListener('input', refresh);
    input.addEventListener('keydown', function (e) {
      if (e.isComposing) return;
      var n = shown.length;
      if (e.key === 'ArrowDown') { e.preventDefault(); if (n) { active = (active + 1) % n; mark(); } }
      else if (e.key === 'ArrowUp') { e.preventDefault(); if (n) { active = (active - 1 + n) % n; mark(); } }
      else if (e.key === 'Enter') { e.preventDefault(); go(active); }
    });
    // 🔴 THE OPEN PALETTE KEEPS FOCUS AND OWNS ESCAPE, whatever in it has
    // focus: Tab stays on the input (nothing else in it takes focus; the
    // options are reached by the arrows), and Escape closes the palette ONLY —
    // stopped here, on the palette, so it never also reaches a document
    // handler (the message index's sheet, the chief panel) in the same keypress.
    pal.addEventListener('keydown', function (e) {
      if (e.key === 'Tab') { e.preventDefault(); input.focus(); }
      else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); hide(pal); }
    });
    // A tap opens the option; the mousedown is kept from moving focus, so a
    // phone's keyboard stays up until the page goes.
    list.addEventListener('mousedown', function (e) { e.preventDefault(); });
    list.addEventListener('click', function (e) {
      var li = e.target && e.target.closest ? e.target.closest('[role=option]') : null;
      if (li) go(Number(li.getAttribute('data-index')));
    });
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
    refresh();
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
  // reads them is refreshed when they land.
  document.addEventListener('htmx:afterSwap', function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === '` + NavAgentsListID + `' && isOpen(byId(PAL))) refresh();
  });
  // "/" opens the palette when focus is not in a text field (nor in a
  // terminal, xterm, where it is the shell's), and one of its buttons is on
  // screen: the bar's, or the sidebar's (the desktop rail, an open drawer —
  // not a shut one, which is moved off the screen's left edge).
  // No Ctrl/Cmd+K: that is the browser's own.
  document.addEventListener('keydown', function (e) {
    if (e.key !== '/' || e.defaultPrevented || e.isComposing || e.altKey || e.ctrlKey || e.metaKey) return;
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
