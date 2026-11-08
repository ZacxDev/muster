package agents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// EVERY WAY A RUNTIME CAN FAIL A STREAMED TURN ENDS THE TURN PROMPTLY, AS AN ERROR.
//
// 🔴 THE INCIDENT. A gateway agent whose model provider refused the call (a
// spending limit) was sent a message over the machine door; the runtime logged
// its failure within a second, and muster never answered the request, never
// stored a reply, and left the agent's outcome `running`. The shape that does
// that is a gateway runtime's own, as READ IN ITS SOURCE (one 2026.5 release;
// the build the incident ran was not inspected): its streaming /v1/responses
// handler writes `response.failed` when the agent run throws, and then — when
// the run had already emitted its lifecycle `error` before throwing — never
// writes the completion, `[DONE]` or the end of the body, because its finalizer
// was already "requested" and returns early on the second call. The connection
// stays open. muster read on until EOF, which never came: the turn
// was bounded only by the HTTP client's timeout (10 min), whose error matches
// context.DeadlineExceeded and so was not counted as a failure either.
//
// Each case below is one failure shape, played by a fake runtime over real
// HTTP, through the one transport every door uses (RunToolLoop →
// streamResponses → the SSE reader). A case passes only if the turn RETURNS
// within a short bound AND returns an error carrying the runtime's words where
// it sent any — "returned" alone is not the claim.

// gatewayPrelude is the events that gateway runtime writes before the run
// starts (setSseHeaders → response.created, response.in_progress,
// output_item.added, content_part.added).
func gatewayPrelude() string {
	return sse("response.created", map[string]any{"response": map[string]any{"status": "in_progress", "output": []any{}}}) +
		sse("response.in_progress", map[string]any{"response": map[string]any{"status": "in_progress", "output": []any{}}}) +
		sse("response.output_item.added", map[string]any{"output_index": 0,
			"item": map[string]any{"type": "message", "role": "assistant", "content": []any{}}}) +
		sse("response.content_part.added", map[string]any{"output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": ""}})
}

// providerFailure is the provider's refusal as a runtime would relay it. The
// vendor and figures are synthetic.
const providerFailure = "FailoverError: 403 Key limit exceeded (daily limit)"

func sse(typ string, data map[string]any) string {
	data["type"] = typ
	b, _ := json.Marshal(data)
	return "event: " + typ + "\ndata: " + string(b) + "\n\n"
}

func failedEvent(msg string) string {
	return sse("response.failed", map[string]any{"response": map[string]any{"status": "failed", "output": []any{},
		"error": map[string]any{"code": "api_error", "message": msg}}})
}

// shapeRuntime plays stream, then — when hold — keeps the connection open
// until the test ends, as the incident's gateway did.
func shapeRuntime(t *testing.T, status int, stream string, hold bool) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(stream))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(stream))
		w.(http.Flusher).Flush()
		if hold {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	// LIFO: release the held handlers, then close the server.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

// turnResult runs one RunToolLoop turn against srv and returns what it
// returned, or hung = true when it did not return within bound.
func turnResult(t *testing.T, client *http.Client, srv *httptest.Server, bound time.Duration) (reply string, err error, hung bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type res struct {
		reply string
		err   error
	}
	ch := make(chan res, 1)
	go func() {
		r, e := RunToolLoop(ctx, client, srv.URL+"/v1/responses", "", "k", testRuntimeModel, "", "hi", nil,
			func(string, string) string { return "{}" }, nil)
		ch <- res{r, e}
	}()
	select {
	case r := <-ch:
		return r.reply, r.err, false
	case <-time.After(bound):
		return "", nil, true
	}
}

func TestEveryStreamFailureShapeEndsTheTurnAsAnError(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		stream string
		hold   bool
		// words is what the error must carry ("" = any error).
		words string
	}{
		{"error HTTP status", http.StatusInternalServerError,
			`{"error":{"message":"` + providerFailure + `","type":"api_error"}}`, false, providerFailure},
		{"response.failed then end", http.StatusOK,
			gatewayPrelude() + failedEvent(providerFailure) + "data: [DONE]\n\n", false, providerFailure},
		// 🔴 THE INCIDENT'S SHAPE: response.failed, then the connection stays open.
		{"response.failed then held open", http.StatusOK,
			gatewayPrelude() + failedEvent(providerFailure), true, providerFailure},
		// The same gateway when the run threw BEFORE any lifecycle event: its
		// finalizer runs after response.failed and writes a completion whose
		// status is "failed" and whose text is its placeholder. Read as the
		// answer, the placeholder was stored as the agent's reply.
		{"response.failed then a failed completion", http.StatusOK,
			gatewayPrelude() + failedEvent(providerFailure) +
				sse("response.completed", map[string]any{"response": map[string]any{"status": "failed",
					"output": []any{map[string]any{"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "No response from the runtime."}}}}}}) +
				"data: [DONE]\n\n", false, providerFailure},
		// The run failed without throwing: no response.failed at all, only a
		// completion whose status says failed.
		{"completion with status failed", http.StatusOK,
			gatewayPrelude() + sse("response.completed", map[string]any{"response": map[string]any{"status": "failed",
				"output": []any{map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "No response from the runtime."}}}}}}) +
				"data: [DONE]\n\n", false, ""},
		// The body ends with no terminal event at all (a runtime that died
		// mid-turn): an answer was never given, so it is not an empty one.
		{"no terminal event then end", http.StatusOK,
			gatewayPrelude() + sse("response.output_text.delta", map[string]any{"output_index": 0, "delta": "Start"}), false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := shapeRuntime(t, c.status, c.stream, c.hold)
			reply, err, hung := turnResult(t, srv.Client(), srv, 5*time.Second)
			if hung {
				t.Fatalf("the turn did not return within 5s of the runtime's failure (it hangs)")
			}
			if err == nil {
				t.Fatalf("the turn returned (%q, nil); want an error", reply)
			}
			if c.words != "" && !strings.Contains(err.Error(), c.words) {
				t.Fatalf("error %q; want the runtime's words %q", err, c.words)
			}
		})
	}
}

// TestAnAnswerStillEndsTheTurnAsAnAnswer is the controls for the shapes above:
// a completion that answers is the answer even when the connection stays open
// after it (the reader stops at the terminal event, not at EOF), and an
// explicit [DONE] with no completion is still the empty answer it always was.
func TestAnAnswerStillEndsTheTurnAsAnAnswer(t *testing.T) {
	completed := sse("response.completed", map[string]any{"response": map[string]any{"status": "completed",
		"output": []any{map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "the answer"}}}}}})
	srv := shapeRuntime(t, http.StatusOK, gatewayPrelude()+completed, true)
	reply, err, hung := turnResult(t, srv.Client(), srv, 5*time.Second)
	if hung || err != nil || reply != "the answer" {
		t.Fatalf("a completion then an open connection = (%q, %v, hung %v); want the answer, promptly", reply, err, hung)
	}
	srv = shapeRuntime(t, http.StatusOK, gatewayPrelude()+"data: [DONE]\n\n", false)
	reply, err, hung = turnResult(t, srv.Client(), srv, 5*time.Second)
	if hung || err != nil || reply != "" {
		t.Fatalf("[DONE] with no completion = (%q, %v, hung %v); want the empty answer", reply, err, hung)
	}
}

