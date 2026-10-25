package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ZacxDev/muster/internal/telemetry"
)

// sampled sends n GETs of each path through the real middleware with
// sampler installed and returns how many server spans were exported per path.
func sampled(t *testing.T, sampler sdktrace.Sampler, n int, header http.Header, paths ...string) map[string]int {
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
			for k, v := range header {
				req.Header[k] = v
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	got := map[string]int{}
	for _, s := range exp.GetSpans() {
		for _, a := range s.Attributes {
			if a.Key == "url.path" {
				got[a.Value.AsString()]++
			}
		}
	}
	return got
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

// TestASampledParentIsAlwaysFollowed: a polling request that arrives with a
// SAMPLED traceparent (a page fetch inside its open's trace) is kept; with an
// unsampled one it is not.
func TestASampledParentIsAlwaysFollowed(t *testing.T) {
	const n = 50
	on := http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}}
	off := http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00"}}
	s := telemetry.Sampler(telemetry.DefaultSampleRatio)
	if got := sampled(t, s, n, on, "/ui/agents")["/ui/agents"]; got != n {
		t.Errorf("a sampled parent was followed %d of %d times", got, n)
	}
	if got := sampled(t, s, n, off, "/ui/agents")["/ui/agents"]; got != 0 {
		t.Errorf("an unsampled parent's child was kept %d times", got)
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
