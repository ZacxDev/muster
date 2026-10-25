package telemetry_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ZacxDev/muster/internal/telemetry"
)

// recorder installs an in-memory exporter as the process's provider for one
// test, and returns it.
func recorder(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restore := telemetry.Use(tp)
	t.Cleanup(func() { restore(); _ = tp.Shutdown(context.Background()) })
	return exp
}

type plain struct{ hits int }

func (p *plain) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.hits++
	_, _ = io.WriteString(w, "ok")
}

// TestOffIsANoOp: with no OTLP endpoint in the environment, Setup turns
// nothing on, and every hook muster has into a request, a query or a frame
// does nothing — the bytes a request gets back are the bytes it got before.
func TestOffIsANoOp(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	shutdown, err := telemetry.Setup(context.Background(), getenv, telemetry.Options{Service: "muster-server"})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if telemetry.Enabled() {
		t.Fatal("tracing is on with no endpoint configured")
	}
	h := &plain{}
	if got := telemetry.Middleware(h); got != http.Handler(h) {
		t.Fatalf("Middleware wrapped the handler while off: %T", got)
	}
	if telemetry.QueryTracer() != nil {
		t.Fatal("QueryTracer is non-nil while off: the pool would be built with a tracer")
	}
	ctx := context.Background()
	if tp := telemetry.Traceparent(ctx); tp != "" {
		t.Fatalf("Traceparent = %q while off", tp)
	}
	sctx, span := telemetry.Start(ctx, "x")
	if sctx != ctx || span.IsRecording() {
		t.Fatal("Start made a span while off")
	}
	if got := telemetry.WithTraceparent(ctx, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"); got != ctx {
		t.Fatal("WithTraceparent changed the context while off")
	}

	// 🔴 THE POSITIVE CONTROL: the SAME checks with an endpoint set do turn it
	// on, so each "off" above is the off-state and not a hook that never works.
	env[telemetry.EnvEndpoint] = "http://127.0.0.1:1"
	if !telemetry.Configured(getenv) {
		t.Fatal("an endpoint did not configure tracing")
	}
	env[telemetry.EnvSDKDisabled] = "true"
	if telemetry.Configured(getenv) {
		t.Fatal("OTEL_SDK_DISABLED=true did not keep tracing off")
	}
	recorder(t)
	if got := telemetry.Middleware(h); got == http.Handler(h) {
		t.Fatal("Middleware did not wrap the handler while on")
	}
	if telemetry.QueryTracer() == nil {
		t.Fatal("QueryTracer is nil while on")
	}
}

// TestMiddlewareNamesSpansByRoutePattern: a request's server span is named
// for the route PATTERN, never the path with its ids, and the response names
// the span in Server-Timing so the page can join its trace.
func TestMiddlewareNamesSpansByRoutePattern(t *testing.T) {
	exp := recorder(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents/{name}", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "page") })
	srv := httptest.NewServer(telemetry.Middleware(mux))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/agents/some-agent-name?q=private")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d span(s), want 1", len(spans))
	}
	s := spans[0]
	if s.Name != "GET /agents/{name}" {
		t.Errorf("span name %q, want the route pattern", s.Name)
	}
	route := ""
	for _, a := range s.Attributes {
		if a.Key == "http.route" {
			route = a.Value.AsString()
		}
		if strings.Contains(a.Value.Emit(), "private") {
			t.Errorf("attribute %s carries the query string: %q", a.Key, a.Value.Emit())
		}
	}
	if route != "/agents/{name}" {
		t.Errorf("http.route = %q", route)
	}
	want := `traceparent;desc="` + telemetry.SpanTraceparent(s.SpanContext) + `"`
	if got := resp.Header.Get("Server-Timing"); got != want {
		t.Errorf("Server-Timing = %q, want %q", got, want)
	}
}

