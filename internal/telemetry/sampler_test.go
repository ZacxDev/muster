package telemetry_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ZacxDev/muster/internal/telemetry"
)

// sampled sends n GETs of each path through the real middleware with
// sampler installed and returns how many server spans were exported per path.
func sampled(t *testing.T, sampler sdktrace.Sampler, n int, header func() http.Header, paths ...string) map[string]int {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sampler))
	restore := telemetry.Use(tp)
	defer func() { restore(); _ = tp.Shutdown(context.Background()) }()
	mux := http.NewServeMux()
	for _, p := range []string{"GET /mirror/view", "GET /mirror/view/ws", "GET /mirror/terminal", "GET /mirror/terminal/ws",
		"GET /agents/{name}", "GET /agents/{name}/ws", "GET /ui/agents", "GET /events", "GET /ui/tasks/review-count"} {
		mux.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	srv := httptest.NewServer(telemetry.Middleware(mux))
	defer srv.Close()
	for _, p := range paths {
		for i := 0; i < n; i++ {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+p, nil)
			if header != nil {
				for k, v := range header() {
					req.Header[k] = v
				}
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	// Counted by the route the mux matched (a span's url.path is replaced
	// by it, so it names no agent): /agents/some-agent is /agents/{name}.
	got := map[string]int{}
	for _, s := range exp.GetSpans() {
		for _, a := range s.Attributes {
			if a.Key == "http.route" {
				got[a.Value.AsString()]++
			}
		}
	}
	for _, p := range paths {
		if r := routeOf(p); r != p {
			got[p] = got[r]
		}
	}
	return got
}

// routeOf is the mux pattern a test path matches.
func routeOf(p string) string {
	switch p {
	case "/agents/some-agent":
		return "/agents/{name}"
	case "/agents/some-agent/ws":
		return "/agents/{name}/ws"
	}
	return p
}

var (
	pollingPaths = []string{"/ui/agents", "/events", "/ui/tasks/review-count"}
	openPaths    = []string{"/mirror/view", "/mirror/view/ws", "/mirror/terminal", "/mirror/terminal/ws", "/agents/some-agent", "/agents/some-agent/ws"}
)

// TestPollingIsMostlyUnsampledAndOpensAlwaysAre: at the default ratio the
// polling routes keep a small share of their traces, and every open route
// keeps every one.
//
// 🔴 THE MEASUREMENT IS SHOWN TO GO RED: the same requests under an
// always-on sampler keep every polling trace, and the bound below refuses
// that — so a sampler that kept everything (the SDK's default) fails here.
func TestPollingIsMostlyUnsampledAndOpensAlwaysAre(t *testing.T) {
	const n = 200
	// At 5% the expected count is 10 of 200; 40 is ~9 standard deviations
	// out, so the bound does not flake, and a sampler at 20% or more fails it.
	const pollingBound = 40
	tooMany := func(got map[string]int) []string {
		var out []string
		for _, p := range pollingPaths {
			if got[p] > pollingBound {
				out = append(out, p)
			}
		}
		return out
	}

	control := sampled(t, sdktrace.AlwaysSample(), n, nil, pollingPaths...)
	if bad := tooMany(control); len(bad) != len(pollingPaths) {
		t.Fatalf("CONTROL: an always-on sampler should exceed the bound on every polling route, got %v", control)
	}

	got := sampled(t, telemetry.Sampler(telemetry.DefaultSampleRatio), n, nil, append(append([]string{}, pollingPaths...), openPaths...)...)
	if bad := tooMany(got); len(bad) != 0 {
		t.Errorf("polling routes kept more than %d of %d traces: %v", pollingBound, n, got)
	}
	total := 0
	for _, p := range pollingPaths {
		total += got[p]
	}
	if total == 0 {
		t.Errorf("POSITIVE CONTROL: no polling trace was kept at all at ratio %g (%v)", telemetry.DefaultSampleRatio, got)
	}
	for _, p := range openPaths {
		if got[p] != n {
			t.Errorf("%s kept %d of %d traces; every open must be sampled", p, got[p], n)
		}
	}
	t.Logf("kept of %d: %v", n, got)
}

// browserParent is a traceparent as the page's Faro sends it on a fetch:
// SAMPLED (its default sampling rate is 1), in a trace of its own — each
// request a fresh trace id, so a ratio decision (made on the trace id) is a
// real draw per request, not one draw repeated.
func browserParent() http.Header {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return http.Header{"Traceparent": {fmt.Sprintf("00-%x-%x-01", b[:16], b[16:])}}
}

const fixedParent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

// TestABrowserParentDoesNotForcePolls: a polling request carrying the
// browser's SAMPLED traceparent is kept at the ratio, not because the page
// sampled it — Faro samples every page, so following it would trace every
// ask poll, list refresh and badge poll in full.
//
// 🔴 THE RED CASE IS RUN: the same requests under the SDK's parent-based
// sampler (which follows any sampled parent) keep every one, beyond the bound.
func TestABrowserParentDoesNotForcePolls(t *testing.T) {
	const n, bound = 200, 40
	control := sampled(t, sdktrace.ParentBased(sdktrace.TraceIDRatioBased(telemetry.DefaultSampleRatio)), n, browserParent, pollingPaths...)
	for _, p := range pollingPaths {
		if control[p] <= bound {
			t.Fatalf("CONTROL: a parent-based sampler kept only %d of %d for %s under a sampled browser parent", control[p], n, p)
		}
	}
	got := sampled(t, telemetry.Sampler(telemetry.DefaultSampleRatio), n, browserParent, pollingPaths...)
	total := 0
	for _, p := range pollingPaths {
		total += got[p]
		if got[p] > bound {
			t.Errorf("%s kept %d of %d under a sampled browser parent; it must be kept at the ratio", p, got[p], n)
		}
	}
	if total == 0 {
		t.Errorf("POSITIVE CONTROL: nothing kept at ratio %g across %d draws — the draw is not per request", telemetry.DefaultSampleRatio, n*len(pollingPaths))
	}
	t.Logf("browser parent, kept of %d: muster %v, parent-based %v", n, got, control)
}

// TestAnOpenRouteJoinsItsBrowserParent: an open route — its page GET and its
// socket UPGRADE (the ?tp= path, muster's own socket span) — is sampled under
// a browser parent and is that parent's child, in its trace.
func TestAnOpenRouteJoinsItsBrowserParent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := telemetry.NewProvider(exp, telemetry.DefaultSampleRatio, true)
	restore := telemetry.Use(tp)
	defer func() { restore(); _ = tp.Shutdown(context.Background()) }()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mirror/view", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /mirror/view/ws", func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewServer(telemetry.Middleware(mux))
	defer srv.Close()
	for _, c := range []struct {
		path    string
		upgrade bool
	}{{"/mirror/view?host=h", false}, {"/mirror/view/ws?host=h&tp=00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", true}} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+c.path, nil)
		if c.upgrade {
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Connection", "Upgrade")
		} else {
			req.Header.Set("Traceparent", fixedParent)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d span(s) kept, want both opens", len(spans))
	}
	for _, s := range spans {
		if s.Parent.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || s.Parent.SpanID().String() != "b7ad6b7169203331" {
			t.Errorf("%q is not the browser span's child (parent %s/%s)", s.Name, s.Parent.TraceID(), s.Parent.SpanID())
		}
	}
}

