package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// BACK AND FORWARD: what a page does when the browser shows it again from a
// cache instead of loading it (operator ask: "cache things better so that I
// don't have to wait for full reloads every time I hit the back button").
//
// The agent page, the mirrored Rendered view and both terminals are cacheable
// (internal/api viewDocument), so Back can RESTORE them from the back/forward
// cache: the same document, its script still running, shown in a frame. That
// is fast, and it is also a page that was asleep. Everything it holds is as old
// as the moment it was hidden, its sockets are gone, and the build it was
// rendered by may no longer be the one serving. restoreJS is the page's answer,
// and it runs on EVERY signed-in document (it is part of appScript):
//
//   - on pagehide, the time is noted (in memory: the document keeps it while
//     it sits in the cache), and each participant closes its own socket. The
//     htmx event streams are NOT closed here: measured (Chromium 153), a page
//     left with them open was restored as often as one that closed them (65
//     of 66 restores each way), and the restore below closes each before it
//     reopens it;
//   - on a restore (pageshow, persisted), a page hidden for longer than
//     RestoreMaxAwayMs reloads; otherwise it asks /health which build is
//     serving and reloads when that is not the page's own; it reopens every
//     stream, and each participant catches up (the chat log's conditional
//     refetch, a mirrored view's socket resuming from its offset, a terminal's
//     re-attach);
//   - a view page (one that draws an Updating… chip) re-requests ITSELF, as a
//     reload would have: the server records the visit and marks the session
//     read, the switcher's Pins and Recent are redrawn from the answer, and an
//     answer that is a refusal — 401, or a redirect to the sign-in — reloads
//     the page, so a page restored after its session ended shows the sign-in
//     (as does one whose agent or window is gone, or that another user's
//     sign-in now answers);
//   - "Updating…" shows in the page's reserved chip only if all of that takes
//     longer than RestoreChipDelayMs, so a fast catch-up shows nothing.
//
// 🔴 A HISTORY LOAD FROM THE HTTP CACHE GETS THE SAME CHECKS. A cacheable
// document is also kept in the HTTP cache, and Chromium takes it from there for
// a Back that is not a restore WITHOUT asking the server (measured: navigation
// type back_forward, transferSize 0, no request). That document's scripts run
// afresh, so its streams open by themselves; what it lacks is the build check,
// the refusal check and a fresh send nonce, which it gets here — on every
// back_forward load, whether or not it came from the cache.
//
// 🔴 WHAT SUCH A COPY CANNOT HAVE IS ITS COMPOSER'S STATE. Chromium serves a
// Back from the HTTP cache whenever the back/forward cache does not: a browser
// with none, a page that was not eligible, or an entry evicted (600 s, or past
// the cache's size; measured: CDP CacheLimit, then navigation type
// back_forward, transferSize 0). A message that may have landed has no "Send
// again" offer there. That protection is the SERVER's
// (internal/api sendNonces.hold: the same text is refused under any nonce
// until the person says Send again), not this script's.
//
// ⚠ ONLY THE TOP WINDOW CHECKS. The view swap draws the other view in a frame
// (view_swap.go); the frame is restored with the page and reconnects its own
// streams, but the build, the age and the refusal are decided once, by the
// page, whose reload takes the frame with it.

// RestoreMaxAwayMs is how long a page may sit in the back/forward cache and
// still be shown as it was: past it, a restore reloads.
//
// ⚠ IN CHROMIUM THIS RULE ALMOST NEVER FIRES; IT IS KEPT FOR THE OTHERS.
// Chromium drops a cached page at 600 s by itself
// (kDefaultTimeToLiveInBackForwardCacheInSeconds; measured in Chromium 153: a
// static page 590 s away was restored, 610 s away it was not, and Back loaded
// it from the HTTP cache), so a Chromium restore is already under this bound.
// WebKit keeps an entry for 30 min (Settings.yaml
// BackForwardCacheExpirationInterval) and Firefox for 30 min
// (browser.sessionhistory.contentViewerTimeout): there a page away for 10 to
// 30 minutes IS restored, and this is what reloads it. That covers Safari and
// every browser on iOS. Not measured here: the WebKit and Firefox builds
// Playwright drives have their back/forward caches turned off.
const RestoreMaxAwayMs = 10 * 60 * 1000

// RestoreChipDelayMs is how long a restore catches up before its Updating…
// chip shows.
const RestoreChipDelayMs = 1000

