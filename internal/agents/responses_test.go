package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// testRuntimeModel is the value these tests put in the request's `model` field.
// It is a FIXTURE, not the runtime's sentinel: the test server answers whatever
// it is asked, so nothing here can establish what a real runtime requires. See
// the carve note at the foot of responses.go.
const testRuntimeModel = "test-runtime-model"

// sseLines builds a single SSE byte stream from event/data pairs. Each event is
// "event: <name>\ndata: <json>\n\n"; a trailing "data: [DONE]" terminates it.
func sseLines(pairs ...[2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString("event: ")
		b.WriteString(p[0])
		b.WriteString("\ndata: ")
		b.WriteString(p[1])
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestParseResponsesSSE feeds a synthetic SSE stream — text deltas, a reasoning
// delta, plus a response.completed carrying a message + a function_call — and
// asserts the emitted events and the assembled *responsesResponse.
func TestParseResponsesSSE(t *testing.T) {
	stream := sseLines(
		[2]string{"response.created", `{"type":"response.created"}`},
		[2]string{"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"let me think"}`},
		[2]string{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hel"}`},
		[2]string{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo"}`},
		[2]string{"response.completed", `{"type":"response.completed","response":{"output":[` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},` +
			`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"do_thing","arguments":"{\"x\":1}"}` +
			`]}}`},
	)

	var events []StreamEvent
	emit := func(ev StreamEvent) { events = append(events, ev) }

	resp, err := parseResponsesSSE(strings.NewReader(stream), emit)
	if err != nil {
		t.Fatalf("parseResponsesSSE: %v", err)
	}

	// Text deltas emitted in order.
	var gotText []string
	var thinking []string
	for _, ev := range events {
		switch ev.Kind {
		case "text":
			gotText = append(gotText, ev.Text)
		case "thinking":
			thinking = append(thinking, ev.Text)
		}
	}
	if strings.Join(gotText, "") != "Hello" {
		t.Errorf("text deltas = %q, want concatenation %q", gotText, "Hello")
	}
	if len(gotText) != 2 || gotText[0] != "Hel" || gotText[1] != "lo" {
		t.Errorf("text deltas not in order: %q", gotText)
	}
	if len(thinking) != 1 || thinking[0] != "let me think" {
		t.Errorf("thinking = %q, want one %q", thinking, "let me think")
	}

	// Assembled response is authoritative: messageText + functionCalls work.
	if got := resp.messageText(); got != "Hello" {
		t.Errorf("messageText = %q, want %q", got, "Hello")
	}
	calls := resp.functionCalls()
	if len(calls) != 1 {
		t.Fatalf("functionCalls = %d, want 1", len(calls))
	}
	if calls[0].Name != "do_thing" || calls[0].Arguments != `{"x":1}` || callID(calls[0]) != "call_1" {
		t.Errorf("function_call = %+v", calls[0])
	}
}

// TestParseResponsesSSE_ReasoningOutputItem asserts a reasoning output item
// (no reasoning-named event) is surfaced as thinking.
func TestParseResponsesSSE_ReasoningOutputItem(t *testing.T) {
	stream := sseLines(
		[2]string{"response.output_item.added", `{"type":"response.output_item.added","item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"hmm"}]}}`},
		[2]string{"response.completed", `{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`},
	)
	var thinking []string
	_, err := parseResponsesSSE(strings.NewReader(stream), func(ev StreamEvent) {
		if ev.Kind == "thinking" {
			thinking = append(thinking, ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(thinking) != 1 || thinking[0] != "hmm" {
		t.Errorf("thinking = %q, want one %q", thinking, "hmm")
	}
}

// TestParseResponsesSSE_MultiItemSeparated reproduces the real gateway shape when
// a turn streams SEVERAL message items (the model narrates around its native
// in-pod tools): each item carries an increasing output_index, its deltas carry
// that index, and item boundaries are marked by output_item.added/done. The
// parser must inject a blank-line separator between the segments so they don't
// fuse ("...the configGood..."), both in the LIVE emit stream and the assembled
// messageText. Mirrors the events captured from a live agent runtime.
func TestParseResponsesSSE_MultiItemSeparated(t *testing.T) {
	stream := sseLines(
		[2]string{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m0"}}`},
		[2]string{"response.content_part.added", `{"type":"response.content_part.added","output_index":0}`},
		[2]string{"response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"delta":"before the config"}`},
		[2]string{"response.output_text.done", `{"type":"response.output_text.done","output_index":0}`},
		[2]string{"response.output_item.done", `{"type":"response.output_item.done","output_index":0}`},
		[2]string{"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"m1"}}`},
		[2]string{"response.content_part.added", `{"type":"response.content_part.added","output_index":1}`},
		[2]string{"response.output_text.delta", `{"type":"response.output_text.delta","output_index":1,"delta":"Good, all done"}`},
		[2]string{"response.output_item.done", `{"type":"response.output_item.done","output_index":1}`},
		[2]string{"response.completed", `{"type":"response.completed","response":{"output":[` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before the config"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Good, all done"}]}` +
			`]}}`},
	)

	var text strings.Builder
	resp, err := parseResponsesSSE(strings.NewReader(stream), func(ev StreamEvent) {
		if ev.Kind == "text" {
			text.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	const want = "before the config\n\nGood, all done"
	// Live emit stream carries the separator (no word-fusing).
	if got := text.String(); got != want {
		t.Errorf("live text = %q, want %q (segments must not fuse)", got, want)
	}
	if strings.HasPrefix(text.String(), "\n") {
		t.Errorf("first segment must not get a leading break: %q", text.String())
	}
	// The assembled (authoritative) text is separated identically.
	if got := resp.messageText(); got != want {
		t.Errorf("messageText = %q, want %q", got, want)
	}
	// The PartCollector observing the same stream persists ONE text part that
	// preserves the separator — the transcript reload matches the live view.
	pc := &PartCollector{}
	_, _ = parseResponsesSSE(strings.NewReader(stream), pc.Observe)
	parts := pc.Parts()
	if len(parts) != 1 || parts[0].Kind != "text" || parts[0].Content != want {
		t.Errorf("collected parts = %+v, want one text part %q", parts, want)
	}
}

// TestMessageTextJoinsSegments asserts messageText inserts a blank line between
// distinct message items but leaves a single-item turn untouched, and that an
// empty segment neither appears nor forces a stray separator.
func TestMessageTextJoinsSegments(t *testing.T) {
	single := &responsesResponse{Output: []responsesOutputItem{
		{Type: "message", Content: []responsesMessageContentPart{{Type: "output_text", Text: "just one"}}},
	}}
	if got := single.messageText(); got != "just one" {
		t.Errorf("single-item messageText = %q, want %q", got, "just one")
	}
	multi := &responsesResponse{Output: []responsesOutputItem{
		{Type: "message", Content: []responsesMessageContentPart{{Type: "output_text", Text: "alpha"}}},
		{Type: "message", Content: []responsesMessageContentPart{{Type: "output_text", Text: ""}}},
		{Type: "message", Content: []responsesMessageContentPart{{Type: "output_text", Text: "beta"}}},
	}}
	if got := multi.messageText(); got != "alpha\n\nbeta" {
		t.Errorf("multi-item messageText = %q, want %q (empty segment must not add a separator)", got, "alpha\n\nbeta")
	}
}

// TestJoinSegments covers the separator helper directly: only adjacent non-empty
// segments are separated.
func TestJoinSegments(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a\n\nb"},
		{[]string{"", "a", "", "b", ""}, "a\n\nb"},
		{[]string{"a", "b", "c"}, "a\n\nb\n\nc"},
	}
	for _, c := range cases {
		if got := joinSegments(c.in); got != c.want {
			t.Errorf("joinSegments(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseResponsesSSE_NilEmit asserts assembly works with a nil emit (kickoff).
func TestParseResponsesSSE_NilEmit(t *testing.T) {
	stream := sseLines(
		[2]string{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"ignored-live"}`},
		[2]string{"response.completed", `{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}]}]}}`},
	)
	resp, err := parseResponsesSSE(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := resp.messageText(); got != "final" {
		t.Errorf("messageText = %q, want %q", got, "final")
	}
}

// TestRunToolLoop exercises one function_call → output → terminal text. It
// asserts tool_call + tool_result events fire, the loop stops on the terminal
// text turn (does NOT exceed the iteration cap / re-dispatch), and the assembled
// reply is the terminal text.
func TestRunToolLoop(t *testing.T) {
	var mu sync.Mutex
	turn := 0
	var sawInputs [][]inputItem
	var sawModels []string
	var sawSessionKeys []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body responsesRequest
		_ = decodeJSON(r, &body)
		mu.Lock()
		sawInputs = append(sawInputs, body.Input)
		sawModels = append(sawModels, body.Model)
		sawSessionKeys = append(sawSessionKeys, r.Header.Get(sessionKeyHeader))
		turn++
		current := turn
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if current == 1 {
			// First turn: a function_call (no terminal text yet).
			_, _ = w.Write([]byte(sseLines(
				[2]string{"response.completed", `{"type":"response.completed","response":{"output":[` +
					`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}` +
					`]}}`},
			)))
			return
		}
		// Second turn: terminal text, no calls → loop stops.
		_, _ = w.Write([]byte(sseLines(
			[2]string{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"the answer is 42"}`},
			[2]string{"response.completed", `{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"the answer is 42"}]}]}}`},
		)))
	}))
	defer srv.Close()

	var events []StreamEvent
	dispatchCalls := 0
	dispatch := func(name, args string) string {
		dispatchCalls++
		if name != "lookup" || args != `{"q":"x"}` {
			t.Errorf("dispatch got name=%q args=%q", name, args)
		}
		return `{"result":"ok"}`
	}

	reply, err := RunToolLoop(context.Background(), srv.Client(), srv.URL, "tok", "sess",
		testRuntimeModel, "sys", "hi", []ToolDef{{Type: "function", Name: "lookup"}}, dispatch,
		func(ev StreamEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("RunToolLoop: %v", err)
	}
	// 🔴 THE REQUEST'S `model` FIELD IS THE CALLER'S, NOT THIS PACKAGE'S. It was
	// a hardcoded vendor literal upstream; a mutation that made RunToolLoop ignore
	// its argument SURVIVED a fully green suite because nothing read the body's
	// model back. The fixture is deliberately a value this package never spells.
	mu.Lock()
	gotModels := append([]string(nil), sawModels...)
	gotKeys := append([]string(nil), sawSessionKeys...)
	mu.Unlock()
	if len(gotModels) != 2 {
		t.Fatalf("saw %d requests, want 2", len(gotModels))
	}
	for i, m := range gotModels {
		if m != testRuntimeModel {
			t.Errorf("request %d carried model %q, want the caller's %q", i, m, testRuntimeModel)
		}
	}
	// The session key must ride on EVERY request of the turn, not only the
	// first: the loop's whole purpose is to continue one conversation, and a
	// key dropped on the continuation silently starts a fresh context.
	for i, k := range gotKeys {
		if k != "sess" {
			t.Errorf("request %d carried %s = %q, want %q", i, sessionKeyHeader, k, "sess")
		}
	}

	if reply != "the answer is 42" {
		t.Errorf("reply = %q, want %q", reply, "the answer is 42")
	}
	if turn != 2 {
		t.Errorf("turns = %d, want 2 (one tool turn + one terminal); did it re-dispatch?", turn)
	}
	if dispatchCalls != 1 {
		t.Errorf("dispatch called %d times, want 1", dispatchCalls)
	}

	// tool_call before tool_result, plus the terminal text delta.
	var kinds []string
	var toolResultOK bool
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == "tool_result" {
			if ev.ToolID != "call_1" || ev.ToolName != "lookup" || ev.ToolOutput != `{"result":"ok"}` {
				t.Errorf("tool_result = %+v", ev)
			}
			toolResultOK = ev.ToolOK
		}
		if ev.Kind == "tool_call" {
			if ev.ToolID != "call_1" || ev.ToolName != "lookup" || ev.ToolArgs != `{"q":"x"}` {
				t.Errorf("tool_call = %+v", ev)
			}
		}
	}
	if !toolResultOK {
		t.Errorf("tool_result OK = false, want true (output had no error field)")
	}
	if !containsInOrder(kinds, "tool_call", "tool_result", "text") {
		t.Errorf("event kinds out of order: %v", kinds)
	}

	// Second turn fed the function_call_output back.
	if len(sawInputs) != 2 || len(sawInputs[1]) != 1 || sawInputs[1][0].Type != "function_call_output" || sawInputs[1][0].CallID != "call_1" {
		t.Errorf("second turn input = %+v, want one function_call_output for call_1", sawInputs)
	}
}

// TestRunToolLoop_ErrorToolResult asserts an error-shaped dispatch output marks
// the tool_result not-ok (cosmetic; the loop still continues normally).
func TestRunToolLoop_ErrorToolResult(t *testing.T) {
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		w.Header().Set("Content-Type", "text/event-stream")
		if turn == 1 {
			_, _ = w.Write([]byte(sseLines(
				[2]string{"response.completed", `{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"c1","name":"boom","arguments":"{}"}]}}`},
			)))
			return
		}
		_, _ = w.Write([]byte(sseLines(
			[2]string{"response.completed", `{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`},
		)))
	}))
	defer srv.Close()

	var resultOK *bool
	_, err := RunToolLoop(context.Background(), srv.Client(), srv.URL, "", "", testRuntimeModel, "", "go",
		[]ToolDef{{Type: "function", Name: "boom"}},
		func(string, string) string { return `{"error":"nope"}` },
		func(ev StreamEvent) {
			if ev.Kind == "tool_result" {
				v := ev.ToolOK
				resultOK = &v
			}
		})
	if err != nil {
		t.Fatalf("RunToolLoop: %v", err)
	}
	if resultOK == nil || *resultOK {
		t.Errorf("tool_result OK = %v, want false for error output", resultOK)
	}
}

// TestStreamResponsesUnsupported asserts a 404 maps to ErrResponsesUnsupported.
func TestStreamResponsesUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := streamResponses(context.Background(), srv.Client(), srv.URL, "", "", responsesRequest{}, nil)
	if err != ErrResponsesUnsupported {
		t.Errorf("err = %v, want ErrResponsesUnsupported", err)
	}
}

func decodeJSON(r *http.Request, v *responsesRequest) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func containsInOrder(haystack []string, needles ...string) bool {
	i := 0
	for _, h := range haystack {
		if i < len(needles) && h == needles[i] {
			i++
		}
	}
	return i == len(needles)
}

// TestResponsesURL pins the whole URL for each endpoint shape.
//
// 🔴 THE ENDPOINT IS THE FIELD THE UPSTREAM DESIGN HAD NO OVERRIDE FOR, so the
// cases below are deliberately NOT variations on one in-cluster address: a
// bare host and port, a non-default scheme, a base path with and without its
// trailing slash, and an IPv6 literal. A test built from one shape would pass
// against a body that ignored every field but Host.
func TestResponsesURL(t *testing.T) {
	cases := []struct {
		name string
		ep   provision.Endpoint
		want string
	}{
		{
			name: "host and port, scheme defaulted",
			ep:   provision.Endpoint{Host: "agent-a", Port: 8080},
			want: "http://agent-a:8080/v1/responses",
		},
		{
			name: "an explicit scheme is honoured",
			ep:   provision.Endpoint{Scheme: "https", Host: "agent-a.example", Port: 443},
			want: "https://agent-a.example:443/v1/responses",
		},
		{
			name: "a base path is preserved",
			ep:   provision.Endpoint{Host: "gw.example", Port: 80, Path: "/agents/a"},
			want: "http://gw.example:80/agents/a/v1/responses",
		},
		{
			// 🔴 THE TRAILING-SLASH CASE. Without the trim this yields a double
			// slash, which some routers treat as a different route — a 404 that
			// looks like an unsupported runtime rather than a malformed URL.
			name: "a trailing slash on the base path is not doubled",
			ep:   provision.Endpoint{Host: "gw.example", Port: 80, Path: "/agents/a/"},
			want: "http://gw.example:80/agents/a/v1/responses",
		},
		{
			name: "an IPv6 literal is bracketed",
			ep:   provision.Endpoint{Host: "fd00::1", Port: 8080},
			want: "http://[fd00::1]:8080/v1/responses",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResponsesURL(c.ep); got != c.want {
				t.Errorf("ResponsesURL(%+v) = %q, want %q", c.ep, got, c.want)
			}
		})
	}
}
