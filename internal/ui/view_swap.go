package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE RENDERED ↔ TERMINAL SWITCH KEEPS BOTH VIEWS ALIVE — a mirrored window's
// two views, and a claude-code agent's chat page and terminal.
//
// Each view stays exactly what it was: its own route, its own DOCUMENT, its own
// scripts, socket and pagehide. The switch used to be a real load each way, so
// every tap closed one host stream and opened another — and coming back opened
// ANOTHER, the transcript read again, a new tmux or pod attach.
//
// Now the first tap opens the other view in a same-origin <iframe> laid over
// the whole page (viewFrameScript), and from then on a tap only shows or hides
// that frame. The page underneath is never unloaded and the frame is never
// reloaded, so neither view's socket is touched: ONE stream per view for as long
// as the person stays on the window.
//
//   - 🔴 THE TERMINAL STILL NEVER LIVES IN THE AGENT PAGE'S hx-boost LIFECYCLE
//     (AgentTerminalPage): over an agent page it is a frame — its own document,
//     which a boosted swap of the page cannot re-run. A boosted navigation AWAY
//     swaps the frame out with the body, which unloads it and closes its socket.
//   - The view off screen is hidden with visibility, not detached and not
//     display:none: it keeps its layout, its scroll position and its live
//     socket, and nothing in it is re-measured. There is no time limit — a view
//     closes its stream only when the window is left (another window, the
//     list, a reload: the page's and the frame's own pagehide).
//   - The ADDRESS FOLLOWS THE VIEW on screen (history.replaceState on the top
//     window), so a reload or a shared link opens that view, and Back leaves
//     the window (a switch adds no history entry). The tab title follows too.
//   - A link in the framed view that leaves the window navigates the TOP window;
//     one back to the view underneath just shows it.
//   - Framing is allowed for these four routes only, and only by this origin
//     (api's allowSameOriginFrame); every other page keeps frame-ancestors
//     'none'.
//   - Anything that goes wrong opening the frame (it lands on another page — a
//     sign-in, a window with no rendered view any more) falls back to the real
//     load the link always was.
//
// Instruments for the browser suite, on the top <html>: data-view (the view on
// screen) once a frame has been opened.

// ViewFrameAttr marks the frame that holds the other view; its value is that
// view's name (ViewRendered, ViewTerminal).
const ViewFrameAttr = "data-muster-view"