// RestoreParticipantsVar is the window property a page's own script pushes its
// participant onto: {hide(event), show(reason) → optional Promise (caught up),
// wake(reason, staleMs) — reconnect if its socket is closed, or open but
// silent for over staleMs since its last heartbeat (live.go), alive() → false
// once the page's script has torn itself down}. A list rather than a call, so
// a script that runs before appScript joins all the same.
const RestoreParticipantsVar = "__musterRestoreParts"

// RestoreRefreshAttr marks a region a restore redraws from the page's own
// fresh copy: the element of the fresh document with the same value replaces
// this one's children. Every such region keeps its size (the switcher's rows
// are a fixed h-11), so the redraw moves nothing.
const RestoreRefreshAttr = "data-restore-refresh"

// UpdatingChipAttr marks a page's Updating… chip (updatingChip). A page with
// one is a VIEW page, and a restore re-requests it.
const UpdatingChipAttr = "data-updating-chip"

// UserMarkerName is the <meta> every VIEW page carries naming its signed-in
// user (UserMarker).
const UserMarkerName = "muster-user"

// UserMarker names the page's signed-in user in its head, so a restored page
// that re-requests itself can tell an answer drawn for ANOTHER user (one
// device, another person signed in since) and reload rather than redraw any of
// it — the check that holds where Clear-Site-Data is not honoured (a plain-HTTP
// origin). Every view page draws it: the agent page, the Rendered view and
// both terminals. Nothing when there is no user.
func UserMarker(f Features) g.Node {
	if f.SnapUser == 0 {
		return nil
	}
	return Meta(Name(UserMarkerName), Content(strconv.FormatInt(f.SnapUser, 10)))
}

// restoreJoinJS is the line a page's script uses to join: p is its participant.
func restoreJoinJS(p string) string {
	return "(window." + RestoreParticipantsVar + " = window." + RestoreParticipantsVar + " || []).push(" + p + ");"
}

// updatingChip is a view's "Updating…": shown only while a page that was shown
// from a cache (a restore) or painted from a snapshot (the Rendered view's)
// has been catching up for over a second.
//
// 🔴 IT TAKES NO LAYOUT AT ALL, AND ITS PLACE IS ALWAYS LAID OUT. It is
// absolutely positioned (pos: its placement inside a `relative` box), so it
// adds no width or height to the row it sits in, and it is `invisible`
// (visibility: hidden), never `hidden` (display: none), so showing it moves
// nothing and its box is where it shows.
func updatingChip(id, pos string) g.Node {
	return Span(ID(id), g.Attr(UpdatingChipAttr, ""), g.Attr("aria-hidden", "true"),
		Class("invisible absolute "+pos+" pointer-events-none z-10 whitespace-nowrap rounded-full bg-s2 px-2 py-0.5 text-[11px] text-fg2 ring-1 ring-inset ring-line"),
		g.Text("Updating…"))
}

