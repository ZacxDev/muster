package mirror

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// TRACING THE TUNNEL (internal/telemetry, docs/telemetry.md).
//
// Every request muster sends a host carries the trace context of the span
// that sent it (Msg.Traceparent, filled in by writeJSON), so the host's spans
// for that request join the server's trace. Each request gets a span here
// whose end is the host's answer, and the same round trip is a histogram
// (metrics.MirrorHubRoundTrip) whose `reason` is Reason's closed vocabulary.
//
// 🔴 NO ERROR TEXT REACHES A SPAN OR A LABEL. A host's refusal is its prose —
// it can quote a path on the host — so only Reason(err) is recorded.

// ErrHostEnded is the cause of a stream the HOST ended with an error of its
// own (Msg.Error on closed): the transcript could not be read, the pane
// stopped running Claude Code, tmux refused the attach.
var ErrHostEnded = errors.New("mirror: the host ended the terminal")

// Reason is err as a closed reason code: what a span's status, a metric's
// label and the page's "disconnected because" all say. nil is ok.
func Reason(err error) string {
	switch {
	case err == nil:
		return telemetry.ReasonOK
	case errors.Is(err, context.Canceled):
		return telemetry.ReasonClientLeft
	case errors.Is(err, context.DeadlineExceeded):
		return telemetry.ReasonTimeout
	case errors.Is(err, ErrHostDisconnected):
		return "host_disconnected"
	case errors.Is(err, ErrHostOffline):
		return "host_offline"
	case errors.Is(err, ErrSessionGone):
		return "session_gone"
	case errors.Is(err, ErrWindowGone):
		return "window_gone"
	case errors.Is(err, ErrNotClaude):
		return "not_agent"
	case errors.Is(err, ErrNoRenderedView):
		return "no_rendered_view"
	case errors.Is(err, ErrTooManyWatches), errors.Is(err, ErrTooManyStreams):
		return "too_many"
	case errors.Is(err, ErrOverrun), errors.Is(err, ErrViewOverrun):
		return "overrun"
	case errors.Is(err, ErrInteractiveRefused):
		return "read_only"
	case errors.Is(err, ErrStepUpRequired):
		return "step_up"
	case errors.Is(err, ErrNoPrincipal):
		return "no_principal"
	case errors.Is(err, ErrPasteUnconfirmed):
		return "unconfirmed"
	case errors.Is(err, ErrPasteTimeout), errors.Is(err, ErrCatalogTimeout), errors.Is(err, ErrScrollTimeout),
		errors.Is(err, ErrSubagentTailTimeout), errors.Is(err, ErrAskTimeout):
		return telemetry.ReasonTimeout
	case errors.Is(err, ErrHostEnded):
		return "host_ended"
	case errors.Is(err, ErrSuperseded):
		return "superseded"
	case errors.Is(err, ErrPasteRefused), errors.Is(err, ErrScrollRefused), errors.Is(err, ErrCatalogRefused),
		errors.Is(err, ErrSubagentTailRefused), errors.Is(err, ErrAskRefused), errors.Is(err, ErrScrollPaneInMode):
		return "refused"
	case errors.Is(err, ErrCatalogUnsupported), errors.Is(err, ErrScrollUnsupported), errors.Is(err, ErrSubagentsUnsupported),
		errors.Is(err, ErrAskUnsupported), errors.Is(err, ErrAskCancelUnsupported), errors.Is(err, ErrPanePasteUnsupported):
		return "unsupported"
	}
	return telemetry.ReasonError
}

// Reasons is Reason's whole vocabulary: a test pins that Reason never says
// anything else, so a reason can be a metric label.
var Reasons = []string{telemetry.ReasonOK, telemetry.ReasonClientLeft, telemetry.ReasonTimeout, telemetry.ReasonError,
	"host_disconnected", "host_offline", "session_gone", "window_gone", "not_agent", "no_rendered_view", "too_many",
	"overrun", "read_only", "step_up", "no_principal", "unconfirmed", "host_ended", "superseded", "refused", "unsupported"}

// hubOp starts the span of ONE request to a host, op naming it
// ("mirror.<op>"), and returns the function that ends it with the request's
// outcome and observes its round trip. ctx carries the span, so the frame
// writeJSON writes under it names it as the host's parent.
func hubOp(ctx context.Context, op string) (context.Context, func(error)) {
	start := time.Now()
	ctx, span := telemetry.Start(ctx, "mirror."+op, attribute.String("mirror.op", op))
	return ctx, func(err error) {
		reason := Reason(err)
		metrics.MirrorHubRoundTrip.WithLabelValues(op, reason).Observe(time.Since(start).Seconds())
		telemetry.End(span, reason)
	}
}

// OpenStats is what the hub measured opening one watch or terminal, on
// muster's own clock: the caller's log line reads it (WithOpenStats). What the
// host spent inside that round trip is its own spans' (host.*), not a field
// here.
type OpenStats struct {
	// Sent is when the request frame was written to the host; First when the
	// first batch (watch) or the first output (terminal) was read from it.
	Sent, First time.Time
}

type openStatsKey struct{}

// WithOpenStats has the watch or terminal opened under ctx fill st, BEFORE it
// hands its first batch or output on — so the caller reads it there.
func WithOpenStats(ctx context.Context, st *OpenStats) context.Context {
	return context.WithValue(ctx, openStatsKey{}, st)
}

func openStatsOf(ctx context.Context) *OpenStats {
	st, _ := ctx.Value(openStatsKey{}).(*OpenStats)
	return st
}

// firstAnswer records the first answer of an open: its round trip, in st and
// the histogram, and the end of its span.
func firstAnswer(span trace.Span, op string, st *OpenStats, sent time.Time) {
	now := time.Now()
	metrics.MirrorHubRoundTrip.WithLabelValues(op, telemetry.ReasonOK).Observe(now.Sub(sent).Seconds())
	if st != nil {
		st.Sent, st.First = sent, now
	}
	telemetry.End(span, telemetry.ReasonOK)
}

// requestCarriesTrace is whether a frame of type typ is a REQUEST a host
// serves under its own spans (and so carries Msg.Traceparent). A close, a
// resize and the welcome are not.
func requestCarriesTrace(typ string) bool {
	switch typ {
	case TypeClose, TypeResize, TypeWelcome:
		return false
	}
	return true
}

// streamReason is why a watch or a terminal ended: err's Reason, or
// client_left when it ended without one because the caller's context did (the
// browser went away) — the stream functions return nil for that.
func streamReason(ctx context.Context, err error) string {
	if err == nil && ctx.Err() != nil {
		return telemetry.ReasonClientLeft
	}
	return Reason(err)
}
