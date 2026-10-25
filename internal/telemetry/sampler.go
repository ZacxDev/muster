package telemetry

import (
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// EnvSampleRatio is the share of traces kept for requests that are not part
// of opening a session (MUSTER_TRACE_SAMPLE_RATIO, 0–1).
const EnvSampleRatio = "MUSTER_TRACE_SAMPLE_RATIO"

// DefaultSampleRatio is EnvSampleRatio's default: one trace in twenty for
// everything that is not an open. The Agents list re-renders, the event
// stream and the badge poll are most of a busy page's requests, and at 1.0
// they would bury the opens this exists for.
const DefaultSampleRatio = 0.05

// Sampler is muster's sampler. In order:
//
//  1. a request that OPENS a session — the GET of a mirrored view, a mirrored
//     terminal or an agent's page, and each of their sockets (OpenRoute) — is
//     ALWAYS sampled, whatever its parent says (a socket's ?tp= parent is
//     still its parent: sampling never changes the trace a span is in);
//  2. any OTHER server span whose parent is REMOTE — a page's fetch, XHR or
//     poll carrying the browser's traceparent — is kept at ratio, NOT because
//     the browser sampled it: Faro samples every page at 1, so following it
//     would trace every ask poll, list refresh and badge poll in full;
//  3. anything else with a parent follows it (parent-based): an in-process
//     child of a sampled span is sampled, so a hub request, a query or a host
//     span (whose remote parent is muster's own span, never a server span of
//     the host's) is kept exactly when its open is;
//  4. a root with a sampled LINK follows it: a socket's close (StreamClosed)
//     is kept exactly when its open was;
//  5. any other root is kept at ratio (trace-id based, so the decision is the
//     same on every process that sees the trace).
//
// The route is read off the server span's START attributes (otelhttp and
// serveSocket set the method and url.path there), since the span's name is
// only the method until the mux has matched.
func Sampler(ratio float64) sdktrace.Sampler {
	return openSampler{ratio: sdktrace.TraceIDRatioBased(ratio), desc: fmt.Sprintf("MusterOpenSampler{ratio=%g}", ratio)}
}

type openSampler struct {
	ratio sdktrace.Sampler
	desc  string
}

func (s openSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	psc := trace.SpanContextFromContext(p.ParentContext)
	ts := psc.TraceState()
	keep := sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample, Tracestate: ts}
	if openRequest(p.Attributes) {
		return keep
	}
	if psc.IsValid() {
		if psc.IsRemote() && p.Kind == trace.SpanKindServer {
			return s.ratio.ShouldSample(p)
		}
		if psc.IsSampled() {
			return keep
		}
		return sdktrace.SamplingResult{Decision: sdktrace.Drop, Tracestate: ts}
	}
	for _, l := range p.Links {
		if l.SpanContext.IsSampled() {
			return keep
		}
	}
	return s.ratio.ShouldSample(p)
}

func (s openSampler) Description() string { return s.desc }

// openRequest reports whether a span's start attributes name a GET of an
// open route.
func openRequest(attrs []attribute.KeyValue) bool {
	var method, path string
	for _, a := range attrs {
		switch a.Key {
		case "http.request.method":
			method = a.Value.AsString()
		case "url.path":
			path = a.Value.AsString()
		}
	}
	return method == "GET" && OpenRoute(path)
}

// OpenRoute reports whether path is one of the routes that open a session:
// /mirror/view, /mirror/terminal, /agents/{name}, and their sockets
// (/mirror/view/ws, /mirror/terminal/ws, /agents/{name}/ws,
// /agents/{name}/terminal and /agents/{name}/terminal/ws).
func OpenRoute(path string) bool {
	switch path {
	case "/mirror/view", "/mirror/view/ws", "/mirror/terminal", "/mirror/terminal/ws":
		return true
	}
	rest, ok := strings.CutPrefix(path, "/agents/")
	if !ok {
		return false
	}
	name, tail, _ := strings.Cut(rest, "/")
	if name == "" {
		return false
	}
	switch tail {
	case "", "ws", "terminal", "terminal/ws":
		return true
	}
	return false
}

// sampleRatio is getenv's EnvSampleRatio, or DefaultSampleRatio when it is
// unset; a value that is not a number in [0, 1] is an error.
func sampleRatio(getenv func(string) string) (float64, error) {
	v := strings.TrimSpace(getenv(EnvSampleRatio))
	if v == "" {
		return DefaultSampleRatio, nil
	}
	r, err := strconv.ParseFloat(v, 64)
	if err != nil || r < 0 || r > 1 {
		return 0, fmt.Errorf("telemetry: %s=%q is not a number from 0 to 1", EnvSampleRatio, v)
	}
	return r, nil
}
