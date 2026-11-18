package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// FaroSettings holds the frontend RUM (Grafana Faro) configuration surfaced into
// the page head. It is set ONCE at startup from the environment (see main.go) and
// read by faroHead on every full-document render.
//
// The API key is CLIENT-PUBLIC by design (it is shipped to every browser and only
// gates the public Faro collect endpoint), so it is a plain value, not a secret.
type FaroSettings struct {
	// URL is the Faro collect endpoint (MUSTER_FARO_URL). EMPTY DISABLES all
	// telemetry — no script tags, no init are rendered (the e2e/local default).
	URL string
	// Key is the public Faro app API key (MUSTER_FARO_API_KEY); the SDK sends it
	// as the x-api-key header.
	Key string
	// Version is the running build version, used as the Faro app version.
	Version string
	// Tracing enables the faro-web-tracing bundle + OTEL tracing instrumentation
	// (frontend → Tempo spans). Off by default to keep the phone payload lean.
	// On, the page's same-origin fetches and XHRs carry a traceparent (their
	// server spans join the browser's trace), and each session open is a span
	// of its own (musterOpen) parented on the document's server span.
	Tracing bool
	// Relay: URL is muster's own relay route (api.FaroRelayPath), posted to
	// same-origin; the page makes it absolute from location.origin, because
	// Faro's tracing ignores its own collector by exact URL and a relative
	// one would trace every telemetry post.
	Relay bool
}

// faroConfig is the process-wide Faro config. The zero value (empty URL) renders
// no telemetry, which is exactly the desired local/e2e behavior.
var faroConfig FaroSettings

// SetFaroConfig installs the Faro RUM config read by every page head. Called once
// from main at startup.
func SetFaroConfig(c FaroSettings) { faroConfig = c }

// faroHead returns the Faro initializer's tag for the page head. When the
// collect URL is empty (local/e2e) it returns NOTHING — zero telemetry surface,
// so no SDK loads and no events are sent.
//
// 🔴 THE ~170 KB OF VENDORED SDK IS NOT IN THE HEAD. It used to be: two
// parser-blocking <script src> tags (the SDK and the tracing bundle) on every
// document, read and parsed before the body. Measured (e2e/tests/open-perf
// .spec.ts, the throttled profile, a mirrored view): DOMContentLoaded 150 ms
// later with them than without. Now the head carries only faro-init.js, which
// defines window.musterOpen and window.cgTrack at once — recording into memory
// — and loads the two bundles itself once the page's open is over
// (musterOpen.quiet), in idle time; Faro then reports what was recorded, at
// the times it was recorded. The bundles' URLs ride on the init's tag.
func faroHead() g.Node {
	if faroConfig.URL == "" {
		return g.Text("")
	}
	tracing, relay, tracingSrc := "0", "0", ""
	if faroConfig.Tracing {
		tracing, tracingSrc = "1", web.Asset("/static/vendor/faro-web-tracing.iife.js")
	}
	if faroConfig.Relay {
		relay = "1"
	}
	return staticScript(faroInitSrc, Data("faro-url", faroConfig.URL), Data("faro-key", faroConfig.Key),
		Data("faro-version", faroConfig.Version), Data("faro-tracing", tracing), Data("faro-relay", relay),
		Data("faro-sdk", web.Asset("/static/vendor/faro-web-sdk.iife.js")), Data("faro-tracing-src", tracingSrc))
}

