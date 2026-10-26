package telemetry

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestAStatementIsNormalisedOnlyForAKeptSpan: a query under a sampled-OUT
// request (a span that records nothing) never pays for the three regexp
// passes; one under a kept request does — the control that the counter sees
// a call at all.
func TestAStatementIsNormalisedOnlyForAKeptSpan(t *testing.T) {
	calls := 0
	normalize = func(s string) string { calls++; return NormalizeSQL(s) }
	t.Cleanup(func() { normalize = NormalizeSQL })
	exp := tracetest.NewInMemoryExporter()
	tp := NewProvider(exp, 0, true)
	restore := Use(tp)
	t.Cleanup(restore)
	qt := QueryTracer()
	tr := tp.Tracer("t")

	// A request the sampler drops: a poll's root at ratio 0.
	dropped, _ := tr.Start(context.Background(), "poll", trace.WithSpanKind(trace.SpanKindServer))
	end := qt.TraceQueryStart(dropped, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	qt.TraceQueryEnd(end, nil, pgx.TraceQueryEndData{})
	if calls != 0 {
		t.Fatalf("a query under a dropped span was normalised %d time(s)", calls)
	}
	// A request the sampler keeps: one of the open routes.
	kept, span := tr.Start(context.Background(), "GET", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrMethodPath("GET", "/mirror/view")...))
	end = qt.TraceQueryStart(kept, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	qt.TraceQueryEnd(end, nil, pgx.TraceQueryEndData{})
	span.End()
	if calls != 1 {
		t.Fatalf("CONTROL: a query under a kept span was normalised %d time(s), want 1", calls)
	}
}

// TestNormalizeSQLCutsOnARuneBoundary: a statement longer than the bound whose
// cut falls inside a multi-byte character is cut BEFORE it, never through it.
func TestNormalizeSQLCutsOnARuneBoundary(t *testing.T) {
	for _, pad := range []int{maxQueryText - 1, maxQueryText - 2, maxQueryText - 3} {
		// "é" (2 bytes) and "€" (3 bytes) straddling the bound.
		for _, r := range []string{"é", "€"} {
			sql := "SELECT " + strings.Repeat("x", pad-len("SELECT ")) + strings.Repeat(r, 10)
			got := NormalizeSQL(sql)
			if !utf8.ValidString(got) {
				t.Errorf("pad %d, %q: the cut left invalid UTF-8 (%d bytes, ends %q)", pad, r, len(got), got[len(got)-3:])
			}
			if len(got) > maxQueryText || len(got) < maxQueryText-3 {
				t.Errorf("pad %d, %q: %d bytes, want within 3 of %d", pad, r, len(got), maxQueryText)
			}
		}
	}
}

func attrMethodPath(method, path string) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("http.request.method", method), attribute.String("url.path", path)}
}
