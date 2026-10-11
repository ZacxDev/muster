package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// floatingMenuScript places every [data-floating-menu] popover next to the
// button that opens it (its [popovertarget]). The popover itself — top layer,
// outside-tap and Escape dismissal — is the browser's; this only decides WHERE.
//
// The rule, in order:
//   - right-aligned to the trigger, just below it;
//   - flipped ABOVE the trigger when below would cross the viewport bottom, and
//     pinned to the bottom edge when neither side fits;
//   - clamped horizontally inside an 8px edge, which is what keeps a 176px menu
//     on screen when its trigger sits near either edge of a 412px phone.
//
// 🔴 IT RE-PLACES WHENEVER THE MENU CAN HAVE MOVED OR GROWN, NOT ONLY ON OPEN.
// Opening the nested "Delete" <details> makes the menu taller, so a menu placed
// above or below a trigger near the bottom must be re-placed when it GROWS
// (a ResizeObserver, see below — the toggle event arrives a task too late);
// and a scroll or resize moves the trigger out from under it.
//
// ⚠ A MORPH OF THE CARD LIST CLOSES AN OPEN MENU — MEASURED, NOT ASSUMED. This
// script once also re-placed after every htmx settle, on the theory that a morph
// keeps a popover open while stripping its inline position; an e2e probe found
// the menu CLOSED after the morph every time, so that listener was dead code and
// is gone. agent-menu.spec.ts pins the invariant that matters either way: after a
// morph the menu is closed or beside its trigger, never stranded mid-screen.
//
// A request started from inside a menu closes it as the request STARTS, rather
// than waiting for the response's morph — and a request that fails, and so
// morphs nothing, still closes it.
//
// Bound once per document (__musterFloatingMenu), like every other script here:
// htmx re-executes in-body <script> tags on each boosted swap.
func floatingMenuScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterFloatingMenu) return;
  window.__musterFloatingMenu = true;
  var GAP = 4, EDGE = 8;
  function isOpen(m) { try { return m.matches(':popover-open'); } catch (e) { return false; } }
  function place(m) {
    if (!m || !m.id || !isOpen(m)) return;
    var t = document.querySelector('[popovertarget="' + m.id + '"]');
    if (!t) return;
    var r = t.getBoundingClientRect();
    var vw = document.documentElement.clientWidth, vh = window.innerHeight;
    m.style.position = 'fixed';
    m.style.inset = 'auto';
    m.style.margin = '0';
    m.style.maxHeight = (vh - 2 * EDGE) + 'px';
    m.style.overflowY = 'auto';
    var w = m.offsetWidth, h = m.offsetHeight;
    var left = Math.max(EDGE, Math.min(r.right - w, vw - w - EDGE));
    var top = r.bottom + GAP;
    if (top + h > vh - EDGE) {
      var above = r.top - GAP - h;
      top = above >= EDGE ? above : Math.max(EDGE, vh - h - EDGE);
    }
    m.style.left = left + 'px';
    m.style.top = top + 'px';
  }
  function placeAll() {
    var ms = document.querySelectorAll('[data-floating-menu]');
    for (var i = 0; i < ms.length; i++) place(ms[i]);
  }
  // A size change re-places through a ResizeObserver, NOT through the nested
  // <details>' toggle event: opening Delete grows the menu SYNCHRONOUSLY, but
  // toggle is dispatched in a LATER task, and until it ran the grown menu hung
  // over its trigger (measured: an e2e probe sampling between the two saw it,
  // every time at 412px). ResizeObserver callbacks run in the rendering steps
  // BEFORE paint, so the overhanging frame is never drawn.
  var ro = window.ResizeObserver ? new ResizeObserver(function (es) {
    for (var i = 0; i < es.length; i++) place(es[i].target);
  }) : null;
  // toggle does not bubble; a CAPTURING listener on document sees every one —
  // the popover's own (start/stop observing it) and the nested <details>' (a
  // fallback re-place where ResizeObserver is missing).
  document.addEventListener('toggle', function (e) {
    var el = e.target;
    if (!el || !el.closest) return;
    var m = el.closest('[data-floating-menu]');
    if (!m) return;
    if (ro && el === m) { if (isOpen(m)) ro.observe(m); else ro.unobserve(m); }
    place(m);
  }, true);
  window.addEventListener('resize', placeAll);
  document.addEventListener('scroll', placeAll, { capture: true, passive: true });
  document.addEventListener('htmx:beforeRequest', function (e) {
    var el = e.target;
    var m = el && el.closest && el.closest('[data-floating-menu]');
    if (m && isOpen(m)) { try { m.hidePopover(); } catch (err) {} }
  });
})();
`))
}
