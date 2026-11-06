package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestATurnEndedByAToolRunsNoFurtherTool (ask_owner, internal/api owner_ask.go):
// a tool that calls EndTurn ends the turn STRUCTURALLY. The loop sends the
// round's outputs back once — the runtime's session must hold an output for
// every call it made — with tool_choice "none", and returns; and when the
// runtime ignores tool_choice and asks for another tool anyway, that call is
// NOT run. Control: the same runtime without EndTurn runs the second call.
func TestATurnEndedByAToolRunsNoFurtherTool(t *testing.T) {
	type seen struct {
		inputs     [][]inputItem
		toolChoice []any
	}
	runtime := func(s *seen, mu *sync.Mutex) *httptest.Server {
		turn := 0
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body responsesRequest
			_ = decodeJSON(r, &body)
			mu.Lock()
			turn++
			current := turn
			s.inputs = append(s.inputs, body.Input)
			s.toolChoice = append(s.toolChoice, body.ToolChoice)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			switch current {
			case 1:
				_, _ = w.Write([]byte(sseLines([2]string{"response.completed", `{"type":"response.completed","response":{"output":[` +
					`{"type":"function_call","call_id":"call_ask","name":"ask_owner","arguments":"{}"}]}}`})))
			case 2:
				// A runtime that does NOT honour tool_choice: another call.
				_, _ = w.Write([]byte(sseLines([2]string{"response.completed", `{"type":"response.completed","response":{"output":[` +
					`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"asked"}]},` +
					`{"type":"function_call","call_id":"call_more","name":"agent_get_task","arguments":"{}"}]}}`})))
			default:
				_, _ = w.Write([]byte(sseLines([2]string{"response.completed", `{"type":"response.completed","response":{"output":[` +
					`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"went on"}]}]}}`})))
			}
		}))
	}
	run := func(end bool) (reply string, dispatched []string, s *seen) {
		s = &seen{}
		var mu sync.Mutex
		srv := runtime(s, &mu)
		defer srv.Close()
		ctx := WithTurnEnd(context.Background())
		dispatch := func(name, _ string) string {
			dispatched = append(dispatched, name)
			if name == "ask_owner" && end {
				EndTurn(ctx)
			}
			return `{"ok":true}`
		}
		reply, err := RunToolLoop(ctx, srv.Client(), srv.URL, "tok", "sess", testRuntimeModel, "sys", "pick one",
			[]ToolDef{{Type: "function", Name: "ask_owner"}, {Type: "function", Name: "agent_get_task"}}, dispatch, nil)
		if err != nil {
			t.Fatalf("RunToolLoop: %v", err)
		}
		return reply, dispatched, s
	}

	reply, dispatched, s := run(true)
	if len(dispatched) != 1 || dispatched[0] != "ask_owner" {
		t.Fatalf("an ended turn dispatched %v, want only [ask_owner]", dispatched)
	}
	if len(s.inputs) != 2 {
		t.Fatalf("an ended turn made %d requests, want 2 (the call, then its output back)", len(s.inputs))
	}
	if in := s.inputs[1]; len(in) != 1 || in[0].Type != "function_call_output" || in[0].CallID != "call_ask" {
		t.Errorf("the second request carried %+v, want ask_owner's output", in)
	}
	if s.toolChoice[0] != nil || s.toolChoice[1] != "none" {
		t.Errorf("tool_choice = %v, want [nil none]", s.toolChoice)
	}
	if reply != "asked" {
		t.Errorf("reply %q, want the closing text %q", reply, "asked")
	}

	// Control: without EndTurn, the same runtime's second call IS run.
	_, dispatched, s = run(false)
	if len(dispatched) != 2 || dispatched[1] != "agent_get_task" || len(s.inputs) != 3 {
		t.Fatalf("control: an unended turn dispatched %v over %d requests, want [ask_owner agent_get_task] over 3",
			dispatched, len(s.inputs))
	}
}

// TestTheTurnEndMarkIsPerTurn: EndTurn on a ctx without a mark does nothing and
// says so (an MCP call has none); a mark starts unset, and a new turn's mark is
// not the last one's.
func TestTheTurnEndMarkIsPerTurn(t *testing.T) {
	if EndTurn(context.Background()) || TurnEnded(context.Background()) {
		t.Fatal("a ctx without a mark reported one")
	}
	a := WithTurnEnd(context.Background())
	if TurnEnded(a) {
		t.Fatal("a fresh mark is set")
	}
	if !EndTurn(a) || !TurnEnded(a) {
		t.Fatal("EndTurn did not set the mark")
	}
	if b := WithTurnEnd(a); TurnEnded(b) {
		t.Fatal("a new turn inherited the last turn's mark")
	}
}
