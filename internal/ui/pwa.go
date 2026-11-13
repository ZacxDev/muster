package ui

import (
	"io"
	"net/url"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE INSTALLED-APP SURFACE: manifest link, service worker, update toast,
// install button, app badge, and the composer a share or shortcut opens.
// ---------------------------------------------------------------------------

// docHead is the head furniture every document carries: the manifest link,
// and the favicon and touch icon (brandIconLinks — the logo's when one is set,
// muster's own otherwise).
//
// 🔴 ONE HELPER, SO A DOCUMENT CANNOT HAVE ONE WITHOUT THE OTHER. In 0.187.0
// every document head spelled manifestLink() and only two of them also spelled
// brandIconLinks, so the landing page (the chief's agent page) had no favicon
// at all. Every document head calls this, and nothing else calls the two
// (TestDocumentHeadsHaveOneSpelling, TestEveryDocumentLinksTheBrandIcon).
func docHead(b Brand) g.Node {
	return g.Group{manifestLink(), brandIconLinks(b)}
}

// DocHeadHTML is docHead as a raw string, for internal/api's hand-written
// documents (writeAuthDocument: sign-in, claim, invite, reset, notice), the way
// ThemeHeadHTML carries ThemeHead there. It renders docHead itself, so the
// link markup has one spelling.
//
// The manifest link is included on those signed-out pages on purpose: the
// manifest is the same for every caller and needs no session
// (internal/api's TestPWAMetadataIsSessionIndependent), so linking it from a
// signed-out page exposes nothing. A link does not change what a forward-auth
// proxy lets through (docs/auth.md keeps /manifest.webmanifest behind its
// sign-in): the browser's fetch of it is answered by the proxy like any other.
func DocHeadHTML(b Brand) string {
	var sb strings.Builder
	_ = docHead(b).Render(&sb)
	return sb.String()
}

// manifestLink is the <link rel=manifest>; documents get it through docHead.
//
// 🔴 crossorigin="use-credentials" IS LOAD-BEARING, AND THE REASON IS BLINK, NOT
// THE EDGE. Chromium fetches the manifest in CORS mode with credentials OMITTED
// unless this attribute says otherwise — even same-origin. Without it the
// manifest request carries no session cookie at all. (A deployment behind a
// forward-auth proxy should let muster's PWA metadata through without a
// session — GET/HEAD on exactly /manifest.webmanifest, /static/icons/*.png and
// /sw.js: the WebAPK icon hasher never
// sends cookies, so an icon behind the proxy degrades the install to a plain
// shortcut. muster's side of that bargain is that those responses never depend
// on a session and never carry per-user data;
// internal/api's TestPWAMetadataIsSessionIndependent pins it.)
func manifestLink() g.Node {
	return Link(Rel("manifest"), Href("/manifest.webmanifest"), g.Attr("crossorigin", "use-credentials"))
}

// pwaChrome is the per-document PWA furniture: the update toast and the
// script that owns the worker. Every document renders it, so an update is
// offered and the badge stays true wherever the operator happens to be. It
// also carries swapInlineScript, so every boosted document runs a swap's
// scripts inside the swap (swap_inline.go).
func pwaChrome() g.Node {
	return g.Group{updateToast(), pwaScript(), swapInlineScript()}
}

// buildVersion is the running server build (api.BuildVersion), installed once
// by main through SetBuildVersion. The update pill shows it as the version the
// page is ON. Empty (a binary that never set it) renders no versions at all:
// the pill then reads "Update available".
var buildVersion string

// SetBuildVersion installs the running build the update pill names. Called
// once from main at startup, before any page is rendered.
func SetBuildVersion(v string) { buildVersion = v }

// releaseVersionPattern is what a RELEASE build version looks like: the
// X.Y.Z the release flow stamps from a vX.Y.Z tag (ci.yml image-muster). A
// main push is stamped with a short sha, a local build with "dev": those have
// no release page, so they get no link. pwaScript is handed this text as a
// regex literal and tests the waiting worker's version with it: only the
// version being MOVED TO links to its notes; the current one is plain text.
const releaseVersionPattern = `^[0-9]+\.[0-9]+\.[0-9]+$`

// releaseNotesURL is where a release's notes live; the release flow pushes the
// public tag v<version> and a GitHub Release for it.
const releaseNotesURL = "https://github.com/ZacxDev/muster/releases/tag/v"

// releaseLinkClass styles the new version's release-notes link, drawn by
// pwaScript (spliced in, so Tailwind sees the classes in this file). 44px
// tall: it is a tap target.
const releaseLinkClass = "inline-flex min-h-[44px] items-center font-semibold text-accent underline decoration-dotted underline-offset-2 hover:decoration-solid"


// reloadIcon is the circular arrow the Reload button draws.
const reloadIcon = `<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/></svg>`

// updateToast is the "new version" prompt. Hidden until pwaScript sees a
// waiting worker; its Reload button hands control to that worker.
//
// It reads "Update <current> → <new>" (operator, 2026-11-12). The current
// version is rendered here (data-sw-current, and the from-version); the new
// one is asked of the WAITING worker by pwaScript (sw.js answers GET_VERSION
// with the build the server stamped into it) and drawn into [data-sw-to]. Until
// then — or when the worker does not answer — the pill says "Update available"
// ([data-sw-label]), and Reload works either way.
//
// 🔴 RELOAD IS AN ICON BUTTON (a circular arrow, aria-label "Reload to update",
// 44×44) and still carries data-sw-reload: pwaScript's click handler is what
// it triggers.
//
// The pill can be dragged by its body (pwaScript: pointer events with a
// movement threshold, so a tap on a button or the link is still a tap); the
// spot is remembered per device and overrides the corner below.
//
// 🔴 THE NEW WORKER WAITS FOR THIS BUTTON. sw.js no longer calls skipWaiting()
// at install, so an open page is never silently switched to a worker whose
// scripts it does not have. Reload is the operator's decision.
//
// 🔴 IT IS A COMPACT PILL IN THE TOP-RIGHT CORNER — NOT A BAR AT THE BOTTOM. It used to float 5.5rem above the bottom edge, which
// on a phone is exactly where the composer, Send, the FAB and the chat button
// live: the operator reported it "blocking the UI", and chose this shape
// ("Updated · Reload ×", top-right; since 2026-11-12 "Update a → b ⟳ ×", and
// draggable — a remembered spot wins over the corner). The classes below put it in the corner
// under the safe-area insets; pwaScript's placeToast slides it DOWN past any
// control it would sit on (a header icon, a filter chip) before it is shown. It is `fixed` and sized to its content, so it takes no layout space,
// showing it shifts nothing, and it blocks taps only inside its own box.
//
// ⚠ z-[35] IS BETWEEN THE FAB/BACKDROP (z-30) AND THE SIDEBAR PANEL (z-40): an
// open sidebar slides OVER the pill, so the pill never covers its links.
func updateToast() g.Node {
	return updateToastFor(buildVersion)
}

// updateToastFor is updateToast for a given running build (tests pass one).
func updateToastFor(current string) g.Node {
	var versions g.Node
	if current != "" {
		versions = Span(
			g.Attr("data-sw-versions", ""),
			g.Attr("hidden", ""),
			Class("whitespace-nowrap"),
			g.Text("Update "), Span(g.Attr("data-sw-from", ""), Class("font-semibold"), g.Text(current)), g.Text(" → "), Span(g.Attr("data-sw-to", "")),
		)
	}
	return Div(
		ID("sw-update-toast"),
		g.Attr("role", "status"),
		g.Attr("aria-live", "polite"),
		g.Attr("hidden", ""),
		g.Attr("data-sw-current", current),
		// touch-none: a touch drag on the pill moves the pill, not the page.
		// select-none: a mouse drag does not select its text.
		Class("fixed right-[calc(env(safe-area-inset-right)+0.75rem)] top-[calc(env(safe-area-inset-top)+0.5rem)] z-[35] flex cursor-grab touch-none select-none items-center rounded-full bg-s3 pl-3 text-xs text-fg shadow-lg shadow-black/40 ring-1 ring-inset ring-edge data-[sw-dragging]:cursor-grabbing"),
		Span(g.Attr("data-sw-label", ""), Class("whitespace-nowrap"), g.Text("Update available")),
		versions,
		Button(
			Type("button"),
			g.Attr("data-sw-reload", ""),
			g.Attr("aria-label", "Reload to update"),
			g.Attr("title", "Reload to update"),
			Class("press ml-1 inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-full text-accent transition hover:bg-s2"),
			g.Raw(reloadIcon),
		),
		Button(
			Type("button"),
			g.Attr("data-sw-dismiss", ""),
			g.Attr("aria-label", "Dismiss"),
			Class("press inline-flex min-h-[44px] min-w-[36px] items-center justify-center rounded-r-full pr-1 text-muted transition hover:text-fg"),
			g.Raw(`<svg class="h-3.5 w-3.5" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8"/></svg>`),
		),
	)
}

// installButton is the sidebar's "Install app" control. Hidden until the
// browser fires beforeinstallprompt (pwaScript), and never shown once the app
// runs in ANY installed display mode (pwaScript's installed()) or has been
// installed. It is rendered `hidden`, so a page whose script never runs — or
// runs inside the app — never shows it.
func installButton() g.Node {
	return Button(
		Type("button"),
		g.Attr("data-install-app", ""),
		g.Attr("hidden", ""),
		Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium text-accent transition hover:bg-s2"),
		g.Raw(`<svg class="h-4 w-4 shrink-0" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 2v8m-3.5-3.5L8 10l3.5-3.5M3 13h10"/></svg>`),
		Span(g.Text("Install app")),
	)
}

// reviewCountPath is the session-gated count the desktop app badge reads.
const reviewCountPath = "/ui/tasks/review-count"

// AppBridgeName is the JavaScript object the Android app's WebView injects
// (WebViewCompat.addWebMessageListener), on muster's own origin only. Its
// presence is how a page knows it is inside the app; the page posts the
// pairing code and "open the app's notification settings" to it, and never
// receives a credential from it. The app repository uses the same name.
const AppBridgeName = "musterApp"

// pwaScript is the ONE service-worker registration site, plus the three things
// that hang off the registration: the update toast, the install button and the
// app badge. It is idempotent per page load (a boosted navigation re-runs body
// scripts) and looks elements up at use time, because a boosted swap replaces
// the toast and the button underneath it.
//
// ⚠ ORDER: it publishes window.musterSWReady, which pushScript consumes, so it
// renders before pushScript on the shell.
func pwaScript() g.Node { return staticScript(pwaScriptSrc) }

// pwaScriptSrc is the path pwaScript's code is served at, /static/js/pwa.js (static_script.go).
var pwaScriptSrc = staticJS("pwa", `
(function () {
  if (window.__musterPWA) return;
  window.__musterPWA = true;
  if (!('serviceWorker' in navigator)) return;

  // --- registration ---------------------------------------------------------
  // updateViaCache:'none' is explicit; the top-level script already bypasses
  // the HTTP cache by default. Every deploy changes sw.js's bytes (the server
  // stamps the build into it), so every deploy is an update.
  var registered = navigator.serviceWorker.register('/sw.js', { scope: '/', updateViaCache: 'none' });
  window.musterSWReady = registered;

  // --- update toast -----------------------------------------------------------
  var wantReload = false;
  var dismissed = null; // the waiting worker the operator dismissed the toast for
  // placeToast puts the pill in the top-right corner, under the safe-area
  // inset — and then, if a control sits there (a header icon, a filter chip),
  // slides it DOWN the right edge to the first spot where it covers no visible
  // control. On the shell pages (/, /tasks, /agents) the header's right end is
  // empty, so the pill sits there; on an agent chat page the header's icons push
  // it below the header. (Measured at 390 wide: BELOW the header on /tasks the
  // right column has no free 44px slot at all — chips, status filters, Select,
  // then the cards — so "always under the header" would always cover a control.)
  // With no free spot anywhere, the spot covering the least is used. It runs
  // while the pill is laid out but invisible (visibility:hidden), BEFORE it is
  // shown, so the pill appears in its final place and nothing moves.
  //
  // 🔴 A PROPOSAL CARD IS AVOIDED WHOLE ([data-proposal], ui.ProposalCard), not
  // only its buttons: its kickoff and its "what happens" lines ARE what the
  // owner is deciding on, and the pill once sat on a succession card's
  // kickoff and an end card's subtitle at phone width. The card arrives after
  // the page settled (the view's own fetch, no htmx swap), so the view's
  // script says so ('muster:layout') and a pill on it moves off.
  var CONTROLS = 'a[href], button, input, select, textarea, [role=button], [tabindex], [data-proposal]';
  // controlBoxes: the boxes of visible controls (and proposal cards) in the
  // pill's column.
  function controlBoxes(t, pill) {
    var boxes = [];
    var all = document.querySelectorAll(CONTROLS);
    for (var i = 0; i < all.length; i++) {
      var e = all[i];
      if (t.contains(e)) continue;
      var r = e.getBoundingClientRect();
      if (!r.width || !r.height || r.right <= pill.left || r.left >= pill.right) continue;
      if (getComputedStyle(e).visibility === 'hidden') continue;
      boxes.push(r);
    }
    return boxes;
  }
  // overlap: how much of the controls' area a pill at [top, top+height) would
  // cover (with a 4px margin), 0 when it covers none.
  function overlap(boxes, pill, top, height) {
    var sum = 0;
    for (var j = 0; j < boxes.length; j++) {
      var b = boxes[j];
      var dy = Math.min(b.bottom + 4, top + height) - Math.max(b.top - 4, top);
      if (dy <= 0) continue;
      sum += dy * (Math.min(b.right, pill.right) - Math.max(b.left, pill.left));
    }
    return sum;
  }
  // 🔴 A SPOT THE OPERATOR DRAGGED THE PILL TO WINS (savedPos): it is used,
  // clamped to the viewport, instead of the corner and the slide-down below.
  // With none saved, the corner and the slide-down apply as before.
  function placeToast(t) {
    var vis = t.style.visibility;
    var wasHidden = t.hidden;
    t.style.top = '';
    t.style.left = '';
    t.style.right = '';
    t.style.visibility = 'hidden';
    t.hidden = false;
    var saved = savedPos();
    if (saved) {
      applyPos(t, clampPos(t, saved));
      t.hidden = wasHidden;
      t.style.visibility = vis;
      return;
    }
    var pill = t.getBoundingClientRect();
    var boxes = controlBoxes(t, pill);
    var best = pill.top, bestCost = Infinity, limit = window.innerHeight - pill.height - 8;
    for (var top = pill.top; top <= limit; top += 4) {
      var cost = overlap(boxes, pill, top, pill.height);
      if (cost < bestCost) { best = top; bestCost = cost; }
      if (cost === 0) break;
    }
    t.style.top = best + 'px';
    t.hidden = wasHidden;
    t.style.visibility = vis;
  }
  // 🔴 THE PILL IS PLACED ONCE THE PAGE HAS SETTLED, NOT AT REGISTRATION. The
  // registration resolves before the page's htmx "load" fragments (the task
  // list, its label chips) have arrived; a pill placed then sat on a chip that
  // landed a moment later. So the first show waits for the document's load
  // event and for no htmx request in flight (at most ~3 s, then shows anyway).
  function whenSettled(fn) {
    var tries = 0;
    (function check() {
      var busy = document.readyState !== 'complete' || document.querySelector('.htmx-request');
      if (busy && ++tries < 30) { setTimeout(check, 100); return; }
      fn();
    })();
  }
  // --- the versions ----------------------------------------------------------
  // The NEW version is asked of the waiting worker itself: sw.js answers
  // {type:'GET_VERSION'} on the port it is handed with {build}, the build the
  // server stamped into those bytes. No network request. A worker that does
  // not answer within VERSION_ASK_MS (an older sw.js, a worker that went
  // redundant) leaves the pill's "Update available" label — the pill is shown
  // either way, and Reload never waits on this. The answer is kept per worker,
  // so a re-offer after a swap draws at once.
  var VERSION_ASK_MS = 1000;
  var RELEASE_RE = /` + releaseVersionPattern + `/;
  var asked = { w: null, v: null };
  function workerVersion(w, done) {
    if (!w || typeof w.postMessage !== 'function') return done(null);
    if (asked.w === w) return done(asked.v);
    var over = false;
    var finish = function (v) {
      if (over) return;
      over = true;
      asked = { w: w, v: v };
      done(v);
    };
    var timer = setTimeout(function () { finish(null); }, VERSION_ASK_MS);
    try {
      var ch = new MessageChannel();
      ch.port1.onmessage = function (e) {
        clearTimeout(timer);
        var b = e.data && e.data.build;
        finish(typeof b === 'string' && b ? b : null);
      };
      w.postMessage({ type: 'GET_VERSION' }, [ch.port2]);
    } catch (err) { clearTimeout(timer); finish(null); }
  }
  // drawVersions: "Update <current> → <new>" when both are known and differ,
  // the plain label otherwise. Only the new version can link: to its release
  // notes, when it is a release (RELEASE_RE). The current one is plain text.
  function drawVersions(t, v) {
    var label = t.querySelector('[data-sw-label]');
    var vs = t.querySelector('[data-sw-versions]');
    var to = t.querySelector('[data-sw-to]');
    var cur = t.getAttribute('data-sw-current') || '';
    var on = !!(v && vs && to && cur && v !== cur);
    if (on) {
      var n;
      if (RELEASE_RE.test(v)) {
        n = document.createElement('a');
        n.href = '` + releaseNotesURL + `' + v;
        n.target = '_blank';
        n.rel = 'noopener noreferrer';
        n.className = '` + releaseLinkClass + `';
        n.setAttribute('data-sw-release', v);
        n.setAttribute('aria-label', 'Release notes for ' + v);
      } else {
        n = document.createElement('span');
        n.className = 'font-semibold';
      }
      n.textContent = v;
      to.textContent = '';
      to.appendChild(n);
    }
    if (label) label.hidden = on;
    if (vs) vs.hidden = !on;
  }

  // --- the dragged spot ---------------------------------------------------------
  // The pill can be dragged by its body; where it is let go is remembered for
  // this device (localStorage) and used for every later offer (placeToast).
  // 🔴 EVERY STORAGE ACCESS IS IN A try: a private window or blocked site data
  // throws on the accessor. Then the spot lives for this page only (memPos).
  var POS_KEY = 'muster.updatePill.pos';
  var memPos = null;
  function savedPos() {
    try {
      var p = JSON.parse(localStorage.getItem(POS_KEY) || 'null');
      if (p && typeof p.left === 'number' && typeof p.top === 'number' && isFinite(p.left) && isFinite(p.top)) return { left: p.left, top: p.top };
    } catch (err) {}
    return memPos;
  }
  function savePos(p) {
    memPos = p;
    try { localStorage.setItem(POS_KEY, JSON.stringify(p)); } catch (err) {}
  }
  // insets: the safe-area insets in px, read off a probe (env() has no JS API).
  function insets() {
    var p = document.createElement('div');
    p.style.cssText = 'position:fixed;top:0;left:0;visibility:hidden;pointer-events:none;' +
      'padding:env(safe-area-inset-top) env(safe-area-inset-right) env(safe-area-inset-bottom) env(safe-area-inset-left)';
    document.body.appendChild(p);
    var s = getComputedStyle(p);
    var r = { t: parseFloat(s.paddingTop) || 0, r: parseFloat(s.paddingRight) || 0, b: parseFloat(s.paddingBottom) || 0, l: parseFloat(s.paddingLeft) || 0 };
    p.remove();
    return r;
  }
  // clampPos: p moved the least distance that puts the whole pill inside the
  // viewport, inside the safe-area insets. A pill wider (taller) than the room
  // sits at the left (top) inset.
  function clampPos(t, p) {
    var b = t.getBoundingClientRect(), i = insets();
    var vw = Math.min(document.documentElement.clientWidth || window.innerWidth, window.innerWidth);
    var vh = window.innerHeight;
    return {
      left: Math.max(i.l, Math.min(p.left, vw - i.r - b.width)),
      top: Math.max(i.t, Math.min(p.top, vh - i.b - b.height)),
    };
  }
  function applyPos(t, p) {
    t.style.left = p.left + 'px';
    t.style.top = p.top + 'px';
    t.style.right = 'auto';
  }
  // 🔴 A DRAG STARTS ONLY PAST DRAG_THRESHOLD px, AND NEVER ON A BUTTON. Below
  // the threshold nothing moves and the tap is an ordinary click (the release
  // link opens); a press on Reload or × is never a drag at all. Pointer events,
  // so a mouse and a finger take the one path; the pill is touch-action:none
  // (touch-none), so a finger drags the pill and not the page. Once a drag
  // began, the click its release would fire is swallowed (swallowDragClick).
  var DRAG_THRESHOLD = 6;
  var drag = null;
  function dragStart(e) {
    var t = e.currentTarget;
    if (t.hidden || !e.target || !t.contains(e.target)) return;
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    if (e.target.closest('button')) return;
    var b = t.getBoundingClientRect();
    drag = { t: t, id: e.pointerId, x: e.clientX, y: e.clientY, left: b.left, top: b.top, moved: false };
  }
  // 🔴 THE PILL ITSELF LISTENS FOR click, NOT ONLY THE DOCUMENT. Chromium's
  // touch adjustment moves a touch that lands on a node that does not respond
  // to a tap to the nearest node that does — and a pointerdown listener does
  // not count, a click listener does. Without one, a finger on the pill's body
  // was retargeted to the header's wordmark link beside it (measured at a
  // 390-wide phone): the drag never began and the tap navigated. The click
  // listener has a job of its own: it swallows the click that ends a drag, so
  // letting go over the release link does not open it. The pill is swapped
  // with the body on a boosted navigation, so each pill is bound when it is
  // offered (bindDrag, once per element).
  var justDragged = false;
  function swallowDragClick(e) {
    if (!justDragged) return;
    e.preventDefault();
    e.stopPropagation();
  }
  function bindDrag(t) {
    if (t.__musterDrag) return;
    t.__musterDrag = true;
    t.addEventListener('pointerdown', dragStart);
    t.addEventListener('click', swallowDragClick, true);
  }
  document.addEventListener('pointermove', function (e) {
    if (!drag || e.pointerId !== drag.id) return;
    var dx = e.clientX - drag.x, dy = e.clientY - drag.y;
    if (!drag.moved) {
      if (Math.abs(dx) < DRAG_THRESHOLD && Math.abs(dy) < DRAG_THRESHOLD) return;
      drag.moved = true;
      try { drag.t.setPointerCapture(e.pointerId); } catch (err) {}
      drag.t.setAttribute('data-sw-dragging', '');
    }
    e.preventDefault();
    applyPos(drag.t, clampPos(drag.t, { left: drag.left + dx, top: drag.top + dy }));
  });
  function endDrag(e) {
    if (!drag || e.pointerId !== drag.id) return;
    var d = drag;
    drag = null;
    if (!d.moved) return;
    d.t.removeAttribute('data-sw-dragging');
    var b = d.t.getBoundingClientRect();
    savePos({ left: b.left, top: b.top });
    justDragged = true;
    setTimeout(function () { justDragged = false; }, 0);
  }
  document.addEventListener('pointerup', endDrag);
  document.addEventListener('pointercancel', endDrag);

  function offerUpdate(w) {
    if (w && w === dismissed) return;
    var t = document.getElementById('sw-update-toast');
    if (!t || !t.hidden) return;
    whenSettled(function () {
      workerVersion(w, function (v) {
        var t2 = document.getElementById('sw-update-toast');
        if (!t2 || !t2.hidden || (w && w === dismissed)) return;
        // Drawn BEFORE placing: the label sets the pill's width, so the pill
        // is placed at its final size and shown once, without moving.
        drawVersions(t2, v);
        bindDrag(t2);
        placeToast(t2);
        t2.hidden = false;
      });
    });
  }
  // A swap that lands a control UNDER a visible pill (a list refresh) moves the
  // pill off it. Only then: an unprompted move is a layout shift, so a pill
  // that covers nothing is never touched. A pill at a dragged spot stays there
  // — only re-clamped, so a resize cannot leave it off screen (a rotation
  // fires resize too, so it needs no listener of its own).
  function replaceIfCovering() {
    var t = document.getElementById('sw-update-toast');
    if (!t || t.hidden) return;
    var saved = savedPos();
    if (saved) { applyPos(t, clampPos(t, saved)); return; }
    var pill = t.getBoundingClientRect();
    if (overlap(controlBoxes(t, pill), pill, pill.top, pill.height) > 0) placeToast(t);
  }
  document.addEventListener('htmx:afterSettle', replaceIfCovering);
  window.addEventListener('resize', replaceIfCovering);
  // A region drawn by a page's own script (the view's proposal card), and a
  // disclosure opened or closed (the card's kickoff), move what is under it.
  document.addEventListener('muster:layout', replaceIfCovering);
  document.addEventListener('toggle', replaceIfCovering, true);
  registered.then(function (reg) {
    if (reg.waiting && navigator.serviceWorker.controller) offerUpdate(reg.waiting);
    // A boosted navigation swaps the body, and the toast with it: the new one
    // arrives from the server hidden while the update is still waiting. Offer
    // it again whenever a waiting worker exists.
    document.addEventListener('htmx:load', function () {
      if (reg.waiting && navigator.serviceWorker.controller) offerUpdate(reg.waiting);
    });
    reg.addEventListener('updatefound', function () {
      var w = reg.installing;
      if (!w) return;
      w.addEventListener('statechange', function () {
        // installed + an existing controller = an UPDATE waiting. With no
        // controller this is the first install, which needs no prompt.
        if (w.state === 'installed' && navigator.serviceWorker.controller) offerUpdate(w);
      });
    });
    // An installed app resumes from the background rather than navigating, so
    // the browser's own navigation-time check rarely runs. Ask on every resume,
    // at most once a minute; a throttled resume does not move the window.
    var last = 0;
    document.addEventListener('visibilitychange', function () {
      if (document.visibilityState !== 'visible' || Date.now() - last < 60 * 1000) return;
      last = Date.now();
      reg.update().catch(function () {});
    });
  }).catch(function () {});
  // ONE reload, and only one the operator asked for: the first install's
  // clients.claim() also fires controllerchange, and must not reload the page.
  function reloadOnce() {
    if (!wantReload) return;
    wantReload = false;
    location.reload();
  }
  navigator.serviceWorker.addEventListener('controllerchange', reloadOnce);

  // --- a notification tap the worker cannot navigate --------------------------
  // sw.js focusOrOpen cannot navigate() a tab it does not control (after a
  // hard reload, say), so it asks the page to go: {type:'NAVIGATE', url} with
  // a port the page answers on. Only a same-origin url is followed. The answer
  // tells the worker not to open a window instead.
  navigator.serviceWorker.addEventListener('message', function (e) {
    var m = e.data || {};
    if (m.type !== 'NAVIGATE' || typeof m.url !== 'string') return;
    var u;
    try { u = new URL(m.url, location.href); } catch (err) { return; }
    if (u.origin !== location.origin) return;
    if (e.ports && e.ports[0]) e.ports[0].postMessage({ ok: true });
    location.assign(u.href);
  });
  navigator.serviceWorker.startMessages();
  // 🔴 RELOAD ASKS THE REGISTRATION WHAT IS WAITING *NOW*, AND ALWAYS RELOADS.
  // This used to post SKIP_WAITING to the worker captured when the toast was
  // shown, and then wait for a controllerchange. When ANOTHER client — a second
  // tab, the installed app beside a browser tab — took the update first, that
  // worker was already the active one: skipWaiting() did nothing, no
  // controllerchange came, and the tap disabled the button and reloaded
  // nothing (e2e/tests/update.spec.ts, "another client took the update"). So:
  // nothing waiting -> reload straight away (the page is an old document under
  // the new worker); something waiting -> SKIP_WAITING and reload on
  // controllerchange, with a timed fallback so a worker that never activates
  // cannot leave the tap dead.
  var RELOAD_FALLBACK_MS = 3000;
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('[data-sw-reload]');
    if (!b || wantReload) return;
    wantReload = true;
    b.disabled = true;
    setTimeout(reloadOnce, RELOAD_FALLBACK_MS);
    registered.then(function (reg) {
      if (!reg.waiting) return reloadOnce();
      reg.waiting.postMessage({ type: 'SKIP_WAITING' });
    }, reloadOnce);
  });
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('[data-sw-dismiss]');
    if (!b) return;
    var t = document.getElementById('sw-update-toast');
    if (t) t.hidden = true;
    registered.then(function (reg) { dismissed = reg.waiting; }, function () {});
  });

  // --- install button -------------------------------------------------------
  var deferred = null;
  // installed() is the ONE "are we already the app" predicate. Every display
  // mode but 'browser' is an installed window: the manifest asks for
  // standalone, a browser that cannot do that falls back along the spec's
  // chain (standalone -> minimal-ui -> browser), and a desktop app window can
  // switch to window-controls-overlay at runtime. Asking only for standalone —
  // what this used to do — offered "Install app" inside the other three.
  // navigator.standalone is iOS Safari's own flag for a home-screen launch.
  var INSTALLED_MODES = ['standalone', 'fullscreen', 'minimal-ui', 'window-controls-overlay'];
  function installed() {
    if (navigator.standalone === true) return true;
    // Inside the Android app's WebView the page IS the installed app: never
    // offer Install there (docs/design/android.md, server change 24).
    if (window.` + AppBridgeName + `) return true;
    if (!window.matchMedia) return false;
    for (var i = 0; i < INSTALLED_MODES.length; i++) {
      if (window.matchMedia('(display-mode: ' + INSTALLED_MODES[i] + ')').matches) return true;
    }
    return false;
  }
  // 🔴 THE PREDICATE IS APPLIED HERE, ON EVERY SHOW, NOT ONLY WHEN THE EVENT
  // ARRIVES. A window's display mode can change after beforeinstallprompt (the
  // install itself moves a desktop tab into an app window), and every caller
  // that turns the button on goes through this one function.
  function showInstall(on) {
    on = on && !installed();
    var bs = document.querySelectorAll('[data-install-app]');
    for (var i = 0; i < bs.length; i++) bs[i].hidden = !on;
  }
  window.addEventListener('beforeinstallprompt', function (e) {
    e.preventDefault();
    if (installed() || !window.isSecureContext) return;
    deferred = e;
    showInstall(true);
  });
  window.addEventListener('appinstalled', function () { deferred = null; showInstall(false); });
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('[data-install-app]');
    if (!b || !deferred) return;
    var ev = deferred;
    deferred = null;
    showInstall(false);
    ev.prompt();
    if (ev.userChoice) ev.userChoice.catch(function () {});
  });
  document.addEventListener('htmx:load', function () { showInstall(!!deferred); });

  // --- app badge (desktop / ChromeOS) ---------------------------------------
  // Where setAppBadge exists, show the ready-for-review count. Android has no
  // badging API (its icon dot is the unread-notification count) and muster does
  // not clear that dot: that needs muster-native Web Push, a separate arc.
  if ('setAppBadge' in navigator) {
    var timer = null;
    var refresh = function () {
      fetch('` + reviewCountPath + `', { credentials: 'include', headers: { Accept: 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) {
          if (!d || typeof d.count !== 'number') return;
          if (d.count > 0) navigator.setAppBadge(d.count).catch(function () {});
          else navigator.clearAppBadge().catch(function () {});
        })
        .catch(function () {});
    };
    var soon = function () { clearTimeout(timer); timer = setTimeout(refresh, 2000); };
    refresh();
    document.addEventListener('visibilitychange', function () { if (document.visibilityState === 'visible') soon(); });
    // task.changed carries only an id; the count is re-read rather than pushed
    // so no write path pays for a count it does not need.
    document.addEventListener('sse:task.changed', soon);
  }
})();
`)

// ---------------------------------------------------------------------------
// THE COMPOSER, OPENED BY A URL.
// ---------------------------------------------------------------------------

// composeMaxBytes bounds a pre-filled task body. A share can carry a whole
// page of text; the composer is for a sentence and a link.
const composeMaxBytes = 8 << 10

// ClampComposeBody cuts s to composeMaxBytes without splitting a UTF-8
// sequence.
func ClampComposeBody(s string) string {
	if len(s) <= composeMaxBytes {
		return s
	}
	cut := composeMaxBytes
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ComposeURL is the new-task sheet's body URL with an optional pre-filled
// body. The value is query-escaped here and attribute-escaped by gomponents.
func ComposeURL(body string) string {
	if body == "" {
		return "/ui/tasks/new"
	}
	return "/ui/tasks/new?" + url.Values{"body": {ClampComposeBody(body)}}.Encode()
}

// ComposePage is the tasks shell with the new-task sheet opening itself on
// load, pre-filled with body. It is what GET /share and the "New task"
// shortcut (/tasks?new=1) serve.
//
// 🔴 IT CREATES NOTHING. Opening the page only fills the form; the task is
// written by the operator pressing Save, through the SAME POST /tasks every
// other create uses — one write path, one session check, one broadcast. A GET
// that wrote would be a create-by-link.
func ComposePage(feat Features, body string) g.Node {
	return Doctype(shell("tasks", feat, composeOnLoad(body), ShellData{}))
}

// RenderComposePage writes ComposePage.
func RenderComposePage(w io.Writer, feat Features, body string) error {
	return ComposePage(feat, body).Render(w)
}

// composeOnLoad opens the new-task sheet once the page's scripts are ready,
// fetches its body from ComposeURL, and rewrites the address bar to /tasks so a
// reload (or the back button) does not reopen it.
func composeOnLoad(body string) g.Node {
	return g.Group{
		Div(ID("compose-on-load"), g.Attr("hidden", ""), g.Attr("data-compose-url", ComposeURL(body))),
		staticScript(composeOnLoadSrc),
	}
}

// composeOnLoadSrc is the path composeOnLoad's code is served at, /static/js/compose-on-load.js.
var composeOnLoadSrc = staticJS("compose-on-load", `
(function () {
  var tries = 0;
  function go() {
    var el = document.getElementById('compose-on-load');
    if (!el) return;
    if (!window.htmx || !window.cgModalShow) { if (++tries < 100) setTimeout(go, 50); return; }
    var u = el.getAttribute('data-compose-url');
    el.remove();
    // 🔴 THE SHEET OPENS WHEN ITS BODY HAS LANDED, NOT BEFORE. Opened first, an
    // empty sheet appeared and then grew to the form's height — and a centred
    // sheet growing moves its top: a layout shift of 0.045 at 1280×800, with no
    // tap to excuse it (e2e/tests/cls.spec.ts). Opened after, it appears whole.
    // A failed load still opens it, so the failure is seen rather than nothing.
    // cgModalShow, NOT cgModalOpen: Open empties the body, which here is the
    // composer that was just loaded into it.
    var open = function () { window.cgModalShow('task-modal'); };
    var req = window.htmx.ajax('GET', u, { target: '#task-modal-body', swap: 'innerHTML' });
    if (req && typeof req.then === 'function') req.then(open, open); else open();
    try { history.replaceState(history.state, '', '/tasks'); } catch (e) {}
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', go); else go();
})();
`)