// faroInitSrc is the initializer, a STATIC script (static_script.go): its
// bytes are the same for every page and every deployment, and what differs —
// the collect URL, the key, the version, tracing, relay and the two vendored
// bundles' URLs — rides on its tag as data-faro-* attributes (faroHead), read
// through document.currentScript.
//
// It runs in the head and at once defines window.cgTrack (custom events) and
// window.musterOpen (musterOpenJS), both of which record into memory until Faro
// is up; it also keeps the page's uncaught errors meanwhile. Then, once the
// page has loaded and no open it began is still running (at most 10 s), in
// idle time, it loads the SDK and the tracing bundle (in that order) and
// initializes Faro with the standard web instrumentations minus the console and
// CSP reports, the tracing instrumentation when on, and hands Faro what was
// recorded. The web-vitals and performance instrumentations read the browser's
// buffered entries, so loading late loses none of them.
//
// The web-vitals instrumentation is among them, so real users' CLS is sent to
// the collector as a `web-vitals` measurement, with the vendored SDK's (1.19)
// default attribution — the element of the largest shift. Nothing in this
// repository reads it back. The browser suite's layout-shift gate,
// e2e/tests/cls.spec.ts, is a separate and stricter number: the sum of every
// shift, not web-vitals' worst session window.
var faroInitSrc = staticJS("faro-init", faroInitJS)

