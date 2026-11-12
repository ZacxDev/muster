package agents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// completedWith is a /v1/responses SSE body whose response.completed carries
// usage (a raw JSON object, "" for none).
func completedWith(usage string) string {
	resp := `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]`
	if usage != "" {
		resp += `,"usage":` + usage
	}
	resp += "}"
	return "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + resp + "}\n\n"
}

// TestATurnsUsageIsSummedOverItsRequestsInBothShapes: a turn's sink adds every
// request's reported usage — the Anthropic split ccd sends taken as it is, a
// Responses gateway's input_tokens_details.cached_tokens moved out of input
// into cache read — and a request that reports none adds nothing. Literal
// fixture numbers, literal sums.
func TestATurnsUsageIsSummedOverItsRequestsInBothShapes(t *testing.T) {
	bodies := []string{
		completedWith(`{"input_tokens":1200,"output_tokens":340,"cache_read_input_tokens":50000,"cache_creation_input_tokens":800}`),
		completedWith(`{"input_tokens":9000,"output_tokens":120,"input_tokens_details":{"cached_tokens":6000}}`),
		completedWith(""),
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, bodies[n])
		n++
	}))
	t.Cleanup(srv.Close)

	// No sink: the request still answers, and nothing is kept.
	if _, err := streamResponses(context.Background(), srv.Client(), srv.URL+"/v1/responses", "", "", responsesRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	n = 0
	ctx, sink := WithTurnUsage(context.Background())
	if _, _, _, _, known := sink.Snapshot(); known {
		t.Fatal("an empty sink reports known usage")
	}
	for range bodies {
		out, err := streamResponses(ctx, srv.Client(), srv.URL+"/v1/responses", "", "", responsesRequest{}, nil)
		if err != nil || out.messageText() != "ok" {
			t.Fatalf("request: %v %+v", err, out)
		}
	}
	in, out, read, create, known := sink.Snapshot()
	if in != 4200 || out != 460 || read != 56000 || create != 800 || !known {
		t.Fatalf("the turn's usage = in %d out %d read %d create %d known %v; want 4200 460 56000 800 true", in, out, read, create, known)
	}
	// The context fill is the LAST request that reported usage — 3000 + 6000
	// cached = 9000 — not the sum (60,200 + 800) of every request.
	if fill, ok := sink.Context(); !ok || fill != 9000 {
		t.Fatalf("the turn's context fill = %d (%v), want the last request's 9000", fill, ok)
	}

	// A turn whose only response carried none: unknown, zero.
	n = 2
	ctx, sink = WithTurnUsage(context.Background())
	if _, err := streamResponses(ctx, srv.Client(), srv.URL+"/v1/responses", "", "", responsesRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if in, out, read, create, known := sink.Snapshot(); known || in+out+read+create != 0 {
		t.Fatalf("a turn with no usage reported: %d %d %d %d %v", in, out, read, create, known)
	}
}

// TestUsageSplitClampsANonsenseCachedCount: a gateway that reports more cached
// tokens than input never yields negative input.
func TestUsageSplitClampsANonsenseCachedCount(t *testing.T) {
	u := responsesUsage{InputTokens: 100, OutputTokens: 5}
	u.InputTokensDetails = &struct {
		CachedTokens int64 `json:"cached_tokens"`
	}{CachedTokens: 400}
	if in, out, read, create := u.split(); in != 0 || out != 5 || read != 100 || create != 0 {
		t.Fatalf("split = %d %d %d %d; want 0 5 100 0", in, out, read, create)
	}
}

// TestCcdsLastCallFillIsTheContext: ccd's one response sums a whole turn and
// names its last call's fill in context_tokens; that, not the sum, is the
// context.
func TestCcdsLastCallFillIsTheContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completedWith(`{"input_tokens":6,"output_tokens":946,"cache_read_input_tokens":493988,"cache_creation_input_tokens":4751,"context_tokens":249972}`))
	}))
	t.Cleanup(srv.Close)
	ctx, sink := WithTurnUsage(context.Background())
	if _, err := streamResponses(ctx, srv.Client(), srv.URL+"/v1/responses", "", "", responsesRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if fill, ok := sink.Context(); !ok || fill != 249972 {
		t.Fatalf("context = %d (%v), want 249972", fill, ok)
	}
}
