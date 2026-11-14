package api

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// BuildVersion is the running build version, set from main via -ldflags
// "-X github.com/ZacxDev/muster/internal/api.BuildVersion=<ver>". Surfaced in
// /health and the startup log so the running build is identifiable.
var BuildVersion = "dev"

// telemetryConnectSrc, when non-empty, is an extra origin appended to the CSP
// connect-src directive so the browser may POST cross-origin RUM (the Grafana
// Faro collect endpoint lives on another host). Set once at startup.
var telemetryConnectSrc string

// SetTelemetryConnectSrc registers an additional connect-src CSP origin from a
// full URL (only its scheme://host is used). Without this, connect-src 'self'
// silently blocks every Faro POST and no telemetry is ever sent. Empty/invalid
// is a no-op (telemetry off ⇒ no CSP relaxation).
func SetTelemetryConnectSrc(rawURL string) {
	if rawURL == "" {
		return
	}
	if u, err := url.Parse(rawURL); err == nil && u.Scheme != "" && u.Host != "" {
		telemetryConnectSrc = u.Scheme + "://" + u.Host
	}
}

// withMiddleware wraps the mux with the cross-cutting middleware chain, outermost
// first: the plugin-origin Host refusal (refusePluginOriginHosts), panic
// recovery (so a handler panic can never crash the single-replica process),
// security headers, the request's server span (internal/telemetry; none while
// tracing is off), then access logging.
//
// Every listener muster-server binds (the main port, MUSTER_CONNECT_PORT's
// OnlyConnectSurface and MUSTER_BROWSER_PORT's BrowserListener) wraps this one
// Handler, so the refusal covers all three (TestAPluginOriginHostIsRefusedOnEveryListener).
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.refusePluginOriginHosts(s.recoverMiddleware(securityHeaders(telemetry.Middleware(s.accessLog(next)))))
}

// PluginOriginHostRefusal is the 421 body for a request whose Host is under
// MUSTER_PLUGIN_ORIGINS.
const PluginOriginHostRefusal = "this host belongs to a plugin (MUSTER_PLUGIN_ORIGINS), not to muster\n"

// refusePluginOriginHosts answers 421 Misdirected Request, before any handler,
// for a request whose Host is under MUSTER_PLUGIN_ORIGINS.
//
// 🔴 WHY: those hosts are a plugin's (a preview gateway's zone), and muster's
// same-origin checks trust the request's own Host (wsAllowedOrigins puts r.Host
// on the list requireSameOrigin and the WebSockets accept). A request reaching
// muster with Host and Origin both a plugin host, by a misrouted edge or a
// chosen Host, would pass them as same-origin. Refusing the host outright
// closes that, whatever the route. Unset: nothing is refused.
//
// Being outermost, a refusal is outside accessLog, the span and the http
// metrics, so it counts and logs itself (muster_plugin_origin_refusals_total,
// and a line in requireSameOrigin's shape).
func (s *Server) refusePluginOriginHosts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if plugins.AnyMatchesHost(s.ext.PluginOrigins, r.Host) {
			metrics.PluginOriginRefusals.Inc()
			s.logger.Printf("refused %s %s for Host %q: it is under MUSTER_PLUGIN_ORIGINS (421)", r.Method, r.URL.Path, r.Host)
			h := w.Header()
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusMisdirectedRequest)
			_, _ = w.Write([]byte(PluginOriginHostRefusal))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware turns a panic in any handler into a logged 500 instead of a
// process crash. muster runs a single replica fronting every Claude Code
// approval; one nil-deref must not take the gateway down.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				metrics.Panics.WithLabelValues("http").Inc()
				s.logger.Printf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				// Best-effort 500; if the handler already wrote a header this is a no-op.
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// safeGo runs fn in a goroutine with panic recovery, so a panic in background
// work (dispatch, kickoff, grant apply, push) is logged rather than crashing the
// process. Use in place of a bare `go func(){...}` for any goroutine that touches
// external data (k8s objects, gateway responses, the DB).
func safeGo(logger *log.Logger, what string, fn func()) {
	go func() {
		defer recoverGoroutine(logger, what)
		fn()
	}()
}

// SuperviseBackoff is how long Supervise waits before restarting a loop that
// panicked; it doubles per consecutive panic, up to SuperviseBackoffMax. A run
// that stayed up at least SuperviseHealthy before it panicked counts as
// healthy, and the wait starts again from SuperviseBackoff. Vars so a test
// need not wait.
var (
	SuperviseBackoff    = 5 * time.Second
	SuperviseBackoffMax = 5 * time.Minute
	SuperviseHealthy    = 5 * time.Minute
)

// superviseExited, when set (tests only), is called with a supervised loop's
// name as its supervisor goroutine returns.
var superviseExited func(name string)