// TestABareErrorThenSilenceEndsAtTheClientTimeoutWithTheRuntimesWords: a bare
// `error` is not terminal (a fallback may still complete), so the reader reads
// on after it; a runtime that then holds the connection open silently is
// bounded by the HTTP client's Timeout, and the turn ends as the runtime's OWN
// error — its words, and NOT context.DeadlineExceeded, which the outcome
// tracker does not count.
func TestABareErrorThenSilenceEndsAtTheClientTimeoutWithTheRuntimesWords(t *testing.T) {
	srv := shapeRuntime(t, http.StatusOK,
		gatewayPrelude()+sse("error", map[string]any{"code": "api_error", "message": providerFailure}), true)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	_, err, hung := turnResult(t, client, srv, 5*time.Second)
	if hung {
		t.Fatal("a bare error then silence outlived the client's timeout")
	}
	if err == nil {
		t.Fatal("a bare error then silence until the client timeout returned no error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %q matches context.DeadlineExceeded, which the outcome tracker does not count", err)
	}
	var se *StreamError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), providerFailure) {
		t.Fatalf("error %q; want the runtime's StreamError with its words %q", err, providerFailure)
	}
}

// TestABareErrorAPauseThenACompletionIsTheAnswer: a bare `error`, a pause (a
// fallback model starting), then a completion that answers — over real HTTP,
// within the client's timeout — is the answer, not a failure.
func TestABareErrorAPauseThenACompletionIsTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gatewayPrelude() + sse("error", map[string]any{"code": "api_error", "message": providerFailure})))
		w.(http.Flusher).Flush()
		select {
		case <-time.After(400 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(sse("response.completed", map[string]any{"response": map[string]any{"status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "answered on the fallback"}}}}}})))
	}))
	t.Cleanup(srv.Close)
	reply, err, hung := turnResult(t, &http.Client{Timeout: 5 * time.Second}, srv, 10*time.Second)
	if hung || err != nil || reply != "answered on the fallback" {
		t.Fatalf("error, pause, completion = (%q, %v, hung %v); want the completion's answer", reply, err, hung)
	}
}

// TestAStreamThatGoesSilentIsBoundedByTheRequestTimeoutAndCountsAsAFailure:
// a stream with no failure event that simply stops sending (no terminal event,
// connection open) is bounded by the HTTP client's Timeout — there is no
// separate idle guard, because a claude-code runtime (ccd) sends NOTHING until
// its turn is over, so silence alone is not evidence of a dead turn. What this
// pins is that the timeout's error does NOT match context.DeadlineExceeded,
// which the outcome tracker reads as "the caller gave up" and does not count:
// matched, a timed-out turn left the agent `running` for ever.
func TestAStreamThatGoesSilentIsBoundedByTheRequestTimeoutAndCountsAsAFailure(t *testing.T) {
	srv := shapeRuntime(t, http.StatusOK, gatewayPrelude(), true)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	_, err, hung := turnResult(t, client, srv, 5*time.Second)
	if hung {
		t.Fatal("a silent stream outlived the client's timeout")
	}
	if err == nil {
		t.Fatal("a silent stream that timed out returned no error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the request timeout's error %q matches context.DeadlineExceeded, which the outcome tracker "+
			"does not count as a failure", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q; want it to say the runtime's request timed out", err)
	}
}