// restoreJS is the restore handler (see the top of this file), as a part of
// appScript's IIFE. It binds once per window: appScript re-runs on a boosted
// swap, and the document it serves is still the same one. BUILD, the build the
// page was rendered by, is the tag's appBuildAttr, read through
// appNavJS's APP_TAG.
var restoreJS = `
  // --- Back/forward restore (restore.go) ------------------------------------
  if (!window.__musterRestore) {
    window.__musterRestore = (function () {
      var BUILD = APP_TAG.getAttribute('` + appBuildAttr + `') || '';
      var MAX_AWAY = ` + strconv.Itoa(RestoreMaxAwayMs) + `;
      var CHIP_DELAY = ` + strconv.Itoa(RestoreChipDelayMs) + `;
      var TOP = window.top === window;
      var hiddenAt = 0;
      // The live heartbeat (live.go): how often every live stream's server
      // speaks, and the silences that make a stream stale — at a wake, and at
      // the watchdog's tick.
      var HB = parseInt(APP_TAG.getAttribute('` + LiveHeartbeatAttr + `'), 10) || ` + strconv.FormatInt(DefaultLiveHeartbeat.Milliseconds(), 10) + `;
      var MARGIN = Math.min(Math.round(HB / 3), 5000);
      var STALE = 2 * HB + MARGIN, WAKE_STALE = HB + MARGIN;
      // --- The htmx event streams (/events) ------------------------------
      // Each is found where the sse extension keeps it: on its sse-connect
      // element's htmx data. lastAt is when each last spoke; one that has
      // spoken a heartbeat (muster.ping) can be judged stale.
      var esSeen = typeof WeakMap === 'function' ? new WeakMap() : null;
      function esWatch(src) {
        if (!esSeen || !src || esSeen.has(src)) return;
        var st = { lastAt: Date.now(), beat: false };
        esSeen.set(src, st);
        var hear = function () { st.lastAt = Date.now(); };
        try {
          src.addEventListener('open', hear);
          src.addEventListener('muster.hello', hear);
          src.addEventListener('` + LivePingEvent + `', function () { st.beat = true; hear(); });
        } catch (e) {}
      }
      function sseElts() {
        var out = [];
        document.querySelectorAll('[sse-connect],[data-sse-connect]').forEach(function (el) {
          var d = el['htmx-internal-data'];
          if (d && d.sseEventSource) out.push({ elt: el, src: d.sseEventSource });
        });
        return out;
      }
      // restartStream replaces an element's stream NOW, by the extension's
      // own path — the element processed again, and every element under it
      // that listens to the stream re-registered on the new one (what the
      // extension's own retry does on its reopen) — rather than through its
      // onerror, whose retry waits up to a minute after a run of failures.
      // Its open fires htmx:sseOpen, which each listener catches up on (the
      // board's resync, the chat log's conditional refetch).
      //
      // 🔴 ONLY THIS STREAM'S OWN LISTENERS. An element under a NESTED
      // sse-connect (the chief panel's chat pane inside the shell's wrapper)
      // listens to that nested stream, which is not being restarted:
      // re-registering it added a second listener on the nested source, and
      // every event there then fired twice (audit #299). So a descendant is
      // re-registered only when its nearest sse-connect is this element.
      //
      // The source the restart makes is kept as the element's (keep): the
      // extension's own retry timer, still pending from before, would
      // otherwise replace it with yet another when it fires (wrapES's sweep
      // hands the element back the kept one and closes the late one).
      var SSE_CONNECT = '[sse-connect],[data-sse-connect]';
      function restartStream(s) {
        if (!window.htmx || !document.body.contains(s.elt)) return;
        try { s.src.close(); } catch (e) {}
        try {
          htmx.trigger(s.elt, 'htmx:afterProcessNode');
          s.elt.querySelectorAll('[sse-swap],[data-sse-swap],[hx-trigger],[data-hx-trigger]').forEach(function (c) {
            if (c.closest(SSE_CONNECT) === s.elt) htmx.trigger(c, 'htmx:afterProcessNode');
          });
          var d = s.elt['htmx-internal-data'];
          if (d && d.sseEventSource && d.sseEventSource !== s.src) s.elt.__musterKeep = d.sseEventSource;
        } catch (e) {}
      }
      // 🔴 ONE STREAM PER ELEMENT. The extension's own retry is a timer that
      // creates a source whenever it fires, over whatever the element holds
      // by then — after a restart above, a second stream. So every source
      // made is noted, and once the extension has handed it to its element,
      // any source no element holds any more is closed.
      function wrapES() {
        if (!window.htmx || typeof htmx.createEventSource !== 'function' || htmx.createEventSource.__muster) return;
        var make = htmx.createEventSource, made = [];
        var sweep = function () {
          // A late source over a restart's (restartStream's keep) that is
          // still alive: the element gets the restart's back, and the late
          // one is closed before it opens — so no second open, and no second
          // catch-up.
          sseElts().forEach(function (s) {
            var k = s.elt.__musterKeep;
            if (!k) return;
            if (k.readyState === 2) { s.elt.__musterKeep = null; return; }
            if (k !== s.src) { s.elt['htmx-internal-data'].sseEventSource = k; try { s.src.close(); } catch (e) {} }
          });
          var held = sseElts().map(function (s) { return s.src; });
          made = made.filter(function (src) {
            if (src.readyState === 2) return false;
            if (held.indexOf(src) < 0) { try { src.close(); } catch (e) {} return false; }
            return true;
          });
        };
        var f = function (url) {
          var src = make(url);
          made.push(src);
          esWatch(src);
          setTimeout(sweep, 0);
          return src;
        };
        f.__muster = true;
        htmx.createEventSource = f;
      }
      wrapES();
      // The extension is deferred: it is defined by the time the document is
      // parsed, and this listener runs before htmx's own, which processes
      // the page (it was added while the document was still parsing).
      document.addEventListener('DOMContentLoaded', wrapES);
      // wakeStreams: a closed stream is replaced at any wake; an open one
      // that has gone silent past its bound, at any wake or tick.
      function wakeStreams(reason, limit) {
        sseElts().forEach(function (s) {
          esWatch(s.src);
          var st = esSeen && esSeen.get(s.src);
          if (s.src.readyState === 2) { if (reason !== 'tick') restartStream(s); return; }
          if (s.src.readyState === 1 && st && st.beat && Date.now() - st.lastAt > limit) restartStream(s);
        });
      }
      function parts() {
        return (window.` + RestoreParticipantsVar + ` || []).filter(function (p) {
          try { return !p.alive || p.alive(); } catch (e) { return false; }
        });
      }
      // renewNonces gives every message box on the page a NEW send nonce.
      //
      // 🔴 A PAGE SHOWN AGAIN FROM A CACHE CARRIES THE NONCE OF A MESSAGE THAT
      // MAY HAVE GONE ALREADY. The server delivers a (nonce, text) pair once
      // and answers a repeat "sent" without delivering it (composer.go), so
      // that a browser re-posting a form is not a second message. A Back
      // answered from the HTTP cache shows the document as first served — its
      // ORIGINAL nonce, which the message sent from it used — so the same text
      // typed and sent again ("yes") was swallowed: the box cleared and
      // nothing reached the session (audit #270 round 1, measured). A
      // restored page may hold a used one too. A send from a page shown again
      // is a new message, so it gets a new nonce, as the composers' own
      // renew does after a send; with no way to mint one the field is emptied
      // (no nonce is no protection; a reused one swallows the message).
      //
      // 🔴 RENEWING IS SAFE FOR A MESSAGE THAT MAY HAVE LANDED TOO. A
      // composer offering "Send again" (mirror_compose.go) used to keep its
      // nonce out of this renewal, because renewed, a plain Send delivered
      // the same message a second time (audit #270 round 2). The server now
      // refuses that send by its TEXT under any nonce (internal/api
      // sendNonces.hold) until the person presses Send again, so every nonce
      // is renewed.
      function renewNonces() {
        document.querySelectorAll('input[name="nonce"]').forEach(function (n) {
          var v = '';
          try {
            var b = new Uint8Array(16);
            crypto.getRandomValues(b);
            for (var i = 0; i < b.length; i++) v += (b[i] < 16 ? '0' : '') + b[i].toString(16);
          } catch (e) { v = ''; }
          n.value = v;
        });
      }
      function chips(on) {
        document.querySelectorAll('[` + UpdatingChipAttr + `]').forEach(function (c) {
          c.classList.toggle('invisible', !on);
          c.setAttribute('aria-hidden', on ? 'false' : 'true');
        });
      }
      // reload: why is for a reader of this code; the page simply loads again.
      function reload(why) { void why; location.reload(); }
      // within: p, or nothing after ms — a catch-up that never answers
      // must not hold the chip up for ever.
      function within(p, ms) {
        return Promise.race([Promise.resolve(p).catch(function () {}), new Promise(function (r) { setTimeout(r, ms); })]);
      }
      // build: /health names the build serving; another than the page's own
      // is a deploy since the page was rendered.
      function build() {
        return fetch('/health', { cache: 'no-store', credentials: 'same-origin' })
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (j) { if (j && j.version && j.version !== BUILD) reload('build'); })
          .catch(function () {});
      }
      // self: the page asks for itself again, as a reload would have. A
      // refusal reloads (to the sign-in); otherwise each refreshable region
      // is redrawn from the fresh copy.
      function self() {
        return fetch(location.href, { cache: 'no-store', credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } })
          .then(function (r) {
            var path = '';
            try { path = new URL(r.url).pathname; } catch (e) {}
            if (r.status === 401 || r.status === 403 || path === '/login' || path.indexOf('/login/') === 0) { reload('refused'); return; }
            // Gone (an agent deleted, a window closed) or now somewhere else
            // (a redirect): what the page shows is not what is there. A
            // reload shows what is. Any other failure (a 5xx mid-deploy, the
            // network) keeps the page; its streams retry on their own.
            if (r.status === 404 || r.status === 410 || r.redirected) { reload('changed'); return; }
            if (!r.ok) return;
            return r.text().then(function (html) {
              var doc = new DOMParser().parseFromString(html, 'text/html');
              // Another person's page now (one device, a sign-in in between
              // that this browser did not purge for): never redraw theirs
              // into this one — load it.
              // A page drawn for nobody (no marker) is not this user's either.
              var mine = document.querySelector('meta[name="` + UserMarkerName + `"]'), now = doc.querySelector('meta[name="` + UserMarkerName + `"]');
              if (mine && (!now || mine.getAttribute('content') !== now.getAttribute('content'))) { reload('user'); return; }
              document.querySelectorAll('[` + RestoreRefreshAttr + `]').forEach(function (el) {
                var k = el.getAttribute('` + RestoreRefreshAttr + `');
                var fresh = doc.querySelector('[` + RestoreRefreshAttr + `="' + (window.CSS && CSS.escape ? CSS.escape(k) : k) + '"]');
                if (fresh && fresh.innerHTML !== el.innerHTML) el.innerHTML = fresh.innerHTML;
              });
            });
          })
          .catch(function () {});
      }
      // reopen: every htmx event stream, by the extension's own reconnect
      // (its onerror on a closed source opens a new one and re-registers the
      // element's listeners), so the stream is the extension's as before. Its
      // reopen fires htmx:sseOpen, which is what the chat log's conditional
      // refetch catches up on (chatLogTrigger).
      function reopen() { sseElts().forEach(restartStream); }
      function catchUp(how) {
        var done = false;
        var t = setTimeout(function () { if (!done) chips(true); }, CHIP_DELAY);
        var waits = [];
        if (TOP) waits.push(build());
        if (TOP && document.querySelector('[` + UpdatingChipAttr + `]')) waits.push(self());
        if (how === 'restore') {
          reopen();
          parts().forEach(function (p) { try { if (p.show) waits.push(p.show('pageshow')); } catch (e) {} });
        }
        return Promise.all(waits.map(function (w) { return within(w, 15000); })).then(function () {
          done = true;
          clearTimeout(t);
          chips(false);
        });
      }
      window.addEventListener('pagehide', function (e) {
        hiddenAt = Date.now();
        parts().forEach(function (p) { try { if (p.hide) p.hide(e); } catch (x) {} });
      });
      window.addEventListener('pageshow', function (e) {
        if (!e.persisted) return;
        renewNonces();
        if (TOP && hiddenAt && Date.now() - hiddenAt > MAX_AWAY) { reload('away'); return; }
        catchUp('restore');
      });
      // --- Wake: reconnect what the page was away from (live.go) ------------
      // The triggers of one moment (a phone unlocked fires visibilitychange
      // and focus; a reconnect, online too) are one wake, by the strongest
      // reason. A hidden page does nothing: its showing is the next wake.
      var WAKE_RANK = { tick: 0, focus: 1, visibility: 2, online: 3 };
      var wakeReason = '', wakeTimer = 0;
      function wake(reason) {
        if (!wakeReason || WAKE_RANK[reason] > WAKE_RANK[wakeReason]) wakeReason = reason;
        if (wakeTimer) return;
        wakeTimer = setTimeout(function () {
          var r = wakeReason;
          wakeReason = ''; wakeTimer = 0;
          if (document.hidden) return;
          var limit = r === 'tick' ? STALE : WAKE_STALE;
          wakeStreams(r, limit);
          parts().forEach(function (p) { try { if (p.wake) p.wake(r, limit); } catch (e) {} });
        }, 0);
      }
      document.addEventListener('visibilitychange', function () { if (document.visibilityState === 'visible') wake('visibility'); });
      window.addEventListener('online', function () { wake('online'); });
      window.addEventListener('focus', function () { wake('focus'); });
      setInterval(function () { if (!document.hidden) wake('tick'); }, ` + strconv.Itoa(LiveTickMs) + `);
      // A Back that was not a restore, answered by the HTTP cache: the
      // document is as old as its first load. Checked once it is parsed.
      try {
        var nav = performance.getEntriesByType('navigation')[0];
        var whenParsed = function (fn) { if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', fn); else fn(); };
        // Any history load, from the cache or not (a fresh nonce costs nothing).
        if (nav && nav.type === 'back_forward') whenParsed(renewNonces);
        // 🟢 THE TRIGGER IS THE NAVIGATION'S TYPE, NOT transferSize. A browser
        // that does not report transferSize (or reports 0 for every load)
        // would otherwise skip the checks silently; one that reports a network
        // load costs a /health and a re-request of a document just fetched.
        if (TOP && nav && nav.type === 'back_forward') {
          whenParsed(function () { catchUp('http-cache'); });
        }
      } catch (e) {}
      // The bounds, for a participant deciding at its own moment (a chat's
      // send) whether its socket can still be trusted.
      return { staleMs: STALE, wakeStaleMs: WAKE_STALE };
    })();
  }
`
