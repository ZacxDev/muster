package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
)

// THE TAB SWIPE: a sideways swipe on a swipe area moves its tablist one tab,
// in the swipe's direction. ONE implementation for every swiped tablist — the
// Asks page's feed (asks_drafts.go) and the session panel's Previews | Pages
// (plugin_chip.go) — so the two cannot drift apart.
//
// A swipe area is an element carrying TabSwipeAttr, whose value is the id of
// the tablist it moves (role=tab children, aria-selected on the current one,
// tabs.go's markup). The gesture is locked to an axis after TabSwipeLockPx, so
// a vertical scroll never switches a tab; it is ignored when it starts in a
// field, the Asks page's answer row or media, anything marked data-own-swipe or
// that scrolls sideways itself, the sidebar's open-swipe edge, with text
// selected, or with the page pinch-zoomed. A swipe switches the tab by CLICKING
// it, so whatever a tap on the tab does (tabs.go's select and its stored
// choice, a page's own listeners) a swipe does too.
//
// A frame is its own document: a touch inside one (the panel's preview or
// page) never reaches this one, so the swipe there is the frame's.

// TabSwipeAttr marks a swipe area; its value is its tablist's id.
const TabSwipeAttr = "data-tab-swipe"

// The swipe's thresholds.
const (
	// TabSwipeMinPx is the least horizontal travel that switches a tab (the
	// larger of it and TabSwipeMinFrac of the area's width).
	TabSwipeMinPx   = 60
	TabSwipeMinFrac = 0.18
	// TabSwipeLockPx is how far a finger moves before the gesture is locked
	// to one axis; TabSwipeRatio how much more sideways than vertical it
	// must be to lock horizontal. A vertical lock never switches a tab.
	TabSwipeLockPx = 10
	TabSwipeRatio  = 1.2
	// TabSwipeEdgePx is the sidebar's open-swipe edge (components.go
	// SB_EDGE): a gesture starting inside it is the sidebar's, never a tab's.
	TabSwipeEdgePx = 28
	// TabSwipeMaxScale: with the page pinch-zoomed past this
	// (visualViewport.scale), a sideways drag is the browser's pan, never a
	// tab swipe.
	TabSwipeMaxScale = 1.01
)

// tabSwipeScriptSrc is the swipe, a static file (static_script.go), bound
// once per window by delegation.
var tabSwipeScriptSrc = staticJS("tab-swipe", `(function () {
  if (window.__musterTabSwipe) return;
  window.__musterTabSwipe = true;
  var sw = null;
  function zoomed() { var v = window.visualViewport; return !!(v && v.scale > `+strconv.FormatFloat(TabSwipeMaxScale, 'f', -1, 64)+`); }
  function ownsSideways(t) {
    for (var el = t; el && el.nodeType === 1 && el !== document.body; el = el.parentElement) {
      if (el.hasAttribute('data-own-swipe')) return true;
      if (el.scrollWidth > el.clientWidth + 1) { var ox = getComputedStyle(el).overflowX; if (ox === 'auto' || ox === 'scroll') return true; }
    }
    return false;
  }
  function tabsOf(area) {
    var l = document.getElementById(area.getAttribute('`+TabSwipeAttr+`') || '');
    return l ? Array.prototype.slice.call(l.querySelectorAll('[role="tab"]')) : [];
  }
  function selIndex(ts) { for (var i = 0; i < ts.length; i++) if (ts[i].getAttribute('aria-selected') === 'true') return i; return -1; }
  document.addEventListener('touchstart', function (e) {
    sw = null;
    if (!e.touches || e.touches.length !== 1) return;
    var t = e.target, area = t && t.closest && t.closest('[`+TabSwipeAttr+`]');
    if (!area) return;
    var x = e.touches[0].clientX, y = e.touches[0].clientY;
    if (x <= `+strconv.Itoa(TabSwipeEdgePx)+`) return;
    // Pinch-zoomed in: a sideways drag pans the zoomed page (touch-action is
    // left to the browser), never a tab swipe. Checked when the finger lands:
    // a pinch DURING a drag is two touches, which already ends the swipe.
    if (zoomed()) return;
    if (t.closest('input,textarea,select,[contenteditable],[data-asks-media],[data-asks-answer-row],[data-own-swipe]')) return;
    if (ownsSideways(t)) return;
    var s = window.getSelection && String(window.getSelection());
    if (s) return;
    sw = { x: x, y: y, lock: '', area: area };
  }, { passive: true });
  document.addEventListener('touchmove', function (e) {
    if (!sw || !e.touches || !e.touches.length) return;
    var dx = e.touches[0].clientX - sw.x, dy = e.touches[0].clientY - sw.y;
    if (!sw.lock && (Math.abs(dx) > `+strconv.Itoa(TabSwipeLockPx)+` || Math.abs(dy) > `+strconv.Itoa(TabSwipeLockPx)+`)) {
      sw.lock = Math.abs(dx) > Math.abs(dy) * `+strconv.FormatFloat(TabSwipeRatio, 'f', -1, 64)+` ? 'x' : 'y';
    }
    sw.dx = dx;
  }, { passive: true });
  function endSwipe() {
    var s = sw; sw = null;
    if (!s || s.lock !== 'x') return;
    var w = s.area.clientWidth || window.innerWidth;
    if (Math.abs(s.dx || 0) < Math.max(`+strconv.Itoa(TabSwipeMinPx)+`, w * `+strconv.FormatFloat(TabSwipeMinFrac, 'f', -1, 64)+`)) return;
    var ts = tabsOf(s.area), i = selIndex(ts), n = i + (s.dx < 0 ? 1 : -1);
    if (i < 0 || n < 0 || n >= ts.length) return;
    ts[n].click();
  }
  document.addEventListener('touchend', endSwipe, { passive: true });
  document.addEventListener('touchcancel', function () { sw = null; }, { passive: true });
})();
`)

// tabSwipeScript is the swipe's tag.
func tabSwipeScript() g.Node { return staticScript(tabSwipeScriptSrc) }

// TabSwipeScriptHTML is the swipe's tag as HTML, for a page drawn outside this
// package (the Asks page, internal/api).
func TabSwipeScriptHTML() string { return nodeString(tabSwipeScript()) }
