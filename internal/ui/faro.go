package ui

import (
	"encoding/json"

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

// faroHead returns the Faro <script> tags + inline init for the page head. When
// the collect URL is empty (local/e2e) it returns NOTHING — zero telemetry
// surface, so no SDK loads and no events are sent.
//
// Load order matters: the SDK bundle must execute before the inline init (so
// GrafanaFaroWebSdk is defined) and before the tracing bundle (which depends on
// the SDK global). The vendored bundles are plain IIFEs that synchronously define
// their globals, so they are NOT deferred — they must be ready when the init runs.
func faroHead() g.Node {
	if faroConfig.URL == "" {
		return g.Text("")
	}
	nodes := []g.Node{
		Script(Src(web.Asset("/static/vendor/faro-web-sdk.iife.js"))),
	}
	if faroConfig.Tracing {
		nodes = append(nodes, Script(Src(web.Asset("/static/vendor/faro-web-tracing.iife.js"))))
	}
	nodes = append(nodes, faroInitScript())
	return g.Group(nodes)
}

// faroInitScript is the inline initializer. All dynamic values are JSON-encoded so
// they are safely embedded regardless of contents. It initializes Faro with the
// standard web instrumentations (page views, web vitals, errors, navigation),
// optionally registers the tracing instrumentation, and exposes window.cgTrack —
// a guarded helper that forwards custom events to faro.api.pushEvent.
//
// The web-vitals instrumentation is among them, so real users' CLS is sent to
// the collector as a `web-vitals` measurement, with the vendored SDK's (1.19)
// default attribution — the element of the largest shift. Nothing in this
// repository reads it back. The browser suite's layout-shift gate,
// e2e/tests/cls.spec.ts, is a separate and stricter number: the sum of every
// shift, not web-vitals' worst session window.
func faroInitScript() g.Node {
	cfg := struct {
		URL     string `json:"url"`
		Key     string `json:"key"`
		Version string `json:"version"`
		Tracing bool   `json:"tracing"`
		Relay   bool   `json:"relay"`
	}{
		URL:     faroConfig.URL,
		Key:     faroConfig.Key,
		Version: faroConfig.Version,
		Tracing: faroConfig.Tracing,
		Relay:   faroConfig.Relay,
	}
	// Marshal can only fail on unsupported types; these are all strings/bool.
	b, _ := json.Marshal(cfg)
	return Script(g.Raw(`
(function () {
  // cgTrack is always defined (no-op until/unless Faro initializes) so call sites
  // never have to guard. It forwards a named custom event + attributes to Faro.
  window.cgTrack = function () {};
  try {
    var C = ` + string(b) + `;
    var SDK = window.GrafanaFaroWebSdk;
    if (!SDK || !SDK.initializeFaro || !C.url) return;
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
    window.musterOpen = musterOpenAPI(faro);
  } catch (e) {}

  ` + musterOpenJS + `
})();
`))
}

// musterOpenJS defines musterOpenAPI(faro): window.musterOpen, the page's
// measure of OPENING a session (docs/telemetry.md). A view's script asks it for
// a handle when it opens (begin), marks the moments of the open on it —
// snapshot_painted, stream_connected, first_paint, usable — and ends it with a
// closed outcome; a socket that ends reports why (disconnected).
//
// Each open is:
//   - a Faro MEASUREMENT `muster_open` — every mark's milliseconds since the
//     open began, with the view, the outcome and the trace id as context;
//   - with tracing on, a SPAN `open <view>`. The page's first open starts at
//     the navigation itself (performance.timeOrigin) and is a child of the
//     document's server span, read off the navigation entry's Server-Timing
//     traceparent — so the browser's open, the server's document and (through
//     tp(), which the socket URL carries as ?tp=) the stream's server span and
//     the host's spans are ONE trace. Its attributes carry the document's
//     server phases (the Server-Timing entries).
//
// A disconnect is a Faro EVENT `muster.session_disconnected` {view, reason,
// …} and, with tracing on, a span `disconnected <view>` in the open's trace.
//
// 🔴 WHAT IT SENDS IS IDS, DURATIONS AND CLOSED WORDS. A caller passes no
// text: never a title, a message or an error's prose (the server's `reason`
// beside its `message` is the closed word to report).
const musterOpenJS = `
  function musterOpenAPI(faro) {
    var otel = null, tracer = null;
    try { otel = faro.api.getOTEL ? faro.api.getOTEL() : null; tracer = otel ? otel.trace.getTracer('muster-page') : null; } catch (e) {}
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
    var pageOpened = false;
    function disconnectedFrom(parentSpan, view, reason, extra) {
      var a = words(extra); a.view = view; a.reason = reason || 'unknown';
      try { faro.api.pushEvent('muster.session_disconnected', a); } catch (e) {}
      if (!tracer) return;
      try {
        var ctx = parentSpan ? otel.trace.setSpan(otel.context.active(), parentSpan) : otel.context.active();
        var attrs = { 'muster.view': view, 'muster.reason': a.reason };
        for (var k in extra || {}) attrs['muster.' + k] = String(extra[k]);
        var s = tracer.startSpan('disconnected ' + view, { attributes: attrs }, ctx);
        s.setStatus({ code: 2, message: a.reason });
        s.end();
      } catch (e) {}
    }
    return {
      begin: function (view, attrs) {
        // The page's first open began at the navigation; a reconnect, now.
        var reopen = pageOpened;
        pageOpened = true;
        var t0 = reopen ? performance.now() : 0;
        var span = null;
        if (tracer) {
          try {
            var parent = otel.context.active();
            var p = reopen ? null : parse(docTP);
            if (p) parent = otel.trace.setSpanContext(parent, p);
            var a = { 'muster.view': view, 'muster.reopen': reopen };
            if (!reopen) { for (var k in server) a['muster.server.' + k + '_ms'] = server[k]; }
            for (var k2 in attrs || {}) a['muster.' + k2] = attrs[k2];
            span = tracer.startSpan('open ' + view, { startTime: epoch(t0), attributes: a }, parent);
          } catch (e) { span = null; }
        }
        var marks = {}, ended = false;
        return {
          tp: function () { return span ? tpOf(span) : ''; },
          mark: function (name) {
            if (ended || marks[name] !== undefined) return;
            var now = performance.now();
            marks[name] = now - t0;
            if (span) { try { span.addEvent(name, epoch(now)); } catch (e) {} }
          },
          end: function (outcome) {
            if (ended) return;
            ended = true;
            var now = performance.now();
            var vals = { total_ms: Math.round(now - t0) };
            for (var m in marks) vals[m + '_ms'] = Math.round(marks[m]);
            try {
              faro.api.pushMeasurement({ type: 'muster_open', values: vals },
                { context: { view: view, outcome: outcome, reopen: String(reopen), trace_id: span ? span.spanContext().traceId : '' } });
            } catch (e) {}
            if (span) {
              try {
                span.setAttribute('muster.outcome', outcome);
                if (outcome !== 'ok') span.setStatus({ code: 2, message: outcome });
                span.end(epoch(now));
              } catch (e) {}
            }
          },
          disconnected: function (reason, extra) { disconnectedFrom(span, view, reason, extra); },
        };
      },
      disconnected: function (view, reason, extra) { disconnectedFrom(null, view, reason, extra); },
    };
  }
`