// Supervise runs a background loop in a goroutine and keeps it alive across a
// PANIC: the panic is logged and counted (as safeGo's are), and the loop is
// started again after SuperviseBackoff, doubling per consecutive panic and
// starting again from SuperviseBackoff after a run that stayed up
// SuperviseHealthy. A loop that RETURNS (its ctx ended, or it is switched off)
// is not restarted, and a ctx that ends during the backoff ends the
// supervisor without another run.
//
// 🔴 EVERY LOOP cmd/muster-server STARTS GOES THROUGH IT. A bare `go` has no
// recover, so one panicking pass of one sweep killed the whole process — and,
// started again, the same sweep panicked again: a crash loop (a team row met
// by a per-owner sweep, #654 round 1). Restarting the LOOP rather than
// recovering once and returning keeps the rest of that loop's work running.
// A loop with a "finished" signal for shutdown must raise it only when it
// RETURNS, never on a panic's unwind (agentkickoff.Deliverer.Done,
// officers.Reconciler.Done): a restarted run is still working.
func Supervise(ctx context.Context, logger *log.Logger, name string, run func(context.Context)) {
	safeGo(logger, "supervise "+name, func() {
		if superviseExited != nil {
			defer superviseExited(name)
		}
		wait := SuperviseBackoff
		for {
			started := time.Now()
			if !runRecovered(ctx, logger, name, run) {
				return
			}
			if time.Since(started) >= SuperviseHealthy {
				wait = SuperviseBackoff
			}
			logger.Printf("supervise: %s panicked; restarting it in %s", name, wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if wait *= 2; wait > SuperviseBackoffMax {
				wait = SuperviseBackoffMax
			}
		}
	})
}

// runRecovered runs run once and reports whether it panicked.
func runRecovered(ctx context.Context, logger *log.Logger, name string, run func(context.Context)) (panicked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			panicked = true
			metrics.Panics.WithLabelValues("goroutine").Inc()
			logger.Printf("PANIC in background loop %s: %v\n%s", name, rec, debug.Stack())
		}
	}()
	run(ctx)
	return false
}

// recoverGoroutine is safeGo's recover: deferred DIRECTLY (recover only works
// in the deferred function itself), it counts and logs a panic of the
// goroutine named what. safeGo defers it outside fn; a goroutine that needs a
// defer of its own to run AFTER the panic is logged defers it inside fn, below
// that defer (destroyAgent: destroyInFlight's Done).
func recoverGoroutine(logger *log.Logger, what string) {
	if rec := recover(); rec != nil {
		metrics.Panics.WithLabelValues("goroutine").Inc()
		logger.Printf("PANIC in goroutine %s: %v\n%s", what, rec, debug.Stack())
	}
}

// permissionsPolicy is the Permissions-Policy every response carries.
//
// 🔴 IT NAMES THE MICROPHONE, AND ONLY THE MICROPHONE, FOR THIS ORIGIN ALONE.
// The message box's dictation (internal/ui/dictate.go) records through
// getUserMedia; a top-level page of this origin may do that by the browser's
// default already, so this widens nothing — it pins the grant to `self`, which
// is what stops any frame of another origin from ever asking (the CSP's
// frame-ancestors 'none' already keeps muster out of other pages' frames). No
// other feature is listed, so none changes from the browser's default.
const permissionsPolicy = "microphone=(self)"

// securityHeaders sets conservative security headers on every response. The app
// is server-rendered HTML + vendored htmx/idiomorph + inline <script> blocks and
// `hx-on:` attributes. htmx compiles `hx-on:` handlers via `new Function`, so the
// script-src needs BOTH 'unsafe-inline' (inline scripts + hx-on attrs) AND
// 'unsafe-eval' (the Function constructor) or modal-close/form-reset break.
// ⚠ So the CSP is NOT an exfiltration barrier: an injected inline script can
// always navigate (location = …), which no CSP directive governs. What it does
// earn: framing (frame-ancestors, clickjacking), base-uri and form-action
// hijacking, external SCRIPTS and styles (script-src/style-src 'self'), and
// keeping the pages' own fetches same-origin (connect-src 'self', plus a Faro
// collector when telemetry is on). img-src also admits other origins' brand
// icon at one fixed path (serverSwitcherImgSrc). Referrer-Policy:no-referrer
// keeps the magic-link token out of the Referer header.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy("'none'", ""))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", permissionsPolicy)
		next.ServeHTTP(w, r)
	})
}

// serverSwitcherImgSrc is what the web app's server switcher may DRAW from
// another origin (docs/design/web-server-switcher.md): another muster's
// current logo icon, as a plain <img>, at the one path every muster serves it
// on. The operator's decision on review ("Logo via <img>, no CORS"): nothing on
// another origin is READ — connect-src stays 'self', and /api/instance and
// /brand/* send no Access-Control-Allow-Origin.
//
// 🔴 ONE EXACT PATH, NOT AN ORIGIN AND NOT A PREFIX. The servers are the
// person's own list, kept in their browser, so no origin can be named here; a
// path source with no trailing slash matches that path only. http only for the
// two loopback hosts the switcher accepts (development). ⚠ A source's path is
// not checked after a redirect, and an image URL can carry a query: neither
// matters for exfiltration, which a navigation already allows (above).
// ⚠ A host wildcard does not match an IP address in a browser that follows the
// CSP spec, so a server addressed by IP (other than 127.0.0.1) may show no
// logo; the switcher then draws its colour and label.
const serverSwitcherImgSrc = "https://*:*/brand/icon-192.png " +
	"http://localhost:*/brand/icon-192.png http://127.0.0.1:*/brand/icon-192.png"