// viewFrameScript is the switch's machinery. It is in the BODY of every page
// that draws a swapping switch — so it also runs when an agent page arrives by
// a boosted swap — and binds once per document. The same script runs in the
// top page and in the framed view, and takes the role its window has.
func viewFrameScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.musterViews) return;
  var ATTR = '` + ViewFrameAttr + `';
  var framed = false;
  try { framed = !!(window.frameElement && window.frameElement.hasAttribute(ATTR) && window.parent.musterViews); } catch (e) {}
  var root = document.documentElement;

  function frame() { return document.querySelector('iframe[' + ATTR + ']'); }
  function key(u) { try { var x = new URL(u, location.href); return x.pathname + x.search; } catch (e) { return ''; } }
  function sameView(href, own) {
    try {
      var a = new URL(href, location.href), b = new URL(own, location.href);
      if (a.origin !== b.origin || a.pathname !== b.pathname) return false;
      var same = true;
      a.searchParams.forEach(function (v, k) { if (b.searchParams.get(k) !== v) same = false; });
      return same;
    } catch (e) { return false; }
  }
  function inertAll(on, f) {
    Array.prototype.forEach.call(document.body.children, function (el) {
      if (el === f) return;
      if (on && !el.inert) { el.inert = true; el.setAttribute('data-view-inert', ''); }
      if (!on && el.hasAttribute('data-view-inert')) { el.inert = false; el.removeAttribute('data-view-inert'); }
    });
  }
  // reveal puts the frame on screen, over the page.
  function reveal(f) {
    f.style.visibility = '';
    f.removeAttribute('aria-hidden');
    f.inert = false;
    inertAll(true, f);
    try {
      history.replaceState(history.state, '', f.contentWindow.location.href);
      document.title = f.contentDocument.title;
    } catch (e) {}
    root.setAttribute('data-view', f.getAttribute(ATTR));
    try { f.contentWindow.focus(); } catch (e) {}
  }
  // home shows the page's own view again: the frame stays, hidden and live.
  function home(f) {
    f.style.visibility = 'hidden';
    f.setAttribute('aria-hidden', 'true');
    f.inert = true;
    inertAll(false, f);
    history.replaceState(history.state, '', f.getAttribute('data-own-url'));
    document.title = f.getAttribute('data-own-title');
    root.setAttribute('data-view', f.getAttribute('data-own'));
  }

  // show puts view name on screen (the top window's half). href is the view's
  // address, for the first time it is opened.
  function show(name, href) {
    var f = frame();
    if (f) {
      if (name === f.getAttribute('data-own')) { if (!f.style.visibility) home(f); return; }
      if (name === f.getAttribute(ATTR)) { if (f.style.visibility && f.hasAttribute('data-loaded')) reveal(f); return; }
    }
    if (f || !href) { location.assign(href || location.href); return; }
    var want = key(href).split('?')[0];
    f = document.createElement('iframe');
    f.setAttribute(ATTR, name);
    f.setAttribute('data-own', name === '` + ViewTerminal + `' ? '` + ViewRendered + `' : '` + ViewTerminal + `');
    f.setAttribute('data-own-url', location.href);
    f.setAttribute('data-own-title', document.title);
    f.setAttribute('title', name === '` + ViewTerminal + `' ? 'Terminal' : 'Rendered view');
    // Over everything, the size of the viewport, invisible until it has loaded:
    // until then the page under it is what is on screen.
    f.style.cssText = 'position:fixed;inset:0;width:100%;height:100%;border:0;margin:0;padding:0;z-index:2147483000;visibility:hidden';
    f.setAttribute('aria-hidden', 'true');
    var first = true;
    f.addEventListener('load', function () {
      if (!first) return;
      first = false;
      var ok = false;
      try { ok = f.contentWindow.location.pathname === want; } catch (e) {}
      // Not the view (a sign-in, a redirect elsewhere): the real load decides.
      if (!ok) { location.assign(href); return; }
      f.setAttribute('data-loaded', '');
      reveal(f);
    });
    f.src = href;
    document.body.appendChild(f);
  }

  // The top window's API, which a framed view calls.
  window.musterViews = {
    show: show,
    // showOwn: whether href is the page's own view — a link there from the frame
    // shows it instead of loading it again.
    // The own view's address may carry more than a link to it does (an agent
    // page's ?session=): a link whose path is the same and whose every query
    // value the own address has is a link to it.
    showOwn: function (href) {
      var f = frame();
      if (!f || !sameView(href, f.getAttribute('data-own-url'))) return false;
      show(f.getAttribute('data-own'));
      return true;
    }
  };

  function plain(e, a) {
    return !(e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey ||
      (a.target && a.target !== '_self') || a.hasAttribute('download'));
  }

  // The switch, in either window.
  document.addEventListener('click', function (e) {
    var a = e.target && e.target.closest ? e.target.closest('a[data-view-swap]') : null;
    if (!a || !plain(e, a)) return;
    e.preventDefault();
    e.stopPropagation();
    var top = framed ? window.parent.musterViews : window.musterViews;
    top.show(a.getAttribute('data-view-swap'), a.href);
  }, true);

  if (!framed) return;
  // 🔴 IN THE FRAME, A LINK THAT LEAVES THE VIEW NAVIGATES THE TOP WINDOW. Read
  // after every other handler (window, bubbling): a link something else
  // already handled is left alone; an in-page one (#…) too.
  function leave(href) {
    if (!window.parent.musterViews.showOwn(href)) window.parent.location.assign(href);
  }
  window.addEventListener('click', function (e) {
    var a = e.target && e.target.closest ? e.target.closest('a[href]') : null;
    if (!a || !plain(e, a) || a.hasAttribute('data-view-swap')) return;
    var href = a.getAttribute('href') || '';
    if (href.charAt(0) === '#' || /^javascript:/i.test(href)) return;
    e.preventDefault();
    leave(a.href);
  });
  // A boosted link (the agent page's): htmx takes the click first, so its
  // request is what is stopped — the TOP window goes there instead.
  document.addEventListener('htmx:beforeRequest', function (e) {
    var d = e.detail || {};
    if (!d.boosted || !d.elt || d.elt.tagName !== 'A') return;
    e.preventDefault();
    leave(d.elt.href);
  });
})();
`))
}