const faroInitJS = `
(function () {
  // Once per window: a head script, but a boosted swap must never initialise
  // Faro twice.
  if (window.__musterFaroInit) return;
  window.__musterFaroInit = true;
  var me = document.currentScript, d = (me && me.dataset) || {};
  var C = { url: d.faroUrl || '', key: d.faroKey || '', version: d.faroVersion || '', tracing: d.faroTracing === '1', relay: d.faroRelay === '1',
    sdk: d.faroSdk || '', tracingSrc: d.faroTracingSrc || '' };
  // cgTrack is always defined so call sites never have to guard. Until Faro is
  // up it keeps (a bounded number of) events, sent once it is.
  var early = [], errs = [];
  window.cgTrack = function (name, attrs) { if (early.length < 50) early.push([name, attrs]); };
  // Uncaught errors before Faro is up, kept as the Error objects themselves:
  // Faro's beforeSend scrubs them exactly as it scrubs a live one.
  function onErr(e) { if (errs.length < 20 && e && e.error instanceof Error) errs.push(e.error); }
  function onRej(e) { if (errs.length < 20 && e && e.reason instanceof Error) errs.push(e.reason); }
  window.addEventListener('error', onErr);
  window.addEventListener('unhandledrejection', onRej);
  ` + musterOpenJS + `
  window.musterOpen = musterOpenAPI();
  if (!C.url || !C.sdk) return;

  function initFaro() {
    window.removeEventListener('error', onErr);
    window.removeEventListener('unhandledrejection', onRej);
    try {
      var SDK = window.GrafanaFaroWebSdk;
      if (!SDK || !SDK.initializeFaro) return;
      // 🔴 NO QUERY STRING LEAVES THE PAGE. A muster URL's query names a
      // window, carries a search the person typed, or a resume offset; what the
      // telemetry needs is the path. Every URL Faro would send — the page's,
      // a resource's, a fetch span's — is cut at its '?'.
      var noQuery = function (u) { return typeof u === 'string' ? u.split('?')[0].split('#')[0] : u; };
      var scrub = function (o) {
        if (!o || typeof o !== 'object') return;
        for (var k in o) {
          if (Object.prototype.hasOwnProperty.call(o, k) && typeof o[k] === 'string' && /^(https?:|wss?:|\/)/.test(o[k])) o[k] = noQuery(o[k]);
        }
      };
      var spanURL = function (span) {
        try { if (span && span.attributes) { ['http.url', 'url.full'].forEach(function (k) { if (span.attributes[k]) span.setAttribute(k, noQuery(span.attributes[k])); }); } } catch (e) {}
      };
      var url = C.relay ? location.origin + C.url : C.url;
      // 🔴 NO CONSOLE CAPTURE AND NO CSP-VIOLATION REPORTS: a console line can
      // print anything the page holds (pane text, a prompt, a title), and a CSP
      // report carries a sample of the violating text. Neither is collected.
      var instrumentations = SDK.getWebInstrumentations({ captureConsole: false, enableContentSecurityPolicyInstrumentation: false });
      if (C.tracing && window.GrafanaFaroWebTracing && window.GrafanaFaroWebTracing.TracingInstrumentation) {
        instrumentations = instrumentations.concat([new window.GrafanaFaroWebTracing.TracingInstrumentation({
          // Same-origin fetches and XHRs carry a traceparent by default (the
          // OpenTelemetry web instrumentations' rule): no other origin is named.
          instrumentationOptions: {
            fetchInstrumentationOptions: { applyCustomAttributesOnSpan: spanURL },
            xhrInstrumentationOptions: { applyCustomAttributesOnSpan: spanURL },
          },
        })]);
      }
      var opts = {
        url: url,
        app: { name: 'muster', version: C.version || 'dev' },
        instrumentations: instrumentations,
        // 🔴 FEWER, FULLER BATCHES: Faro's default sends every 250 ms, which a
        // page's polls turned into a post every few seconds per tab. Five
        // seconds (or 50 items, whichever comes first) keeps one open view
        // well under the relay's per-tab rate.
        batching: { enabled: true, sendTimeout: 5000, itemLimit: 50 },
        // The polls are not opens: no fetch span and no resource entry for the
        // rendered view's ask poll (every 3 s), the send queue's and the asker's
        // polls, the badge count, or the relay itself.
        ignoreUrls: [/\/mirror\/view\/asks(\?|$)/, /\/ui\/send-queue/, /\/attention\/asker/, /\/ui\/tasks\/review-count/, /\/telemetry\/faro$/],
        beforeSend: function (item) {
          try {
            // A log item can only come from the console, which is not captured:
            // one that appears anyway is dropped, not scrubbed.
            if (item.type === 'log') return null;
            if (item.meta && item.meta.page) { item.meta.page.url = noQuery(item.meta.page.url); }
            // 🔴 AN EXCEPTION KEEPS ITS SHAPE, NEVER ITS TEXT: an error's
            // message can quote whatever the page held (a thrown string, a
            // value interpolated into a message). What is kept is the error's
            // type when it is a plain identifier, and each stack frame's file
            // (cut at its query), function name (when it is an identifier),
            // line and column. Its context goes.
            if (item.type === 'exception' && item.payload) {
              var ex = item.payload;
              ex.type = /^[A-Za-z_$][\w$]{0,63}$/.test(ex.type || '') ? ex.type : 'Error';
              ex.value = '[scrubbed]';
              delete ex.context;
              if (ex.stacktrace && ex.stacktrace.frames) {
                ex.stacktrace.frames = ex.stacktrace.frames.slice(0, 50).map(function (f) {
                  return { filename: noQuery(String(f.filename || '')), 'function': /^[\w$.<>]{1,128}$/.test(f['function'] || '') ? f['function'] : '?', lineno: f.lineno, colno: f.colno };
                });
              }
              return item;
            }
            if (item.payload) { scrub(item.payload); scrub(item.payload.attributes); scrub(item.payload.context); }
          } catch (e) { return null; }
          return item;
        },
      };
      if (C.key) opts.apiKey = C.key;
      var faro = SDK.initializeFaro(opts);
      window.cgTrack = function (name, attrs) {
        try {
          var a = {};
          if (attrs) { for (var k in attrs) { if (Object.prototype.hasOwnProperty.call(attrs, k)) { a[k] = String(attrs[k]); } } }
          faro.api.pushEvent(name, a);
        } catch (e) {}
      };
      early.splice(0).forEach(function (e) { window.cgTrack(e[0], e[1]); });
      errs.splice(0).forEach(function (er) { try { faro.api.pushError(er); } catch (e) {} });
      window.musterOpen.attach(faro);
      window.__musterFaroReady = true;
    } catch (e) {}
  }
  // load adds the bundles, in order (async = false keeps a dynamic script's
  // place), and initializes Faro once the last has run.
  var loading = false;
  function load() {
    if (loading) return;
    loading = true;
    var srcs = [C.sdk];
    if (C.tracing && C.tracingSrc) srcs.push(C.tracingSrc);
    srcs.forEach(function (src, i) {
      var s = document.createElement('script');
      s.src = src;
      s.async = false;
      if (i === srcs.length - 1) { s.onload = initFaro; s.onerror = initFaro; }
      document.head.appendChild(s);
    });
  }
  function idle(fn) {
    if (typeof window.requestIdleCallback === 'function') window.requestIdleCallback(fn, { timeout: 1000 }); else setTimeout(fn, 50);
  }
  function schedule() { window.musterOpen.quiet(function () { idle(load); }, 10000); }
  if (document.readyState === 'complete') schedule(); else window.addEventListener('load', schedule);
})();
`