// contentSecurityPolicy is the CSP, with frameAncestors as its frame-ancestors
// source: 'none' for every response but allowSameOriginFrame's. frameSrc, when
// not empty, is a frame-src directive's sources; "" writes none, so the
// policy is exactly what it was before plugins could be framed (child frames
// then fall back to default-src 'self').
func contentSecurityPolicy(frameAncestors, frameSrc string) string {
	connectSrc := "'self'"
	if telemetryConnectSrc != "" {
		connectSrc += " " + telemetryConnectSrc
	}
	csp := "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: " + serverSwitcherImgSrc + "; " +
		"connect-src " + connectSrc + "; " +
		"frame-ancestors " + frameAncestors + "; "
	if frameSrc != "" {
		csp += "frame-src " + frameSrc + "; "
	}
	return csp + "base-uri 'self'; " +
		"form-action 'self'"
}

// allowSameOriginFrame lets THIS ORIGIN, and only it, frame the response: the
// four pages of a Rendered | Terminal switch, whose other view is opened in a
// frame over the one on screen (ui/view_swap.go) — frame-ancestors 'self' and
// X-Frame-Options SAMEORIGIN in place of the 'none' / DENY every other
// response keeps. An uploaded file is served with a sandbox CSP, so its origin
// is opaque, not 'self': it cannot frame these either.
//
// frameSrc is what the page itself may frame: "" (no frame-src, so
// default-src 'self') on every page but a mirrored window's two session views,
// which pass viewerFrameSrc — 'self' plus the origins of the owner's live
// viewer plugins, for the session-view chip's Show here frame
// (docs/plugins.md#the-session-view-chip).
func allowSameOriginFrame(w http.ResponseWriter, frameSrc string) {
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'self'", frameSrc))
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
}

// accessLog logs one line per request (method, path, status, duration). It skips
// the high-frequency, low-value health + SSE endpoints to keep logs readable.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/readyz", "/events":
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		dur := time.Since(start)
		metrics.ObserveHTTP(routeLabel(r), r.Method, sw.status, dur)
		s.logger.Printf("%s %s %d %s", r.Method, accessLogPath(r.URL), sw.status, dur.Round(time.Millisecond))
	})
}

// loggedPath is the path accessLog writes. Anything under /auth/claim/ is
// cut to that prefix: the claim token lives in the URL fragment now (never
// sent), but an old-style or hand-made /auth/claim/<token> request must still
// not put a credential into the log. Defence in depth.
//
// The same holds for the invite and reset links (accounts PR 5): their tokens
// ride in the fragment too, and a hand-made path form is cut the same way.
// accessLogPath is a request's path as the access log writes it.
//
// 🔴 REDACT ON THE DECODED PATH, THEN ESCAPE. Redaction must see the path the
// mux routes on — the DECODED one — or `/auth/%63laim/<token>` (which reaches
// the claim route) would log its token. And what is logged must be ESCAPED: a
// decoded path is attacker-chosen text, and a %0A in it would start a forged
// line of its own in this log.
func accessLogPath(u *url.URL) string {
	if red := loggedPath(u.Path); red != u.Path {
		return red // a fixed prefix plus "[redacted]": nothing of the request's
	}
	return u.EscapedPath()
}

func loggedPath(p string) string {
	// A page's share link (/s/<token>…) is a bearer capability: none of it
	// is written, whatever follows the token.
	for _, prefix := range []string{"/auth/claim/", "/invite/", "/auth/reset/", pages.SharePrefix} {
		if strings.HasPrefix(p, prefix) {
			return prefix + "[redacted]"
		}
	}
	return p
}

// routeLabel returns the matched ServeMux pattern for the request as a bounded
// metric label, falling back to "unmatched" when no route matched (the 404 path,
// finally made visible). r.Pattern is set by the mux during ServeHTTP (Go 1.23+),
// so this MUST be read AFTER next.ServeHTTP returns. The raw path is deliberately
// NOT used — it carries ids/tokens and would blow up label cardinality.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	return r.Pattern
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController (used
// for SSE/WebSocket Flush/Hijack) reaches the real writer through the recorder.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush implements http.Flusher so SSE/streaming handlers that type-assert
// w.(http.Flusher) directly (e.g. handleAgentLogsStream) still work when the
// response is wrapped for access logging — a bare type assertion does NOT follow
// Unwrap, so without this the assertion fails and the handler 500s. Delegates to
// the underlying writer's Flusher when it has one.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
