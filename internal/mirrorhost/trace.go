package mirrorhost

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// TRACING A REQUEST ON THE HOST (internal/telemetry, docs/telemetry.md).
//
// muster sends each request with the trace context of the server span that
// sent it (mirror.Msg.Traceparent); the host's spans for that request are its
// children, so one trace runs from the browser's open to this host's
// transcript read. While this process's tracing is off — or the server's, so
// the frame carries no context — nothing here does anything: no span, no
// registered open, every mark a no-op on nil. Per-phase aggregates come from
// these spans (a span-metrics generator), not from anything on the wire.
//
// 🔴 A HOST SPAN CARRIES IDS, SIZES, COUNTS AND DURATIONS. Never a session or
// window title, a path, a line of the transcript, pane contents, or an
// error's prose — this host's errors quote paths on it.

// openTrace is one TRACED request's open on this host: its span and the end
// of its last phase (mark), until its first reply is sent (reply).
type openTrace struct {
	ctx  context.Context
	span trace.Span

	mu   sync.Mutex
	last time.Time
	done bool
}

// beginOpen starts the open of request m, named name ("host.watch"), under
// the trace context m carries, and registers it under m's stream id for the
// stream's first reply (viewSend) to find — or, when this host does not trace
// or m names no server span to join, does nothing and returns nil. (A root
// span per open here would be a trace with no server half.)
func (h *hostSide) beginOpen(m mirror.Msg, name string) *openTrace {
	if !telemetry.Enabled() || m.Traceparent == "" {
		return nil
	}
	o := &openTrace{last: time.Now()}
	o.ctx, o.span = telemetry.Start(telemetry.WithTraceparent(context.Background(), m.Traceparent), name, attribute.Int64("mirror.stream", int64(m.ID)),
		attribute.Bool("mirror.window", m.Window != ""), attribute.Bool("mirror.resume", m.Gen != "" && m.Offset > 0))
	h.opens.Store(m.ID, o)
	return o
}

// openOf is stream id's open while it has not replied yet; nil otherwise.
func (h *hostSide) openOf(id uint32) *openTrace {
	v, ok := h.opens.Load(id)
	if !ok {
		return nil
	}
	return v.(*openTrace)
}

// mark ends phase name, a child span from the previous mark (or the request's
// arrival) to now. A mark
// after the reply is ignored — a restart of the view later on is not part of
// its open. Safe on nil.
func (o *openTrace) mark(name string, attrs ...attribute.KeyValue) {
	if o == nil {
		return
	}
	now := time.Now()
	o.mu.Lock()
	if o.done {
		o.mu.Unlock()
		return
	}
	from := o.last
	o.last = now
	o.mu.Unlock()
	_, span := telemetry.StartAt(o.ctx, "host."+name, from, attrs...)
	span.End(trace.WithTimestamp(now))
}

// within is ctx carrying the open's span while the open lasts — so a request
// made for it (opencode's) is its child — and ctx itself otherwise. Safe on
// nil.
func (o *openTrace) within(ctx context.Context) context.Context {
	if o == nil {
		return ctx
	}
	o.mu.Lock()
	done := o.done
	o.mu.Unlock()
	if done {
		return ctx
	}
	return trace.ContextWithSpan(ctx, o.span)
}

// annotate adds attrs to the open's span. Safe on nil.
func (o *openTrace) annotate(attrs ...attribute.KeyValue) {
	if o == nil {
		return
	}
	o.span.SetAttributes(attrs...)
}

// reply ends stream id's open as its FIRST reply is sent: the open is over.
// A no-op for an untraced stream, and after the first.
func (h *hostSide) reply(id uint32) {
	o := h.openOf(id)
	if o == nil {
		return
	}
	h.opens.Delete(id)
	o.mu.Lock()
	already := o.done
	o.done = true
	o.mu.Unlock()
	if !already {
		telemetry.End(o.span, telemetry.ReasonOK)
	}
}

// finish ends stream id's open with err's reason if it never replied (a
// watch refused or failed before its first batch, a terminal that never
// produced output), and forgets it either way.
func (h *hostSide) finish(id uint32, err error) {
	o := h.openOf(id)
	if o == nil {
		return
	}
	h.opens.Delete(id)
	o.mu.Lock()
	already := o.done
	o.done = true
	o.mu.Unlock()
	if !already {
		telemetry.End(o.span, hostReason(err))
	}
}

// hostReason is err as a closed reason code (never its text).
func hostReason(err error) string {
	switch {
	case err == nil:
		return telemetry.ReasonOK
	case errors.Is(err, context.Canceled):
		return telemetry.ReasonClientLeft
	case errors.Is(err, context.DeadlineExceeded):
		return telemetry.ReasonTimeout
	case errors.Is(err, errNotClaudeNow):
		return "not_agent"
	}
	if r := pasteRefusalOf(err); r != "" {
		return "refused_" + r
	}
	return telemetry.ReasonError
}

// traced runs serve for request m under a span named "host.<m.Type>", a child
// of the server span that sent it.
func traced(m mirror.Msg, serve func(mirror.Msg)) {
	if !telemetry.Enabled() || m.Traceparent == "" {
		serve(m)
		return
	}
	ctx := telemetry.WithTraceparent(context.Background(), m.Traceparent)
	_, span := telemetry.Start(ctx, "host."+m.Type, attribute.Int64("mirror.stream", int64(m.ID)))
	defer telemetry.End(span, telemetry.ReasonOK)
	serve(m)
}
