package telemetry

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
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
// 🔴 A WEBSOCKET'S SPAN IS ITS OPEN, NOT ITS LIFE. otelhttp's span lasts as
// long as the handler, which for a socket is minutes to hours, and everything
// done under it would grow one trace for as long. A socket upgrade instead
// gets a server span of muster's own that the handler ends when the open is
// over (OpenDone: a rendered view's first batch, a terminal's first output, a
// chat socket's accept) — or when it returns, if it never got there. Work
// after that runs Detached; the socket's end is a span of its own, a ROOT
// linked to the open (StreamClosed), so a deploy's disconnects can be found
// without a trace that never ends.
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
		routeAttrs(span, r)
	})
	h := otelhttp.NewHandler(named, "http", otelhttp.WithFilter(func(r *http.Request) bool { return traced(r) && !isSocket(r) }))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") == "" {
			if tp := r.URL.Query().Get(TraceparentQuery); tp != "" && ValidTraceparent(tp) {
				r.Header.Set("traceparent", tp)
			}
		}
		if isSocket(r) && traced(r) {
			serveSocket(named, w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// routeAttrs names span for r's matched route: http.route, and url.path
// REPLACED by the pattern (the raw path carries agent names and ids; the
// exporter drops a url.path no route replaced — scrubExporter).
func routeAttrs(span trace.Span, r *http.Request) {
	if r.Pattern == "" {
		return
	}
	route := r.Pattern
	if i := strings.IndexByte(route, ' '); i >= 0 {
		route = route[i+1:]
	}
	span.SetAttributes(semconv.HTTPRoute(route), attribute.String("url.path", route))
}

// isSocket reports whether r asks for a WebSocket upgrade.
func isSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

type socketKey struct{}

// socketSpan is a socket's server span and whether its open has ended it.
type socketSpan struct {
	span trace.Span
	once sync.Once
}

func (s *socketSpan) end(r *http.Request, reason string) {
	s.once.Do(func() {
		if r.Pattern != "" {
			s.span.SetName(r.Pattern)
		}
		routeAttrs(s.span, r)
		End(s.span, reason)
	})
}

// serveSocket serves a socket upgrade under a server span of its own, which
// OpenDone ends (or the handler's return, at the latest).
func serveSocket(next http.Handler, w http.ResponseWriter, r *http.Request) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := Tracer().Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
		attribute.String("http.request.method", r.Method), attribute.String("url.path", r.URL.Path),
		attribute.String("network.protocol.name", "websocket")))
	ss := &socketSpan{span: span}
	r = r.WithContext(context.WithValue(ctx, socketKey{}, ss))
	defer ss.end(r, "")
	next.ServeHTTP(w, r)
}

// OpenDone ends r's socket span — the socket's open is over — with reason
// ("" or ReasonOK for a good one). A no-op for a request that is not a traced
// socket, and after the first call.
func OpenDone(r *http.Request, reason string) {
	if ss, ok := r.Context().Value(socketKey{}).(*socketSpan); ok {
		ss.end(r, reason)
	}
}

// Detached is ctx with no span: work a long-lived socket does after its open
// (each turn, each batch) starts no span in the open's trace. Its values and
// cancellation are ctx's.
func Detached(ctx context.Context) context.Context {
	return trace.ContextWithSpan(ctx, noopSpan)
}

var noopSpan = trace.SpanFromContext(context.Background())

// StreamClosed records a socket's END as a span of its own: a root, LINKED to
// the socket's open span, carrying the reason (an error status for anything
// but ok, client_left and ended) and how long the socket lived. It is kept
// exactly when its open was (Sampler follows a sampled link of a root).
func StreamClosed(r *http.Request, reason string, lived time.Duration) {
	if !Enabled() {
		return
	}
	sc := trace.SpanContextFromContext(r.Context())
	if !sc.IsValid() {
		return
	}
	name := r.Method + " socket closed"
	if r.Pattern != "" {
		name = r.Pattern + " closed"
	}
	_, span := Tracer().Start(context.Background(), name, trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: sc}),
		trace.WithAttributes(attribute.String("muster.close_reason", reason), attribute.Int64("muster.lived_ms", lived.Milliseconds())))
	if reason == "ended" {
		reason = ReasonOK
	}
	End(span, reason)
}

// Annotate adds attrs to the request's server span (a no-op while off, or
// for an untraced path).
func Annotate(r *http.Request, attrs ...attribute.KeyValue) {
	if !Enabled() {
		return
	}
	trace.SpanFromContext(r.Context()).SetAttributes(attrs...)
}
