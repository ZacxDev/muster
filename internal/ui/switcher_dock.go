package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SWITCHER DOCK (docs/design/switcher-dock.md, Phase 1): the session
// switcher's four views as a vertical list in a bottom sheet, for a touch
// screen, where the bar's single sideways-scrolling row is crowded (operator
// ask: "I have to scroll horizontally, which is crowded on mobile").
//
//   - A NATIVE MODAL <dialog>, drawn ONCE PER DOCUMENT in #sidebar-root beside
//     the command palette (sidebarIn), never inside the bar: three ancestors of
//     the bar can hide it (the keyboard rule, the short-screen rule, the
//     Rendered view's composer while it is invisible), and a top-layer element
//     under a display:none ancestor is still not rendered. showModal() gives
//     the role, aria-modal, a focus trap, inertness outside, and Escape.
//   - OPENED IN THE TOUCH LAYOUT ONLY, by the bar's view button, which is its
//     handle there (DockTouchQuery: a coarse primary pointer, at any width;
//     operator decision 3 as amended, and 5). A fine pointer keeps the view
//     menu. Gestures (a swipe up on the bar, the header drag) and the desktop
//     chevron are later phases.
//   - ITS ROWS ARE THE BAR'S, read when it opens: the four rows the page
//     already carries (no request, no second copy of the markup; the bar's
//     bfcache refresh and the pin toggle's out-of-band refresh keep them
//     current, and the dock rebuilds on every open). Recent is the bar's 8
//     followed by the hrefs of Switcher.RecentMore, each drawn from its All
//     pill. Built with textContent / setAttribute only, as the palette is.
//   - 🔴 IT TOUCHES NO HISTORY. Back reaches it through the modal dialog's own
//     close watcher in a browser, and through the back-claim bridge message in
//     the Android shell (window.musterBack answers the shell's press). A row
//     tap is the pill's real load; the dock is CLOSED BEFORE it, so a page the
//     browser keeps in its back/forward cache is stored closed, and it is
//     force-closed on pageshow.persisted, and on htmx:beforeHistorySave (htmx
//     snapshots the body's HTML, which would bring a <dialog open> back as a
//     non-modal open dialog).
//   - 🔴 ONE CLEANUP POINT: the dialog's close event. Every way it closes ends
//     in dialog.close(), and only the close handler releases the claim, resets
//     the sheet and returns focus.
//   - 🔴 ALWAYS IN THE TOP DOCUMENT. A bar inside the view switch's frame
//     (view_swap.go) opens its PARENT's dock (switcherJS checks the parent has
//     one before it prevents the menu), which reads that frame bar's rows,
//     measures the top visual viewport, posts the claim from the main frame
//     (the shell's bridge refuses a subframe), and navigates the top window.

// DockID is the dock dialog's id.
const DockID = "switcher-dock"

// DockTouchQuery is the touch layout: the one media query the bar's script and
// the dock decide by (operator decision 3: "the touch layout is chosen by
// (pointer: coarse) alone, at any width").
const DockTouchQuery = "(pointer: coarse)"

// The dock's parts, as the script finds them.
const (
	dockListID  = "dock-list"
	dockTitleID = "switcher-dock-title"
	// dockRowMin is a row's minimum height in px (a full-width target), and
	// what the half snap keeps room for three of below the header.
	dockRowMin = 56
)

// dockTabID is view key's tab.
func dockTabID(key string) string { return "dock-tab-" + key }

