package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE COMMAND PALETTE: a search box over the page that finds any session, as
// it is typed — by its tmux codename ("wheat", "mango:2") or its title, and,
// ranked AFTER every codename and title match, by its repository, its kind
// ("Claude Code", "opencode", "shell") and a window's raw tmux SESSION name
// ("scratch11"), each a plain word (operator ask: "Teach the palette
// repo+kind"; no field tokens).
//
// 🔴 ONE PALETTE, TWO WAYS IN (operator ask: "a search icon button [in the
// sidebar] that opens the same command palette UI that the bottom bar search
// opens"). It is drawn ONCE per document, in the sidebar's root (sidebarIn) —
// every page with the session switcher has the sidebar too — and opened by
// any button carrying paletteOpenAttr: the session switcher's magnifier and
// the sidebar's (sidebarSearchButton). "/" outside a text field opens it as
// well, wherever one of those buttons is on screen.
//
// WHAT IT SEARCHES is read from the page, with no request: the session
// switcher's All row when the page has the bar (its entries' data-switcher-
// code, -title and -words), otherwise the sidebar's agent rows, which carry
// the same three attributes (navAgentRow, from the same switcherItems). The
// values are attributes the server escapes, read with getAttribute and drawn
// with textContent. The page's own session is left out either way.
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

// SidebarSearchID is the sidebar's magnifier (sidebarSearchButton).
const SidebarSearchID = "sidebar-search"

// searchIconSVG is the magnifier (the 20px solid set the bar's other icon is from).
const searchIconSVG = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clip-rule="evenodd"/></svg>`

// paletteBox is the palette: a box over the page, near its top (a phone's
// keyboard takes the bottom), with an input and the list of what it finds,
// filled by paletteJS as the operator types. Ranked: the codename starts with
// what is typed, then contains it, then the title does, then the words
// (repository, kind, tmux session) do; case-folded; each tier in the source's
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

// paletteJS is the palette's script, part of appScript's static file (every
// page with the sidebar loads it). It binds ONCE PER WINDOW, at the document:
// a boosted navigation swaps the body, palette and buttons with it, so every
// element is looked up when it is used, and the palette's own listeners are
// bound to each palette element once (bindPalette).
const paletteJS = `
(function () {
  if (window.__musterPalette) return;
  window.__musterPalette = true;
  var PAL = '` + paletteID + `', INPUT = '` + paletteInputID + `', RESULTS = '` + paletteResultsID + `';
  var OPEN = '[` + paletteOpenAttr + `]';
  var shown = [], active = -1, opener = null;
  function byId(id) { return document.getElementById(id); }
  function isOpen(p) { try { return !!p && p.matches(':popover-open'); } catch (e) { return false; } }
  function hide(p) { try { if (isOpen(p)) p.hidePopover(); } catch (e) {} }
  function fold(s) { return String(s || '').toLowerCase(); }
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
        kind: a.getAttribute('data-switcher-entry') || a.getAttribute('data-nav-agent'), n: i });
    }
    return out;
  }
  // rank: 0 the codename starts with q, 1 it contains q, 2 the title contains
  // q, 3 the words do, -1 no match. An empty q is everything, in order.
  function rank(e, q) {
    if (!q) return 0;
    var c = fold(e.code);
    if (c.indexOf(q) === 0) return 0;
    if (c.indexOf(q) > 0) return 1;
    if (fold(e.title).indexOf(q) >= 0) return 2;
    if (fold(e.words).indexOf(q) >= 0) return 3;
    return -1;
  }
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
  function refresh() {
    var pal = byId(PAL), input = byId(INPUT), list = byId(RESULTS);
    if (!pal || !input || !list) return;
    var q = fold(input.value).trim();
    var found = [];
    entries().forEach(function (e) { var r = rank(e, q); if (r >= 0) { e.r = r; found.push(e); } });
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
      li.appendChild(el('span', 'truncate text-sm font-semibold text-fg', e.label));
      // The second line: its title, or — when the title is what the first line
      // already says (an agent's display name) — its codename.
      var sub = e.title && e.title !== e.label ? e.title : (e.code !== e.label ? e.code : '');
      if (sub) li.appendChild(el('span', 'truncate text-xs text-muted', sub));
      list.appendChild(li);
    });
    active = shown.length ? 0 : -1;
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
