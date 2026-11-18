package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// droppedAttributes is what no exported span carries, whoever set it: the
// caller's address and port (otelhttp records the client's and the peer's)
// and its user agent. They identify a person's device and network, and none
// of them answers "which hop was slow".
var droppedAttributes = map[attribute.Key]bool{
	"client.address": true, "client.port": true,
	"network.peer.address": true, "network.peer.port": true,
	"user_agent.original": true,
}

// scrubExporter is next, with every span's attributes cut to what muster's
// spans may carry: droppedAttributes removed, and a url.path that no route
// pattern replaced (Middleware's routeAttrs) removed too — a raw path carries
// agent names and ids, and an unmatched one is whatever a caller typed.
type scrubExporter struct{ next sdktrace.SpanExporter }

func (e scrubExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = scrub(s)
	}
	return e.next.ExportSpans(ctx, out)
}

func (e scrubExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

func scrub(s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	attrs := s.Attributes()
	routed := false
	for _, a := range attrs {
		if a.Key == "http.route" {
			routed = true
		}
	}
	keep := make([]attribute.KeyValue, 0, len(attrs))
	changed := false
	for _, a := range attrs {
		if droppedAttributes[a.Key] || (a.Key == "url.path" && !routed) {
			changed = true
			continue
		}
		keep = append(keep, a)
	}
	if !changed {
		return s
	}
	stub := tracetest.SpanStubFromReadOnlySpan(s)
	stub.Attributes = keep
	return stub.Snapshot()
}

// NewProvider is the tracer provider Setup builds around exp: muster's sampler
// at ratio, and every span scrubbed (scrubExporter) before exp sees it,
// exported synchronously when sync (a test's) or batched. Tests build theirs
// with it, so what they read is what a deployment exports.
func NewProvider(exp sdktrace.SpanExporter, ratio float64, sync bool, opts ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	wrapped := scrubExporter{next: exp}
	o := []sdktrace.TracerProviderOption{sdktrace.WithSampler(Sampler(ratio))}
	if sync {
		o = append(o, sdktrace.WithSyncer(wrapped))
	} else {
		o = append(o, sdktrace.WithBatcher(wrapped))
	}
	return sdktrace.NewTracerProvider(append(o, opts...)...)
}