// switcherDock is the dock: closed and empty until the script opens it from a
// bar. Its header holds the grip (a full-width 44px button drawing a 36×4
// handle: a tap toggles half and full), the visually hidden name, the view
// tabs and the close button; the list below is the one vertical scroller.
func switcherDock() g.Node {
	tabs := make([]g.Node, 0, len(switcherViews))
	for _, v := range switcherViews {
		sel := v.key == SwitcherRecent
		tabs = append(tabs, Button(
			Type("button"),
			ID(dockTabID(v.key)),
			g.Attr("role", "tab"),
			g.Attr("aria-controls", dockListID),
			g.Attr("aria-selected", strconv.FormatBool(sel)),
			g.Attr("tabindex", map[bool]string{true: "0", false: "-1"}[sel]),
			g.Attr("data-dock-tab", v.key),
			Class("press inline-flex min-h-[44px] min-w-[44px] flex-1 items-center justify-center gap-1 border-b-2 border-transparent px-2 text-sm font-medium text-fg2 outline-none transition focus-visible:bg-s2 aria-selected:border-accent aria-selected:text-fg"),
			g.Text(v.name),
			Span(g.Attr("data-dock-count", v.key), Class("text-xs tabular-nums text-muted")),
		))
	}
	return g.El("dialog",
		ID(DockID),
		g.Attr("aria-labelledby", dockTitleID),
		// A sideways swipe here is never the sidebar edge-swipe's (appScript's
		// ownsSideways), so the sidebar cannot open over the dock.
		g.Attr("data-own-swipe", ""),
		g.Attr("data-snap", "half"),
		Class("flex-col overflow-hidden bg-s1 p-0 text-fg shadow-2xl ring-1 ring-line"),
		Div(
			g.Attr("data-dock-header", ""),
			Class("shrink-0 border-b border-line"),
			Button(
				Type("button"),
				g.Attr("data-dock-grip", ""),
				g.Attr("aria-label", "Expand"),
				Class("flex h-11 w-full items-center justify-center outline-none focus-visible:bg-s2"),
				Span(g.Attr("aria-hidden", "true"), Class("h-1 w-9 rounded-full bg-line")),
			),
			H2(ID(dockTitleID), Class("sr-only"), g.Text("Switch session")),
			Div(
				Class("flex items-center"),
				Div(g.Attr("role", "tablist"), g.Attr("aria-label", "Session views"), Class("flex min-w-0 flex-1"), g.Group(tabs)),
				Button(
					Type("button"),
					g.Attr("data-dock-close", ""),
					g.Attr("aria-label", "Close"),
					Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center text-muted transition hover:text-fg"),
					g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>`),
				),
			),
		),
		Ul(
			ID(dockListID),
			g.Attr("role", "tabpanel"),
			g.Attr("aria-labelledby", dockTabID(SwitcherRecent)),
			g.Attr("data-dock-list", ""),
			Class("min-h-0 flex-1 overflow-y-auto overscroll-contain pb-[env(safe-area-inset-bottom)]"),
		),
		P(g.Attr("data-dock-empty", ""), g.Attr("hidden", ""), Class("px-4 py-3 text-sm text-muted")),
	)
}

// dockRowClass and dockRowCurrentClass are a dock row's (a link) and the
// page's own session's (accent, not a link).
const (
	dockRowClass        = "flex min-h-[56px] w-full flex-col justify-center gap-0.5 px-4 py-2 text-left text-sm text-fg outline-none transition hover:bg-s2 focus-visible:bg-s2"
	dockRowCurrentClass = "flex min-h-[56px] w-full flex-col justify-center gap-0.5 bg-accent/10 px-4 py-2 text-left text-sm text-accent"
)

// dockJS is the dock's script, part of appScript's static file (every page
// with the sidebar). It binds ONCE PER WINDOW (window.musterDock); a boosted
// navigation swaps the dialog with the body, so the dialog is looked up when
// it is used and its own listeners are bound to each element once.
var dockJS = func() string {
	views, names, empty := "[", "{", "{"
	for i, v := range switcherViews {
		if i > 0 {
			views, names, empty = views+",", names+",", empty+","
		}
		views += strconv.Quote(v.key)
		names += strconv.Quote(v.key) + ":" + strconv.Quote(v.name)
		empty += strconv.Quote(v.key) + ":" + strconv.Quote(v.empty)
	}
	return `
(function () {
  if (window.musterDock) return;
  var ID = '` + DockID + `', LIST = '` + dockListID + `';
  var VIEWS = ` + views + `], EMPTY = ` + empty + `};
  var LABEL = 'span.` + switcherLabelClass + `', MORE = '` + switcherRecentMoreAttr + `';
  var ROW = '` + dockRowClass + `', ROW_CUR = '` + dockRowCurrentClass + `';
  var ROW_MIN = ` + strconv.Itoa(dockRowMin) + `;
  // st is the open dock: the bar it reads (a frame's, when the frame opened
  // it), the element focus goes back to, and the tab shown. null when shut.
  var st = null, claimed = false;
  function dlg() { return document.getElementById(ID); }
  function el(tag, cls, text) { var x = document.createElement(tag); if (cls) x.className = cls; if (text != null) x.textContent = text; return x; }

  // THE SHELL'S CLAIM (the Android app, docs/design/switcher-dock.md "The
  // shell change"): while the dock is open the shell offers its Back press to
  // the page (window.musterBack) first. Posted only where the bridge exists.
  function claim(on) {
    claimed = on;
    var b = window.` + AppBridgeName + `;
    try { if (b && typeof b.postMessage === 'function') b.postMessage(JSON.stringify({ type: 'back-claim', open: on })); } catch (e) {}
  }

  // The bar's menus and the palette are outside the modal (inert under it):
  // opening the dock closes them first, in this document and the bar's.
  function hidePops(doc) {
    ['` + switcherMenuID + `', '` + switcherMoreID + `', '` + paletteID + `'].forEach(function (id) {
      var p = doc && doc.getElementById(id);
      try { if (p && p.matches(':popover-open')) p.hidePopover(); } catch (e) {}
    });
  }

  // entries is view's pills on the bar: its row's, and for Recent the
  // entries past the bar's 8 (MORE, hrefs in visit order), each its All pill.
  function entries(view) {
    var bar = st.bar, out = [];
    var row = bar.querySelector('[data-switcher-list="' + view + '"]');
    if (!row) return out;
    out = Array.prototype.slice.call(row.querySelectorAll('[data-switcher-entry]'));
    var more = view === '` + SwitcherRecent + `' ? row.querySelector('[' + MORE + ']') : null;
    if (more) {
      var all = bar.querySelectorAll('[data-switcher-list="` + SwitcherAll + `"] a[data-switcher-entry]');
      more.getAttribute(MORE).split('\n').forEach(function (h) {
        if (!h) return;
        for (var i = 0; i < all.length; i++) if (all[i].getAttribute('href') === h) { out.push(all[i]); return; }
      });
    }
    return out;
  }

  // rowOf is pill p as a dock row: line 1 the waiting badge, the WHOLE label
  // (wrapping, never cut: the bar's whole-name rule) and the host, icon and
  // name; line 2 the current focus title, one line. The page's own session is
  // drawn current, not linked, where the bar draws it so.
  function rowOf(p) {
    var cur = p.hasAttribute('data-switcher-current');
    var li = el('li');
    var r = el(cur ? 'span' : 'a', cur ? ROW_CUR : ROW);
    r.setAttribute('data-dock-row', p.getAttribute('data-switcher-entry') || '');
    if (cur) r.setAttribute('aria-current', 'page');
    else { r.setAttribute('href', p.getAttribute('href')); r.setAttribute('hx-boost', 'false'); }
    var l1 = el('span', 'flex items-center gap-2');
    if (p.querySelector('[data-switcher-wait]')) {
      var w = el('span', 'h-2 w-2 shrink-0 rounded-full bg-accent');
      w.setAttribute('data-dock-wait', '');
      w.setAttribute('role', 'img');
      w.setAttribute('aria-label', 'Waiting on you');
      l1.appendChild(w);
    }
    var lab = p.querySelector(LABEL);
    var name = el('span', 'min-w-0 flex-1 break-words font-semibold', lab ? lab.textContent : (p.getAttribute('title') || ''));
    name.setAttribute('data-dock-label', '');
    l1.appendChild(name);
    var host = p.getAttribute('data-switcher-host');
    if (host) {
      var hb = el('span', 'inline-flex shrink-0 items-center gap-1 text-xs text-fg2');
      hb.setAttribute('data-dock-host', '');
      var ic = p.querySelector('[data-host-icon]');
      if (ic) {
        // Decorative here: the host's name is written beside it.
        ic = ic.cloneNode(true);
        ic.removeAttribute('role');
        ic.removeAttribute('aria-label');
        ic.setAttribute('aria-hidden', 'true');
        hb.appendChild(ic);
      }
      hb.appendChild(el('span', '', host));
      l1.appendChild(hb);
    }
    r.appendChild(l1);
    var f = p.getAttribute('data-switcher-focus');
    if (f) {
      var l2 = el('span', 'truncate text-xs text-muted', f);
      l2.setAttribute('data-dock-focus', '');
      r.appendChild(l2);
    }
    li.appendChild(r);
    return li;
  }
  function tabOf(view) { return document.getElementById('dock-tab-' + view); }

  // render draws the shown tab's rows, every tab's count and the tabs' state.
  function render() {
    var d = dlg(), list = document.getElementById(LIST);
    if (!d || !list || !st) return;
    var es = entries(st.view);
    list.textContent = '';
    es.forEach(function (p) { list.appendChild(rowOf(p)); });
    list.setAttribute('aria-labelledby', 'dock-tab-' + st.view);
    var empty = d.querySelector('[data-dock-empty]');
    if (empty) { empty.textContent = EMPTY[st.view]; empty.hidden = es.length > 0; }
    VIEWS.forEach(function (v) {
      var t = tabOf(v);
      if (!t) return;
      var on = v === st.view;
      t.setAttribute('aria-selected', on ? 'true' : 'false');
      t.setAttribute('tabindex', on ? '0' : '-1');
      var n = t.querySelector('[data-dock-count]');
      if (n) n.textContent = String(v === st.view ? es.length : entries(v).length);
    });
  }

  // pick shows view's tab, and moves the bar that opened the dock to it at
  // once — and the top window's own bar, when a frame's opened it — through
  // the bars' own hook (switcherJS's musterSwitcherShow: the row, the icon,
  // the menu's checks, the stored choice).
  function pick(view, focus) {
    if (!st) return;
    if (VIEWS.indexOf(view) < 0) view = VIEWS[0];
    st.view = view;
    render();
    var ws = [];
    try { ws.push(st.bar.ownerDocument.defaultView); } catch (e) {}
    if (ws.indexOf(window) < 0) ws.push(window);
    ws.forEach(function (w) { try { if (w && typeof w.musterSwitcherShow === 'function') w.musterSwitcherShow(view); } catch (e) {} });
    if (focus) { var t = tabOf(view); if (t) t.focus(); }
  }

  // THE SNAPS, against the VISUAL viewport: half is 60% of it, never less
  // than the header and three rows; full is the whole of it less the top safe
  // area and an 8px gap (web/css/input.css). Sitting on a soft keyboard is
  // Phase 3's, with the filter (nothing in this phase raises one).
  function measure() {
    var d = dlg();
    if (!d) return;
    var v = window.visualViewport, h = v ? v.height : window.innerHeight;
    var head = d.querySelector('[data-dock-header]');
    var half = Math.max(Math.round(0.6 * h), (head ? head.offsetHeight : 0) + 3 * ROW_MIN);
    d.style.setProperty('--dock-vvh', Math.round(h) + 'px');
    d.style.setProperty('--dock-half', half + 'px');
  }
  function snap(d, to) {
    d.setAttribute('data-snap', to);
    var g = d.querySelector('[data-dock-grip]');
    if (g) g.setAttribute('aria-label', to === 'full' ? 'Shrink to half' : 'Expand');
  }

  function plain(e, a) {
    return !(e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey ||
      (a.target && a.target !== '_self') || a.hasAttribute('download'));
  }

  // bind wires one dialog element's own listeners, once.
  function bind(d) {
    if (d.__musterDockBound) return;
    d.__musterDockBound = true;
    d.addEventListener('click', function (e) {
      var t = e.target;
      // The backdrop: a tap outside the sheet lands on the dialog itself — but
      // so does one on the sheet's own padding (the safe-area sides), so the
      // point decides: outside the dialog's box is the backdrop.
      if (t === d) {
        var b = d.getBoundingClientRect();
        if (e.clientX < b.left || e.clientX > b.right || e.clientY < b.top || e.clientY > b.bottom) d.close();
        return;
      }
      if (!t || !t.closest) return;
      if (t.closest('[data-dock-close]')) { d.close(); return; }
      if (t.closest('[data-dock-grip]')) { snap(d, d.getAttribute('data-snap') === 'full' ? 'half' : 'full'); return; }
      var tab = t.closest('[data-dock-tab]');
      if (tab) { pick(tab.getAttribute('data-dock-tab'), false); return; }
      var a = t.closest('a[data-dock-row]');
      if (!a || !plain(e, a)) return;
      // 🔴 CLOSED BEFORE THE NAVIGATION, which the browser then follows: a
      // page kept in the back/forward cache is stored with the dock shut.
      var framed = !!st && st.bar.ownerDocument !== document;
      d.close();
      // A row for the page's own view, from a frame's bar: shown, not loaded
      // again (as view_swap.go's leave does a frame link).
      if (framed && window.musterViews && window.musterViews.showOwn(a.href)) e.preventDefault();
    });
    // The tabs: Left / Right / Home / End move between them and show the tab
    // (a roving tabindex). No swipe ever changes a tab.
    d.addEventListener('keydown', function (e) {
      var tab = e.target && e.target.closest ? e.target.closest('[data-dock-tab]') : null;
      if (!tab || !st) return;
      var i = VIEWS.indexOf(tab.getAttribute('data-dock-tab')), n = VIEWS.length, to = -1;
      if (e.key === 'ArrowRight') to = (i + 1) % n;
      else if (e.key === 'ArrowLeft') to = (i - 1 + n) % n;
      else if (e.key === 'Home') to = 0;
      else if (e.key === 'End') to = n - 1;
      if (to < 0) return;
      e.preventDefault();
      pick(VIEWS[to], true);
    });
    // The close event: this dialog's opening, cleaned up (see cleanup) —
    // unless the dock is open again, or another dialog's opening is the one
    // held (a boosted swap brought a new dock): then this event is late.
    d.addEventListener('close', function () {
      if (d.open || !st || st.d !== d) return;
      cleanup();
    });
  }

  // 🔴 THE ONE CLEANUP POINT. Escape and a browser's Back (the dialog's
  // cancel), the backdrop, the close button, musterBack(), a row's click,
  // pageshow.persisted and htmx:beforeHistorySave all end in dialog.close(),
  // whose close event runs this; only this releases the claim, resets the
  // sheet and returns focus — to the opener, in whichever document it lives (a
  // frame's view button), never scrolling.
  //
  // 🔴 A close EVENT IS A TASK, AND IT CAN LAND AFTER THE DOCK WAS OPENED
  // AGAIN: it fires a few ms after close() (2–7 ms measured, headless
  // Chromium 153), and a reopen inside that window posted the new opening's
  // claim BEFORE the old one's release, which then released it under the open
  // dock and dropped its state. So an opening that finds the last one not yet
  // cleaned up runs this first, and the late event finds nothing to do.
  function cleanup() {
    var s = st;
    if (!s) return;
    st = null;
    claim(false);
    snap(s.d, 'half');
    if (!s.opener) return;
    s.opener.setAttribute('aria-expanded', 'false');
    if (!s.opener.isConnected) return;
    try {
      var w = s.opener.ownerDocument.defaultView;
      if (w && w !== window) w.focus();
      s.opener.focus({ preventScroll: true });
    } catch (e) {}
  }

  // open shows the dock over the page, reading bar's rows, on bar's shown view.
  // opener is where focus goes back to. Returns whether it is open.
  function open(bar, opener) {
    var d = dlg();
    if (!d || !bar || !bar.querySelector) return false;
    bind(d);
    if (d.open) return true;
    cleanup();
    hidePops(document);
    try { if (bar.ownerDocument !== document) hidePops(bar.ownerDocument); } catch (e) {}
    var v = bar.getAttribute('data-switcher-view');
    st = { d: d, bar: bar, opener: opener || null, view: VIEWS.indexOf(v) < 0 ? VIEWS[0] : v };
    snap(d, 'half');
    render();
    try { d.showModal(); } catch (e) { st = null; return false; }
    measure();
    if (opener) opener.setAttribute('aria-expanded', 'true');
    claim(true);
    // On the shown tab, NOT a text field: opening never raises a keyboard.
    var t = tabOf(st.view);
    if (t) t.focus({ preventScroll: true });
    return true;
  }
  function close() { var d = dlg(); if (d && d.open) { d.close(); return true; } return false; }

  window.musterDock = { open: open, close: close };
  // The shell's Back (the claim above): true when it closed the dock.
  window.musterBack = close;
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', function () { var d = dlg(); if (d && d.open) measure(); });
  }
  // 🔴 htmx's history cache must never hold the dock open: its snapshot is
  // the body's HTML, taken right after this event (as view_swap.go removes its
  // frame here). It also runs as htmx restores on Back.
  document.addEventListener('htmx:beforeHistorySave', close);
  // A page back from the back/forward cache is never shown with the dock open.
  window.addEventListener('pageshow', function (e) { if (e.persisted) close(); });
  window.addEventListener('pagehide', function () { if (claimed) claim(false); });
})();
`
}()
