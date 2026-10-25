package telemetry

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// QueryTracer is the pgx tracer that gives every query, and every wait for a
// pooled connection, a span — nil while tracing is off, so the pool is built
// exactly as before (internal/db.Connect).
//
// 🔴 A QUERY IS TRACED ONLY INSIDE A TRACE. A query whose context carries no
// span (a background loop's sweep, the boot ping) starts none: a root span per
// query would bury the opens this exists for under the reconcilers' noise.
//
// 🔴 THE STATEMENT IS NORMALISED BEFORE IT IS RECORDED: every quoted string and
// every number is replaced with `?`. muster's queries take their values as
// parameters, which pgx never shows here — but a statement that inlined one
// would otherwise put it into a span.
func QueryTracer() pgx.QueryTracer {
	if !Enabled() {
		return nil
	}
	return pgxTracer{}
}

type pgxTracer struct{}

// spanKey marks the span THIS tracer started, so its End ends that span and
// never the caller's (TraceQueryEnd gets the context TraceQueryStart returned,
// and for an untraced query that context's span is the CALLER's).
type spanKey struct{}

func (pgxTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	op := operation(data.SQL)
	ctx, span := Tracer().Start(ctx, "db "+op, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		semconv.DBSystemNamePostgreSQL, semconv.DBOperationName(op), semconv.DBQueryText(NormalizeSQL(data.SQL)),
		attribute.Int("db.query.args", len(data.Args))))
	return context.WithValue(ctx, spanKey{}, span)
}

func (pgxTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	span.SetAttributes(attribute.Int64("db.rows", data.CommandTag.RowsAffected()))
	End(span, dbReason(data.Err))
}

func (pgxTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	ctx, span := Tracer().Start(ctx, "db acquire", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(semconv.DBSystemNamePostgreSQL))
	return context.WithValue(ctx, spanKey{}, span)
}

func (pgxTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	span, ok := ctx.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	End(span, dbReason(data.Err))
}

// dbReason is err as a reason: the SQLSTATE for a server error (a code, never
// its message, which can quote a value), else a closed word.
func dbReason(err error) string {
	var pe *pgconn.PgError
	switch {
	case err == nil:
		return ReasonOK
	case errors.Is(err, context.Canceled):
		return ReasonClientLeft
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.Is(err, pgx.ErrNoRows):
		return ReasonOK
	case errors.As(err, &pe):
		return "sqlstate_" + pe.Code
	}
	return ReasonError
}

var (
	sqlStrings = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlNumbers = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	sqlSpace   = regexp.MustCompile(`\s+`)
)

// maxQueryText bounds a recorded statement.
const maxQueryText = 512

// NormalizeSQL is sql as a span records it: literals replaced with `?`,
// whitespace collapsed, cut to maxQueryText bytes.
func NormalizeSQL(sql string) string {
	s := sqlStrings.ReplaceAllString(sql, "?")
	s = sqlNumbers.ReplaceAllString(s, "?")
	s = strings.TrimSpace(sqlSpace.ReplaceAllString(s, " "))
	if len(s) > maxQueryText {
		s = s[:maxQueryText]
	}
	return s
}

// operation is the statement's first keyword, upper-cased ("SELECT"), or
// "QUERY" when it has none a span name can carry.
func operation(sql string) string {
	f := strings.Fields(strings.TrimLeft(sql, "( \t\r\n"))
	if len(f) == 0 {
		return "QUERY"
	}
	op := strings.ToUpper(f[0])
	for _, r := range op {
		if r < 'A' || r > 'Z' {
			return "QUERY"
		}
	}
	return op
}
