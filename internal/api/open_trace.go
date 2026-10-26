package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
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
// counts it: shutdown when muster ended it for a deploy (terminalSet.endAll's
// cause), client_left when the browser went first, ended for a stream the far
// side ended cleanly, else mirror.Reason.
//
// 🔴 A FAILED WRITE TO THE PAGE'S OWN SOCKET IS THE BROWSER LEAVING
// (errPageGone), whatever the context says yet. When the page closes, the
// socket's reader (CloseRead) answers the close, closes the connection and
// only THEN cancels the context — so a batch written in between fails with
// "use of closed network connection" while ctx.Err() is still nil. Read off
// the error alone, that end was counted as "error": a browser leaving,
// recorded as a fault (measured: ~1 in 150 opens closed right after the first
// batch, with 4 CPUs).
//
// 🔴 BUT A PAGE THAT IS ALIVE AND NOT READING IS A TIMEOUT, NEVER "GONE": a
// write to it that hits terminalWriteTimeout closes the connection, and a
// write that then fails on the closed connection — this one, or the next
// writer's — would read as the page leaving. A stalled page is a prime
// suspect for a "the session disconnected" report, so it must stay visible:
// a write timeout on c, by ANY writer (pageStalled), is "timeout".
func streamEndReason(ctx context.Context, c *websocket.Conn, err error) string {
	switch {
	case errors.Is(context.Cause(ctx), telemetry.ErrShuttingDown):
		return telemetry.ReasonShutdown
	case pageStalled(c), errors.Is(err, context.DeadlineExceeded):
		return telemetry.ReasonTimeout
	case ctx.Err() != nil, errors.Is(err, errPageGone):
		return telemetry.EndedBy(ctx)
	case err == nil:
		return "ended"
	}
	return mirror.Reason(err)
}

// viewEnded counts a view's socket ending, and says why on its span.
//
// The socket's open span ends here with the reason if its open never finished
// (a refusal, a stream that ended before its first batch), and the END is a
// span of its own: a root linked to the open (telemetry.StreamClosed), so a
// view open for hours never grows one trace. It is counted for a refusal
// before the upgrade too (not_found, refused, shutdown, step_up), with the
// reason it was refused for.
func viewEnded(r *http.Request, view, reason string, since time.Time) {
	metrics.ViewDisconnects.WithLabelValues(view, reason).Inc()
	telemetry.OpenDone(r, reason)
	telemetry.StreamClosed(r, reason, time.Since(since))
}

// errPageGone wraps a failed write to the page's own socket (writeToPage,
// pageWriter): the page has gone, and that is why the stream ended. A write
// that TIMED OUT is not marked (markPageGone): that page was there and not
// reading, which is a timeout.
var errPageGone = errors.New("the page's socket is closed")

// markPageGone is err marked errPageGone — unless it is a timeout, returned
// as it is.
func markPageGone(err error) error {
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", errPageGone, err)
}

// writeToPage is terminalWriteJSON to the page's socket, a failure other than
// a timeout marked errPageGone.
func writeToPage(ctx context.Context, c *websocket.Conn, v any) error {
	return markPageGone(terminalWriteJSON(ctx, c, v))
}

// pageWriter is w — the page's socket, as a terminal's output writer — its
// failures marked errPageGone.
type pageWriter struct {
	w interface{ Write([]byte) (int, error) }
}

func (p pageWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	return n, markPageGone(err)
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
