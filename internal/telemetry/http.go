package telemetry

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// TraceparentQuery is the query parameter a browser socket carries its trace
// context in. A browser cannot set a header on a WebSocket's upgrade request,
// so the page puts its open span's traceparent here, and Middleware moves it
// into the header otelhttp reads — the socket's server span is then a child
// of the browser's open, in the same trace.
const TraceparentQuery = "tp"

// untraced is the paths that get no server span: probes and scrapes, the
// static files, the browser telemetry relay itself (a span per Faro post
// would be a trace per trace), and a mirror host's own tunnel — a span as
// long as the host's connection, minutes to days, that describes no open.
//
// 🔴 AND EVERY PATH THAT CARRIES A CREDENTIAL IN THE PATH ITSELF: otelhttp
// records url.path, so a claim, invite or reset link would put its token into
// a span. These are the prefixes accessLog redacts (internal/api's
// loggedPath); internal/api's TestCredentialPathsAreNeverTraced pins the two lists together.
var untraced = []string{"/health", "/readyz", "/metrics", "/static/", "/sw.js", "/manifest.webmanifest", "/favicon.ico",
	"/telemetry/faro", "/mirror/connect", "/auth/claim/", "/invite/", "/auth/reset/"}

// TracedPath reports whether a request for path gets a server span.
func TracedPath(path string) bool {
	for _, p := range untraced {
		if path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) {
			return false
		}
	}
	return true
}

func traced(r *http.Request) bool { return TracedPath(r.URL.Path) }

// Middleware wraps next in a server span per request (otelhttp), named for
// the ROUTE PATTERN the mux matched ("GET /mirror/view"), never the raw path,
// which carries ids. Off (Enabled false), it returns next itself.
//
// The response carries `Server-Timing: traceparent;desc="00-…"`: the browser
// reads it off the navigation entry (performance.getEntriesByType), so the
// page's own spans join the trace of the request that served it.
//
// ⚠ next MUST BE (OR END IN) THE ServeMux, CALLED WITH THE REQUEST IT IS
// HANDED: the mux writes the matched pattern onto that request (r.Pattern),
// which is read after it returns. accessLog relies on the same thing.
func Middleware(next http.Handler) http.Handler {
	if !Enabled() {
		return next
	}
	named := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := trace.SpanFromContext(r.Context())
		if tp := SpanTraceparent(span.SpanContext()); tp != "" {
			w.Header().Add("Server-Timing", `traceparent;desc="`+tp+`"`)
		}
		next.ServeHTTP(w, r)
		// otelhttp renames the span from r.Pattern itself once the handler
		// returns ("GET /agents/{name}"); the route is also an attribute.
		if r.Pattern != "" {
			route := r.Pattern
			if i := strings.IndexByte(route, ' '); i >= 0 {
				route = route[i+1:]
			}
			span.SetAttributes(semconv.HTTPRoute(route))
		}
	})
	h := otelhttp.NewHandler(named, "http", otelhttp.WithFilter(traced))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") == "" {
			if tp := r.URL.Query().Get(TraceparentQuery); tp != "" && ValidTraceparent(tp) {
				r.Header.Set("traceparent", tp)
			}
		}
		h.ServeHTTP(w, r)
	})
}

// Annotate adds attrs to the request's server span (a no-op while off, or
// for an untraced path).
func Annotate(r *http.Request, attrs ...attribute.KeyValue) {
	if !Enabled() {
		return
	}
	trace.SpanFromContext(r.Context()).SetAttributes(attrs...)
}