// TestInProcessChildrenAndClosesFollowTheirOpen: a child of a sampled
// in-process span is sampled (a hub request under its open), and a ROOT
// linked to a sampled span (a socket's close) is sampled — at a ratio of 0,
// so nothing but the rule can keep them.
func TestInProcessChildrenAndClosesFollowTheirOpen(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := telemetry.NewProvider(exp, 0, true)
	restore := telemetry.Use(tp)
	defer func() { restore(); _ = tp.Shutdown(context.Background()) }()
	tr := tp.Tracer("t")
	// An open, kept by its route.
	ctx, open := tr.Start(context.Background(), "GET", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("http.request.method", "GET"), attribute.String("url.path", "/mirror/view")))
	_, child := tr.Start(ctx, "mirror.watch")
	child.End()
	open.End()
	_, closed := tr.Start(context.Background(), "closed", trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: open.SpanContext()}))
	closed.End()
	// A poll's root at ratio 0: dropped (the control that 0 means 0).
	_, poll := tr.Start(context.Background(), "GET", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("http.request.method", "GET"), attribute.String("url.path", "/ui/agents")))
	poll.End()
	names := map[string]bool{}
	for _, s := range exp.GetSpans() {
		names[s.Name] = true
	}
	if !names["mirror.watch"] || !names["closed"] || len(names) != 3 {
		t.Errorf("kept %v: want the open, its child and its close, and not the poll", names)
	}
}

func TestOpenRoute(t *testing.T) {
	for path, want := range map[string]bool{
		"/mirror/view": true, "/mirror/view/ws": true, "/mirror/terminal": true, "/mirror/terminal/ws": true,
		"/agents/x": true, "/agents/x/ws": true, "/agents/x/terminal": true, "/agents/x/terminal/ws": true,
		"/agents": false, "/agents/": false, "/agents/x/logs/stream": false, "/ui/agents": false, "/mirror/view/send": false, "/events": false,
	} {
		if got := telemetry.OpenRoute(path); got != want {
			t.Errorf("OpenRoute(%q) = %v, want %v", path, got, want)
		}
	}
}