// TestTheTraceparentQueryJoinsTheBrowsersTrace: a socket cannot send a
// header, so ?tp= is its trace context — a valid one is the server span's
// parent, a malformed one is ignored.
func TestTheTraceparentQueryJoinsTheBrowsersTrace(t *testing.T) {
	exp := recorder(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mirror/view/ws", func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewServer(telemetry.Middleware(mux))
	defer srv.Close()
	const parent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	for _, tc := range []struct {
		tp     string
		joined bool
	}{{parent, true}, {"00-zz-bogus-01", false}, {"", false}} {
		exp.Reset()
		resp, err := http.Get(srv.URL + "/mirror/view/ws?host=h&tp=" + tc.tp)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		s := exp.GetSpans()[0]
		joined := s.Parent.TraceID().String() == "0af7651916cd43dd8448eb211c80319c" && s.Parent.SpanID().String() == "b7ad6b7169203331"
		if joined != tc.joined {
			t.Errorf("tp=%q: joined the browser's span = %v, want %v", tc.tp, joined, tc.joined)
		}
	}
}

// TestTheQueryTracerTracesOnlyInsideATrace: a query under a span gets a child
// span with its statement normalised; a query with no span starts none, and
// its end never ends the caller's span.
func TestTheQueryTracerTracesOnlyInsideATrace(t *testing.T) {
	exp := recorder(t)
	qt := telemetry.QueryTracer()

	ctx, parent := telemetry.Start(context.Background(), "request")
	qctx := qt.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT id FROM agents WHERE name = 'secret-name' AND n = 42", Args: []any{1}})
	qt.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{})
	telemetry.End(parent, "")

	bare := context.Background()
	if got := qt.TraceQueryStart(bare, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"}); got != bare {
		t.Fatal("a query outside any trace started a span")
	}
	// The END of an untraced query, given a context whose span is a caller's:
	// that span must stay open.
	_, caller := telemetry.Start(context.Background(), "caller")
	qt.TraceQueryEnd(trace.ContextWithSpan(context.Background(), caller), nil, pgx.TraceQueryEndData{})
	if !caller.IsRecording() {
		t.Fatal("an untraced query's end ended its caller's span")
	}
	telemetry.End(caller, "")

	spans := exp.GetSpans()
	var q *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "db SELECT" {
			q = &spans[i]
		}
	}
	if q == nil {
		t.Fatalf("no db span among %d", len(spans))
	}
	if q.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Error("the query span is not the request's child")
	}
	for _, a := range q.Attributes {
		if strings.Contains(a.Value.Emit(), "secret-name") || strings.Contains(a.Value.Emit(), "42") {
			t.Errorf("attribute %s carries a literal: %q", a.Key, a.Value.Emit())
		}
	}
	if got := telemetry.NormalizeSQL("SELECT  a FROM t WHERE b = 'x''y'\n AND c = 3.5"); got != "SELECT a FROM t WHERE b = ? AND c = ?" {
		t.Errorf("NormalizeSQL = %q", got)
	}
}

// TestAnExportedSpanNamesNoPersonOrPath: what a deployment exports (the
// provider Setup builds, NewProvider) carries no caller address, port or user
// agent, and its url.path is the ROUTE PATTERN — never the agent name or the
// raw path. A path no route matched exports no url.path at all.
func TestAnExportedSpanNamesNoPersonOrPath(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := telemetry.NewProvider(exp, 1, true)
	restore := telemetry.Use(tp)
	defer func() { restore(); _ = tp.Shutdown(context.Background()) }()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents/{name}", func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewServer(telemetry.Middleware(mux))
	defer srv.Close()
	for _, p := range []string{"/agents/private-agent-name", "/no-such-page-private-tag"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		req.Header.Set("User-Agent", "UA-SENTINEL-browser")
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("POSITIVE CONTROL: %d span(s) exported, want 2", len(spans))
	}
	paths := map[string]string{}
	for _, s := range spans {
		for _, a := range s.Attributes {
			v := a.Value.Emit()
			switch {
			case a.Key == "client.address" || a.Key == "client.port" || a.Key == "network.peer.address" ||
				a.Key == "network.peer.port" || a.Key == "user_agent.original":
				t.Errorf("%s exports %s=%q", s.Name, a.Key, v)
			case strings.Contains(v, "private") || strings.Contains(v, "UA-SENTINEL") || strings.Contains(v, "203.0.113.7"):
				t.Errorf("%s exports %s=%q", s.Name, a.Key, v)
			case a.Key == "url.path":
				paths[s.Name] = v
			}
		}
	}
	if paths["GET /agents/{name}"] != "/agents/{name}" || len(paths) != 1 {
		t.Errorf("url.path by span: %v — want the pattern for the matched route and none for the unmatched", paths)
	}
}
