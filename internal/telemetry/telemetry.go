// Package telemetry is muster's tracing: OpenTelemetry spans for the
// server (muster-server) and the host agent (muster-mirror), exported over
// OTLP/HTTP, and the W3C trace context that joins them into one trace across
// the browser, the server and the mirror tunnel.
//
// 🔴 OFF UNLESS AN OTLP ENDPOINT IS SET, AND OFF MEANS NOTHING CHANGES. With
// neither OTEL_EXPORTER_OTLP_ENDPOINT nor OTEL_EXPORTER_OTLP_TRACES_ENDPOINT in
// the environment (or with OTEL_SDK_DISABLED=true), Setup installs nothing:
// the global tracer provider stays OpenTelemetry's no-op, Middleware returns
// its handler unwrapped, the database gets no query tracer, no frame on the
// tunnel carries a trace context and no response carries a Server-Timing
// header. The browser suite and a local run see exactly the bytes they saw
// before this package existed (TestOffIsANoOp pins that).
//
// 🔴 A SPAN CARRIES IDS, SIZES, COUNTS, DURATIONS AND STATUS — NEVER CONTENT.
// muster's spans describe operators' private sessions, and the repository is
// public: no attribute, event or status may carry a message, a transcript
// line, pane contents, a window title or label, a prompt, a token, a cookie or
// an error's prose (an error string can quote a path or a value). Errors are
// recorded as a closed reason code (Reason) and nothing else. What otelhttp
// adds on its own is cut at export (scrubExporter): no caller address, port or
// user agent, and url.path is the route PATTERN, never the raw path. The
// mirrored view's and terminal's opens and a hub send are guarded by a test
// that fills every fixture with a sentinel string and asserts it reaches no
// exported span (internal/api's TestNoExportedSpanCarriesPrivateContent); an
// agent's page is not in that test.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// The variables Setup reads by name. Everything else the OTLP exporter and
// the SDK read for themselves (OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES,
// OTEL_EXPORTER_OTLP_HEADERS, …): see
// docs/telemetry.md.
const (
	EnvEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	EnvSDKDisabled    = "OTEL_SDK_DISABLED"
)

// TracerName is the instrumentation scope of every span muster starts.
const TracerName = "github.com/ZacxDev/muster"

// enabled is whether Setup (or a test's Use) installed a real provider.
var enabled atomic.Bool

// Enabled reports whether tracing is on in this process. Everything that
// would cost something when it is off (a wrapped handler, a query tracer, a
// header) asks this first.
func Enabled() bool { return enabled.Load() }

// Options names the process for its resource.
type Options struct {
	// Service is the default service.name; OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES override it.
	Service string
	// Version is service.version.
	Version string
	// Attributes are added to the resource (muster-mirror adds its host).
	Attributes []attribute.KeyValue
	Logger     *log.Logger
}

// Configured reports whether getenv names an OTLP endpoint and does not
// disable the SDK: Setup's whole on/off decision.
func Configured(getenv func(string) string) bool {
	if strings.EqualFold(strings.TrimSpace(getenv(EnvSDKDisabled)), "true") {
		return false
	}
	return strings.TrimSpace(getenv(EnvEndpoint)) != "" || strings.TrimSpace(getenv(EnvTracesEndpoint)) != ""
}

// Setup turns tracing on when getenv names an OTLP endpoint (Configured), and
// returns the flush to run at exit. Off, it installs nothing and returns a
// shutdown that does nothing.
//
// The exporter is OTLP over HTTP/protobuf; it reads the standard OTEL_EXPORTER_OTLP_*
// variables itself (endpoint, headers, timeout). The sampler is muster's own
// (Sampler): every open always, a sampled parent's children always, any other
// root at MUSTER_TRACE_SAMPLE_RATIO. It replaces OTEL_TRACES_SAMPLER, which is
// not read. A ratio that is not a number in [0, 1] keeps tracing off and says
// why, rather than guessing.
func Setup(ctx context.Context, getenv func(string) string, o Options) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !Configured(getenv) {
		return noop, nil
	}
	ratio, err := sampleRatio(getenv)
	if err != nil {
		return noop, err
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop, fmt.Errorf("telemetry: the OTLP trace exporter: %w", err)
	}
	attrs := append([]attribute.KeyValue{semconv.ServiceName(o.Service)}, o.Attributes...)
	if o.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(o.Version))
	}
	// The defaults first, the environment after: OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES win over what the binary names itself.
	res, err := resource.New(ctx, resource.WithAttributes(attrs...), resource.WithTelemetrySDK(), resource.WithFromEnv())
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return noop, fmt.Errorf("telemetry: the resource: %w", err)
	}
	tp := NewProvider(exp, ratio, false, sdktrace.WithResource(res))
	install(tp)
	if o.Logger != nil {
		o.Logger.Printf("telemetry: tracing on (service %s), exporting OTLP/HTTP; opens always sampled, other requests at %g", res.Set().Encoded(attribute.DefaultEncoder()), ratio)
	}
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}

