package agents

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// incidentFailure is the runtime's own sentence for a model it does not know,
// in the shape the incident produced (a display name read as a model of the
// runtime's default provider). The vendor is synthetic.
const incidentFailure = "FailoverError: Unknown model: openai/Acme v4.1 flash"

// TestARuntimeReportedFailureIsAnErrorCarryingItsWords: a stream that reports
// the turn FAILED — `response.failed` with response.error, or a bare `error`
// event, nested or flat — is an error carrying the runtime's sentence, never an
// empty answer.
//
// 🔴 RED BEFORE THIS CHANGE: the parser read only response.completed, so each
// of these assembled to "" with a nil error and the runtime's sentence was lost
// (the kickoff recorded "empty reply"; a chat turn stored nothing).
// CONTROL: a completed stream with the same sentence as TEXT is not an error.
func TestARuntimeReportedFailureIsAnErrorCarryingItsWords(t *testing.T) {
	for _, c := range []struct{ name, stream string }{
		{"response.failed", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"," +
			"\"error\":{\"code\":\"server_error\",\"message\":\"" + incidentFailure + "\"}}}\n\n"},
		{"error, flat", "event: error\ndata: {\"type\":\"error\",\"code\":\"model_not_found\",\"message\":\"" + incidentFailure + "\"}\n\n"},
		{"error, nested", "data: {\"type\":\"error\",\"error\":{\"message\":\"" + incidentFailure + "\"}}\n\n"},
		{"error, by event name only", "event: error\ndata: {\"message\":\"" + incidentFailure + "\"}\n\n"},
		{"failed after a delta", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Start\",\"output_index\":0}\n\n" +
			"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"" + incidentFailure + "\"}}}\n\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, err := parseResponsesSSE(strings.NewReader(c.stream), nil)
			var sf *StreamError
			if !errors.As(err, &sf) {
				t.Fatalf("err = %v (resp %+v); want a *StreamError", err, resp)
			}
			if !strings.Contains(err.Error(), incidentFailure) {
				t.Errorf("error %q does not carry the runtime's sentence", err)
			}
		})
	}
	ok := "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":" +
		"[{\"type\":\"output_text\",\"text\":\"" + incidentFailure + "\"}]}]}}\n\n"
	resp, err := parseResponsesSSE(strings.NewReader(ok), nil)
	if err != nil || resp.messageText() != incidentFailure {
		t.Fatalf("control: a completed stream = (%q, %v); want its text and no error", resp.messageText(), err)
	}
}

// TestATurnsStreamReadsAreActivity: a turn whose context carries
// WithTurnActivity runs the callback for the stream's reads — the evidence of
// life the outcome watchdog reads for a turn whose caller passes no stream
// callback (the kickoff) — and a turn whose context carries none is untouched.
// Both transports: /v1/responses and the chat-completions fallback.
func TestATurnsStreamReadsAreActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			for i := 0; i < 3; i++ {
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
				fl.Flush()
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\",\"output_index\":0}\n\n"))
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"," +
			"\"content\":[{\"type\":\"output_text\",\"text\":\"xxx\"}]}]}}\n\n"))
	}))
	defer srv.Close()
	var n atomic.Int64
	ctx := WithTurnActivity(context.Background(), func() { n.Add(1) })
	if _, err := RunToollessTurn(ctx, srv.Client(), srv.URL+"/v1/responses", "", "k", "m", "", "hi", nil); err != nil {
		t.Fatal(err)
	}
	if n.Load() == 0 {
		t.Fatal("a /v1/responses turn's reads marked no activity")
	}
	before := n.Load()
	if _, err := ChatStream(ctx, srv.Client(), srv.URL+"/v1/chat/completions", "", "k", "m",
		[]ChatMessageIn{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	if n.Load() == before {
		t.Fatal("a chat-completions turn's reads marked no activity")
	}
	// Control: no callback on the context, no count (and nothing panics).
	before = n.Load()
	if _, err := RunToollessTurn(context.Background(), srv.Client(), srv.URL+"/v1/responses", "", "k", "m", "", "hi", nil); err != nil {
		t.Fatal(err)
	}
	if n.Load() != before {
		t.Fatal("a turn with no activity callback moved the count")
	}
}

// TestAnErrorFollowedByACompletionIsNotAFailure (round 1, 🟡3): a runtime may
// report an `error` event mid-stream (a failover attempt) and then complete the
// turn on its fallback. The completion is the answer: no error, its text. Only
// a stream with NO completion after the error fails.
func TestAnErrorFollowedByACompletionIsNotAFailure(t *testing.T) {
	stream := "data: {\"type\":\"error\",\"message\":\"" + incidentFailure + "\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":" +
		"[{\"type\":\"output_text\",\"text\":\"answered on the fallback\"}]}]}}\n\n"
	resp, err := parseResponsesSSE(strings.NewReader(stream), nil)
	if err != nil || resp.messageText() != "answered on the fallback" {
		t.Fatalf("error then completed = (%q, %v); want the completion's text and no error", resp.messageText(), err)
	}
	if _, err := parseResponsesSSE(strings.NewReader("data: {\"type\":\"error\",\"message\":\"x\"}\n\n"), nil); err == nil {
		t.Fatal("control: an error with no completion after it did not fail")
	}
}
