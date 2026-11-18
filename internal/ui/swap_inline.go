package ui

import (
	"encoding/json"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/web"
)

// A BOOSTED SWAP RUNS ITS SCRIPTS INSIDE THE SWAP, AS INLINE SCRIPTS DID.
//
// An inline <script> in a boosted body ran synchronously while htmx inserted
// the body — before htmx settled it and before the browser painted it. A
// <script src> re-created by htmx runs once its bytes are read, a task or more
// later. For a script the window has run before that changes nothing (its
// once-per-window listeners already did the swap's work). For a script this
// window has NEVER run — a boosted navigation from an agent's page, which does
// not carry the Agents filter, into /agents — it means the new body is painted
// before the script's first run: the list drew every card, then the filter hid
// most of them (e2e/tests/cls.spec.ts "boosted navigation to /agents"
// measured a layout shift of 0.48, and host-tabs.spec.ts saw every host's
// cards).
//
// So the window keeps the TEXT of every page script (fetched once it has
// loaded, from the HTTP cache — the URLs are immutable), and on a boosted body
// swap rewrites each static script tag in the incoming HTML into the inline
// <script> it stands for, attributes kept, before htmx inserts it. The swap
// then runs exactly what it ran when the scripts were inline, at the same
// point. A tag whose text the window does not hold yet (a swap in the first
// moments of a page, a URL from a build newer than the list) is left as it is
// and loads as a <script src>: the slower path, never a wrong one.
//
// ⚠ ONLY BODY SWAPS (hx-boost's target), ONLY /static/js/ URLS, AND ONLY THE
// EXACT VERSIONED URL. A partial's swap is not touched; a vendored script is
// not touched; a text is used only for the URL that names its bytes.
//
// ⚠ IT IS REGISTERED IN init(), NOT AS A PACKAGE VARIABLE, BECAUSE ITS TEXT
// LISTS EVERY OTHER PAGE SCRIPT'S URL: init() runs after every package-level
// staticJS registration, so the list is complete — and the same for every
// request, as a static file must be.
var swapInlineSrc string

func init() {
	var urls []string
	for _, p := range web.Generated() {
		urls = append(urls, web.Asset(p))
	}
	list, _ := json.Marshal(urls)
	swapInlineSrc = staticJS("swap-inline", `
(function () {
  if (window.__musterSwapInline) return;
  window.__musterSwapInline = true;
  var URLS = `+string(list)+`;
  var texts = {};
  // warm fetches each text the window does not hold and is not fetching. A
  // failed fetch forgets its URL, so the next boosted swap asks again.
  //
  // Only a response the server marks immutable is kept: it says so only for
  // the URL naming its current bytes, so in a rollout a pod of the other
  // build answering this URL with its own bytes (200, no-cache) is not
  // inlined under it.
  function warm() {
    URLS.forEach(function (u) {
      if (texts[u] !== undefined) return;
      texts[u] = null;
      try {
        fetch(u, { credentials: 'same-origin' })
          .then(function (r) { return r.ok && /immutable/.test(r.headers.get('Cache-Control') || '') ? r.text() : null; })
          .then(function (t) { if (typeof t === 'string') texts[u] = t; else delete texts[u]; })
          .catch(function () { delete texts[u]; });
      } catch (e) { delete texts[u]; }
    });
  }
  if (document.readyState === 'complete') warm(); else window.addEventListener('load', warm);
  // This text never spells a closing script tag, so it can itself be inlined.
  var END = '<\/script>';
  var TAG = new RegExp('<script src="(/static/js/[^"?]+[.]js[?]v=[0-9a-f]+)"([^>]*)>' + END, 'g');
  document.addEventListener('htmx:beforeSwap', function (e) {
    var d = e.detail;
    if (!d || !d.shouldSwap || d.target !== document.body || typeof d.serverResponse !== 'string') return;
    warm();
    d.serverResponse = d.serverResponse.replace(TAG, function (all, u, attrs) {
      var t = texts[u];
      if (typeof t !== 'string' || t.toLowerCase().indexOf('<\/script') >= 0) return all;
      return '<script' + attrs + '>' + t + END;
    });
  });
})();
`)
}

// swapInlineScript is the tag for swapInlineSrc. Every document that carries
// pwaChrome — every boosted document — carries it.
func swapInlineScript() g.Node { return staticScript(swapInlineSrc) }