// install makes tp the process's provider and W3C trace context its
// propagator, and turns Enabled on.
func install(tp trace.TracerProvider) {
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	enabled.Store(true)
}

// Use installs tp for a TEST and returns the function that puts back what was
// there (tests in this module run a package's tests one at a time unless they
// say otherwise, and none that traces says otherwise).
func Use(tp trace.TracerProvider) (restore func()) {
	prevTP, prevProp, prevOn := otel.GetTracerProvider(), otel.GetTextMapPropagator(), enabled.Load()
	install(tp)
	return func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		enabled.Store(prevOn)
	}
}

// Tracer is muster's tracer from the current provider: a no-op one while
// tracing is off.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// Start starts a span named name under ctx's span. Off, it returns ctx and a
// no-op span without asking the provider at all.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if !Enabled() {
		return ctx, trace.SpanFromContext(ctx)
	}
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartAt is Start with an explicit start time (a phase measured before its
// span could be opened).
func StartAt(ctx context.Context, name string, at time.Time, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if !Enabled() {
		return ctx, trace.SpanFromContext(ctx)
	}
	return Tracer().Start(ctx, name, trace.WithTimestamp(at), trace.WithAttributes(attrs...))
}

// End ends span with reason: "" or ReasonOK is OK, anything else an error
// status whose description is the reason CODE — never an error's text.
//
// ⚠ Off, Start handed back the CALLER'S span (a no-op one), so End must not
// end it: it does nothing then.
func End(span trace.Span, reason string) {
	if !Enabled() {
		return
	}
	SetReason(span, reason)
	span.End()
}

// SetReason records reason on span (End's status rule) without ending it.
func SetReason(span trace.Span, reason string) {
	if !Enabled() {
		return
	}
	if reason == "" {
		reason = ReasonOK
	}
	span.SetAttributes(attribute.String("muster.reason", reason))
	if reason != ReasonOK && reason != ReasonClientLeft {
		span.SetStatus(codes.Error, reason)
	}
}

// The reasons every span and metric in muster speaks of: a CLOSED vocabulary,
// so a reason can be a metric label (cardinality) and a span status without
// ever carrying an error's prose.
const (
	ReasonOK         = "ok"
	ReasonClientLeft = "client_left"
	// ReasonShutdown: muster itself ended the stream because it is shutting
	// down (a deploy): ErrShuttingDown is the cause. An error status, unlike
	// client_left — a deploy that cuts every view is a disconnect the person
	// sees, not one they chose.
	ReasonShutdown = "shutdown"
	ReasonTimeout  = "timeout"
	ReasonError    = "error"
)

// ErrShuttingDown is the cause muster cancels a live stream with when it shuts
// down. A stream whose context ends with it is reported as ReasonShutdown —
// never as the browser leaving.
var ErrShuttingDown = errors.New("muster is shutting down")

// EndedBy is the reason a stream whose context ended (and whose own error is
// nil or the context's) ended: ReasonShutdown when ctx was cancelled with
// ErrShuttingDown, else ReasonClientLeft.
func EndedBy(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), ErrShuttingDown) {
		return ReasonShutdown
	}
	return ReasonClientLeft
}

// Traceparent is ctx's span context as a W3C traceparent value, "" when ctx
// has no valid span (always, while tracing is off).
func Traceparent(ctx context.Context) string {
	if !Enabled() {
		return ""
	}
	return SpanTraceparent(trace.SpanContextFromContext(ctx))
}

// SpanTraceparent is sc as a W3C traceparent value, "" for an invalid one.
func SpanTraceparent(sc trace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%s", sc.TraceID(), sc.SpanID(), sc.TraceFlags())
}

// WithTraceparent is ctx with tp (a W3C traceparent) as its REMOTE parent; ctx
// unchanged when tp is empty or malformed, or tracing is off.
func WithTraceparent(ctx context.Context, tp string) context.Context {
	if !Enabled() || tp == "" {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": tp})
}

// ValidTraceparent reports whether tp is a well-formed W3C traceparent naming
// a valid trace and span.
func ValidTraceparent(tp string) bool {
	if len(tp) != 55 {
		return false
	}
	sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": tp}))
	return sc.IsValid()
}

// TraceID is ctx's trace id as hex, "" for none: what a log line carries so a
// trace can be found from `kubectl logs` and the other way round.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