// musterOpenJS defines musterOpenAPI(): window.musterOpen, the page's measure
// of OPENING a session (docs/telemetry.md). A view's script asks it for a
// handle when it opens (begin), marks the moments of the open on it — the
// marks each view names, the document's own milestones (markAt, from the
// navigation entry), counts (count) and closed words (attr) — and ends it with
// a closed outcome; a socket that ends reports why (disconnected).
//
// Each open is:
//   - a Faro MEASUREMENT `muster_open` — every mark's milliseconds since the
//     open began and every count, with the view, the outcome, the closed words
//     and the trace id as context;
//   - with tracing on, a SPAN `open <view>`. The page's first open starts at
//     the navigation itself (performance.timeOrigin) and is a child of the
//     document's server span, read off the navigation entry's Server-Timing
//     traceparent — so the browser's open, the server's document and (through
//     tp(), which the socket URL carries as ?tp=) the stream's server span and
//     the host's spans are ONE trace. Its attributes carry the document's
//     server phases (the Server-Timing entries).
//
// 🔴 IT WORKS BEFORE FARO IS LOADED, AND THAT IS THE COMMON CASE: faro-init
// loads the SDK only once the open is over. Until attach(faro), an open is a
// record in memory — its start, each mark's time, its end — and attach turns
// each record into its measurement and its span, with the recorded times (an
// OpenTelemetry span takes explicit start, event and end times). One thing
// cannot wait: the socket's ?tp= is asked for when the socket opens, before the
// open's span exists, so tp() is then the DOCUMENT's traceparent — the
// stream's server span is the document span's child, in the same trace, beside
// the page's open span rather than under it.
//
// A disconnect is a Faro EVENT `muster.session_disconnected` {view, reason,
// …} and, with tracing on, a span `disconnected <view>` in the open's trace.
//
// 🔴 WHAT IT SENDS IS IDS, DURATIONS, COUNTS AND CLOSED WORDS. A caller passes
// no text: never a title, a message or an error's prose (the server's `reason`
// beside its `message` is the closed word to report). attr() takes only a
// value matching ^[a-z_]{1,32}$ and count() only a finite number; anything else
// is dropped.
const musterOpenJS = `
  function musterOpenAPI() {
    var faro = null, otel = null, tracer = null;
    var docTP = '', server = {};
    try {
      var nav = performance.getEntriesByType('navigation')[0];
      ((nav && nav.serverTiming) || []).forEach(function (e) {
        if (e.name === 'traceparent') docTP = e.description; else server[e.name] = e.duration;
      });
    } catch (e) {}
    function parse(tp) {
      var m = /^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$/.exec(tp || '');
      return m ? { traceId: m[1], spanId: m[2], traceFlags: parseInt(m[3], 16), isRemote: true } : null;
    }
    function tpOf(span) {
      try { var c = span.spanContext(); return '00-' + c.traceId + '-' + c.spanId + '-0' + (c.traceFlags & 1); } catch (e) { return ''; }
    }
    function epoch(perfMs) { return performance.timeOrigin + perfMs; }
    function words(o) { var a = {}; for (var k in (o || {})) { if (Object.prototype.hasOwnProperty.call(o, k)) a[k] = String(o[k]); } return a; }
    var WORD = /^[a-z_]{1,32}$/;
    var pageOpened = false;
    // 🔴 A BOOSTED SWAP IS AN OPEN OF ITS OWN. htmx replaces the body without a
    // navigation, so the navigation entry above is the FIRST document's —
    // an open after a swap measured from it would read the minutes the board
    // was open, in the board's trace. The swap's request start and the
    // Server-Timing of ITS response (the swap's server span, and its phases)
    // are kept here, and the next begin() measures from them.
    var swap = null, swapStart = 0;
    document.addEventListener('htmx:beforeRequest', function (e) {
      if (e.detail && e.detail.boosted) swapStart = performance.now();
    });
    document.addEventListener('htmx:afterRequest', function (e) {
      try {
        if (!e.detail || !e.detail.boosted || !e.detail.xhr) return;
        var st = { tp: '', server: {}, t0: swapStart || performance.now() };
        (e.detail.xhr.getResponseHeader('Server-Timing') || '').split(',').forEach(function (part) {
          var m = /^\s*([\w-]+)\s*(?:;\s*dur=([\d.]+))?(?:\s*;\s*desc="([^"]*)")?/.exec(part);
          if (!m) return;
          if (m[1] === 'traceparent') st.tp = m[3] || ''; else if (m[2]) st.server[m[1]] = parseFloat(m[2]);
        });
        swap = st;
      } catch (err) {}
    });
    // The opens begun before Faro was up (reported at attach), the disconnects
    // meanwhile, and the quiet() callers waiting for the opens to end.
    var pending = [], discs = [], waiters = [];
    function running() { for (var i = 0; i < pending.length; i++) { if (!pending[i].ended) return true; } return false; }
    function wake() {
      if (running()) return;
      waiters.splice(0).forEach(function (w) { clearTimeout(w.t); try { w.fn(); } catch (e) {} });
    }
    function startSpan(o) {
      if (!tracer || o.span) return;
      try {
        var parent = otel.context.active();
        var p = o.reopen ? null : parse(o.openTP);
        if (p) parent = otel.trace.setSpanContext(parent, p);
        var a = { 'muster.view': o.view, 'muster.reopen': o.reopen, 'muster.swap': o.swap };
        if (!o.reopen) { for (var k in o.server) a['muster.server.' + k + '_ms'] = o.server[k]; }
        for (var k2 in o.attrs) a['muster.' + k2] = o.attrs[k2];
        for (var k3 in o.counts) a['muster.' + k3] = o.counts[k3];
        o.span = tracer.startSpan('open ' + o.view, { startTime: epoch(o.t0), attributes: a }, parent);
        o.events.forEach(function (ev) { o.span.addEvent(ev[0], epoch(ev[1])); });
      } catch (e) { o.span = null; }
    }
    // report sends an ENDED open: its measurement, and its span's end.
    function report(o) {
      o.reported = true;
      var vals = { total_ms: Math.round(o.endAt - o.t0) };
      for (var m in o.marks) vals[m + '_ms'] = Math.round(o.marks[m]);
      for (var c in o.counts) vals[c] = o.counts[c];
      var ctx = { view: o.view, outcome: o.outcome, reopen: String(o.reopen), trace_id: o.span ? o.span.spanContext().traceId : '' };
      for (var w in o.attrs) { if (typeof o.attrs[w] === 'string') ctx[w] = o.attrs[w]; }
      try { faro.api.pushMeasurement({ type: 'muster_open', values: vals }, { context: ctx }); } catch (e) {}
      if (o.span) {
        try {
          o.span.setAttribute('muster.outcome', o.outcome);
          if (o.outcome !== 'ok') o.span.setStatus({ code: 2, message: o.outcome });
          o.span.end(epoch(o.endAt));
        } catch (e) {}
      }
    }
    function disconnectedFrom(o, view, reason, extra) {
      if (!faro) { if (discs.length < 20) discs.push([o, view, reason, extra]); return; }
      var a = words(extra); a.view = view; a.reason = reason || 'unknown';
      try { faro.api.pushEvent('muster.session_disconnected', a); } catch (e) {}
      if (!tracer) return;
      try {
        var ctx = o && o.span ? otel.trace.setSpan(otel.context.active(), o.span) : otel.context.active();
        var attrs = { 'muster.view': view, 'muster.reason': a.reason };
        for (var k in extra || {}) attrs['muster.' + k] = String(extra[k]);
        var s = tracer.startSpan('disconnected ' + view, { attributes: attrs }, ctx);
        s.setStatus({ code: 2, message: a.reason });
        s.end();
      } catch (e) {}
    }
    return {
      begin: function (view, attrs) {
        // The page's first open began at the navigation, one after a boosted
        // swap at the swap's request; a reconnect, now.
        var fromSwap = swap;
        swap = null;
        var reopen = pageOpened && !fromSwap;
        pageOpened = true;
        var o = { view: view, t0: fromSwap ? fromSwap.t0 : (reopen ? performance.now() : 0), reopen: reopen, swap: !!fromSwap,
          openTP: fromSwap ? fromSwap.tp : docTP, server: fromSwap ? fromSwap.server : server,
          attrs: {}, counts: {}, marks: {}, events: [], ended: false, outcome: '', endAt: 0, span: null };
        for (var k in attrs || {}) o.attrs[k] = attrs[k];
        if (faro) startSpan(o); else if (pending.length < 20) pending.push(o);
        var h = {
          tp: function () { return o.span ? tpOf(o.span) : (o.reopen ? '' : (o.openTP || '')); },
          mark: function (name) { h.markAt(name, performance.now()); },
          // markAt marks a moment that already happened, at its own time (a
          // navigation milestone) — even after the open ended, until it is
          // reported; one before the open began is not marked.
          markAt: function (name, at) {
            if (o.reported || o.marks[name] !== undefined || !(at >= o.t0)) return;
            o.marks[name] = at - o.t0;
            o.events.push([name, at]);
            if (o.span) { try { o.span.addEvent(name, epoch(at)); } catch (e) {} }
          },
          attr: function (name, word) {
            if (o.ended || !WORD.test(String(word))) return;
            o.attrs[name] = String(word);
            if (o.span) { try { o.span.setAttribute('muster.' + name, String(word)); } catch (e) {} }
          },
          // flag is a yes/no attribute of the open's span (not a word: it is
          // not in the measurement's context).
          flag: function (name, on) {
            if (o.ended) return;
            o.attrs[name] = !!on;
            if (o.span) { try { o.span.setAttribute('muster.' + name, !!on); } catch (e) {} }
          },
          count: function (name, n) {
            if (o.ended || typeof n !== 'number' || !isFinite(n)) return;
            o.counts[name] = n;
            if (o.span) { try { o.span.setAttribute('muster.' + name, n); } catch (e) {} }
          },
          end: function (outcome) {
            if (o.ended) return;
            o.ended = true;
            o.outcome = outcome;
            o.endAt = performance.now();
            if (faro) report(o);
            wake();
          },
          disconnected: function (reason, extra) { disconnectedFrom(o, view, reason, extra); },
        };
        return h;
      },
      disconnected: function (view, reason, extra) { disconnectedFrom(null, view, reason, extra); },
      // quiet calls fn once no open begun on this page is still running, or
      // after capMs whatever is.
      quiet: function (fn, capMs) {
        var w = { fn: fn, t: 0 };
        w.t = setTimeout(function () { var i = waiters.indexOf(w); if (i >= 0) { waiters.splice(i, 1); fn(); } }, capMs);
        waiters.push(w);
        wake();
      },
      // attach hands the page's telemetry to Faro: every open recorded so far
      // becomes its span (and, when it has ended, its measurement), and every
      // disconnect its event.
      attach: function (f) {
        faro = f;
        try { otel = faro.api.getOTEL ? faro.api.getOTEL() : null; tracer = otel ? otel.trace.getTracer('muster-page') : null; } catch (e) {}
        pending.splice(0).forEach(function (o) { startSpan(o); if (o.ended) report(o); });
        discs.splice(0).forEach(function (x) { disconnectedFrom(x[0], x[1], x[2], x[3]); });
      },
    };
  }
`
