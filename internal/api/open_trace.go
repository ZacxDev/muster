package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// ---------------------------------------------------------------------------
// OPENING A SESSION, MEASURED (docs/telemetry.md).
//
// An open is two requests on the server: the page's DOCUMENT, and the view's
// STREAM (a rendered view's transcript socket, a terminal's socket). Each is
// cut into named phases here, and each phase is three things at once:
//
//   - a child span of the request's server span (internal/telemetry; nothing
//     while tracing is off), so a trace shows which phase was slow;
//   - a duration in the request's ONE log line,
//     `open view=… part=… outcome=… trace_id=… total_ms=… <phase>_ms=…`,
//     so `kubectl logs` alone answers "which phase was slow" — and its
//     trace_id finds the trace;
//   - for the document, a `Server-Timing` entry the page's own open span
//     reads back (ui's musterOpen), so the browser's span carries the
//     server's phases too.
//
// The aggregate view is metrics.OpenPhase (the part's total, by view) beside
// mirror's hub and host histograms.
//
// 🔴 THE LOG LINE NAMES THE HOST, SESSION AND WINDOW (as every other mirror
// log line does); THE METRICS AND SPANS DO NOT. A label is a closed
// vocabulary, and a span carries ids and durations only.
// ---------------------------------------------------------------------------

// The views an open is measured for (metrics.OpenPhase's `view`).
const (
	openViewMirror   = "mirror_view"
	openViewTerminal = "mirror_terminal"
	openViewAgent    = "agent"
)

// The parts of an open, and the phase each part's total is observed as.
const (
	openPartDocument = "document"
	openPartStream   = "stream"
)

type openPhase struct {
	name string
	dur  time.Duration
}

// openPhases is one request's phases.
type openPhases struct {
	s          *Server
	ctx        context.Context
	view, part string
	start      time.Time
	cur        string
	curStart   time.Time
	curSpan    trace.Span
	phases     []openPhase
	fields     []string
	finished   bool
}

// openPhases starts measuring request r as part of an open of view. fields
// are key, value pairs for the log line only.
func (s *Server) openPhases(r *http.Request, view, part string, fields ...string) *openPhases {
	return &openPhases{s: s, ctx: r.Context(), view: view, part: part, start: time.Now(), fields: fields}
}

// begin ends the current phase and starts phase name, returning the context
// its work runs under (its span's).
func (o *openPhases) begin(name string) context.Context {
	o.endPhase()
	o.cur, o.curStart = name, time.Now()
	ctx, span := telemetry.Start(o.ctx, "open."+name)
	o.curSpan = span
	return ctx
}

// endPhase ends the current phase, if any.
func (o *openPhases) endPhase() {
	if o.cur == "" {
		return
	}
	o.phases = append(o.phases, openPhase{o.cur, time.Since(o.curStart)})
	telemetry.End(o.curSpan, telemetry.ReasonOK)
	o.cur, o.curSpan = "", nil
}

// set adds a key, value pair to the log line.
func (o *openPhases) set(kv ...string) { o.fields = append(o.fields, kv...) }

// serverTiming writes the phases so far as Server-Timing entries on w
// (before its body): what the page's open span reads back.
func (o *openPhases) serverTiming(w http.ResponseWriter) {
	o.endPhase()
	var b strings.Builder
	for i, p := range o.phases {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s;dur=%.1f", p.name, float64(p.dur.Microseconds())/1000)
	}
	fmt.Fprintf(&b, "%stotal;dur=%.1f", map[bool]string{true: ", ", false: ""}[len(o.phases) > 0], float64(time.Since(o.start).Microseconds())/1000)
	w.Header().Add("Server-Timing", b.String())
}

// finish ends the open's measurement with outcome (a closed reason): its log
// line, and its total observed under the part's phase name. Only the first
// call counts.
func (o *openPhases) finish(outcome string, extra ...string) {
	if o.finished {
		return
	}
	o.finished = true
	o.endPhase()
	total := time.Since(o.start)
	phase := o.part
	if o.part == openPartStream {
		phase = map[string]string{openViewTerminal: "first_output"}[o.view]
		if phase == "" {
			phase = "first_batch"
		}
	}
	if outcome == telemetry.ReasonOK {
		metrics.OpenPhase.WithLabelValues(o.view, phase).Observe(total.Seconds())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "open view=%s part=%s outcome=%s trace_id=%s total_ms=%s", o.view, o.part, outcome, orDash(telemetry.TraceID(o.ctx)), ms(total))
	for _, p := range o.phases {
		fmt.Fprintf(&b, " %s_ms=%s", p.name, ms(p.dur))
	}
	kv := append(append([]string(nil), extra...), o.fields...)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %s=%s", kv[i], logValue(kv[i+1]))
	}
	o.s.logger.Print(b.String())
}

// hubFields is what the hub measured of a stream's open (mirror.OpenStats) as
// log fields: the round trip to the host. What the host spent inside it is
// its spans' (host.*).
func hubFields(st *mirror.OpenStats) []string {
	if st == nil || st.Sent.IsZero() || st.First.IsZero() {
		return nil
	}
	return []string{"hub_rtt_ms", ms(st.First.Sub(st.Sent))}
}

// streamEndReason is why a view's socket ended, as metrics.ViewDisconnects
// counts it: client_left when the browser (or a shutdown) went first, ended
// for a stream the far side ended cleanly, else mirror.Reason.
func streamEndReason(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return telemetry.ReasonClientLeft
	case err == nil:
		return "ended"
	}
	return mirror.Reason(err)
}

// viewEnded counts a view's socket ending, and says why on its span.
func viewEnded(r *http.Request, view, reason string) {
	metrics.ViewDisconnects.WithLabelValues(view, reason).Inc()
	telemetry.Annotate(r, attribute.String("muster.close_reason", reason))
}

// firstWriter calls first once, before its first write.
type firstWriter struct {
	w     interface{ Write([]byte) (int, error) }
	first func()
	done  bool
}

func (f *firstWriter) Write(p []byte) (int, error) {
	if !f.done {
		f.done = true
		f.first()
	}
	return f.w.Write(p)
}

func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 1, 64)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// logValue is v as one logfmt value: quoted when it holds a space, a quote or
// an equals sign, and never a control character (a session name is the
// host's, and must not start a forged line of its own in this log).
func logValue(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, " \"=") || strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return strconv.Quote(v)
	}
	return v
}
