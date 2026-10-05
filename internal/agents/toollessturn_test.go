package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRunToollessTurnSendsNoToolsAndReturnsTheAssembledText pins the request shape.
//
// 🔴 THE ABSENT `tools` KEY IS THE ASSERTION, NOT A DETAIL. An empty-but-present
// tools array is a different request, and a runtime that receives one is entitled to
// treat the turn as tool-enabled — which is precisely what this function exists not
// to be. It is read off the RAW JSON rather than off a decoded struct, because
// `omitempty` means a decoded []ToolDef cannot tell "absent" from "empty".
func TestRunToollessTurnSendsNoToolsAndReturnsTheAssembledText(t *testing.T) {
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\n")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","output_index":0,"delta":"six of them"}`+"\n\n")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"message",`+
			`"role":"assistant","content":[{"type":"output_text","text":"six of them"}]}]}}`+"\n\n")
	}))
	defer srv.Close()

	var streamed string
	reply, err := RunToollessTurn(context.Background(), srv.Client(), srv.URL, "tok", "sess-19",
		"runtime-sentinel", "be brief", "how many?",
		func(ev StreamEvent) { streamed += ev.Text })
	if err != nil {
		t.Fatalf("RunToollessTurn: %v", err)
	}
	if reply != "six of them" {
		t.Errorf("reply = %q, want %q", reply, "six of them")
	}
	if _, present := raw["tools"]; present {
		t.Errorf("the request carries a `tools` key (%s). A tool-less turn must omit it: a "+
			"runtime handed an empty array may still treat the turn as tool-enabled, and the "+
			"model would then be offered nothing it can call", raw["tools"])
	}
	// The three inputs the caller is required to supply, read off the wire.
	for key, want := range map[string]string{
		"model":        "runtime-sentinel",
		"instructions": "be brief",
	} {
		var got string
		if err := json.Unmarshal(raw[key], &got); err != nil || got != want {
			t.Errorf("wire %s = %s, want %q", key, raw[key], want)
		}
	}
	var wantStream bool
	if err := json.Unmarshal(raw["stream"], &wantStream); err != nil || !wantStream {
		t.Errorf("wire stream = %s, want true — the UI renders deltas as they arrive", raw["stream"])
	}
	// Asserted separately from the return value: a version that assembled the text
	// without streaming it satisfies the reply check above, and the observable is a
	// turn that appears frozen until it ends.
	if streamed != "six of them" {
		t.Errorf("streamed text = %q, want %q", streamed, "six of them")
	}
}

// TestRunToollessTurnMapsA404ToTheUnsupportedSentinelUnWrapped pins the fallback
// signal on this path, which is the one agentgateway.Gateway.Chat branches on.
//
// 🔴 A WRAPPED ERROR BREAKS THAT BRANCH AND THE OBSERVABLE IS A LOST TURN. Chat's
// only fallback keys on this sentinel; without it an agent whose image predates the
// endpoint gets an error instead of an answer from the legacy transport.
func TestRunToollessTurnMapsA404ToTheUnsupportedSentinelUnWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := RunToollessTurn(context.Background(), srv.Client(), srv.URL, "", "", "m", "", "hi", nil)
	if err != ErrResponsesUnsupported {
		t.Errorf("err = %v, want ErrResponsesUnsupported compared with == (a %%w wrap still "+
			"satisfies errors.Is, so == is the stricter claim and the one callers make)", err)
	}
}

// TestRunToollessTurnSurvivesAFunctionCallItNeverOfferedATool is the reason this is
// not RunToolLoop with nil arguments.
//
// 🔴 THAT LOOP CALLS dispatch FOR EVERY function_call, SO A NIL DISPATCH IS A NIL
// FUNC CALL — a panic inside an HTTP handler, i.e. the server, reachable the moment
// a runtime emits a call for a tool the request never carried. One turn with no loop
// cannot reach it, and this test is what makes that a measured property rather than
// an argument: the fixture emits exactly such a call.
func TestRunToollessTurnSurvivesAFunctionCallItNeverOfferedATool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[`+
			`{"type":"function_call","call_id":"call-83","name":"agent_get_task","arguments":"{}"},`+
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"asking anyway"}]}`+
			`]}}`+"\n\n")
	}))
	defer srv.Close()

	reply, err := RunToollessTurn(context.Background(), srv.Client(), srv.URL, "", "", "m", "", "hi", nil)
	if err != nil {
		t.Fatalf("RunToollessTurn: %v", err)
	}
	if reply != "asking anyway" {
		t.Errorf("reply = %q, want the message text %q — the function_call item is not a tool this "+
			"turn can run, and it must not displace the text the model did produce",
			reply, "asking anyway")
	}
}
