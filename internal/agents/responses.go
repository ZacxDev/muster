package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/provision"
)

// Native function-tool calling against an agent runtime's /v1/responses
// endpoint.
//
// 🔴 TOOLS REACH THE MODEL THROUGH THE REQUEST, NOT THROUGH THE AGENT'S OWN
// CONFIGURATION, and that was established by measurement rather than by reading
// a runtime's documentation. Tools are passed in the request (the flat function
// shape below) and THIS side runs the client-side call→execute→continue loop,
// dispatching each function call to an in-process handler. A runtime's own skill
// files, instruction files and tool-server configuration do NOT surface tools to
// the model over this endpoint — wiring them there looks correct, deploys
// cleanly, and silently gives the model no tools at all.
//
// ⚠ THE ENDPOINT IS NOT UNIVERSAL. Older runtime builds answer 404 here; callers
// are expected to branch on ErrResponsesUnsupported and fall back to a plain
// chat-completions call, which carries no tools.

// ErrResponsesUnsupported signals the agent runtime lacks /v1/responses (404),
// so the caller should fall back to /v1/chat/completions (no tools).
var ErrResponsesUnsupported = errors.New("agent runtime does not support /v1/responses (HTTP 404)")

// sessionKeyHeader is the request header the agent runtime reads a conversation
// key from.
//
// 🔴 ITS VALUE IS A THIRD PARTY'S WIRE PROTOCOL, NOT THIS PROJECT'S IDENTITY,
// which is why it survived the scrub with a vendor name still in it. Changing
// the spelling would not genericise anything — it would simply stop the runtime
// recognising the header, and every turn would start with an empty context
// while everything still returned 200. It is a constant so there is exactly one
// place to change if the attached runtime ever changes.
const sessionKeyHeader = "X-Openclaw-Session-Key"

// maxToolLoopIterations caps the call→execute→continue loop per user turn.
const maxToolLoopIterations = 8

// ToolDef is the flat Responses-API tool shape (verified against 2026.5.7):
//
//	{"type":"function","name":"...","description":"...","parameters":{json-schema}}
//
// The nested {"function":{...}} form is rejected (HTTP 400) on this codepath.
type ToolDef struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolDispatch executes a tool call (by name, with raw JSON arguments) and
// returns the output string fed back to the model as a function_call_output.
// Errors should be returned as a string the model can read (e.g. JSON error);
// the loop never aborts on a tool error.
type ToolDispatch func(name, arguments string) string

// StreamEvent is one server-side streaming event surfaced to the chat UI as the
// tool loop runs. Kind is one of "text", "thinking", "tool_call", "tool_result".
// The KICKOFF stream (provision.go) additionally uses two synthetic kinds the tool
// loop never emits: "user" (the kickoff prompt, shown first) and "done" (terminal,
// releasing the chat's streaming guard) — carried over the same StreamEmit/hook.
type StreamEvent struct {
	Kind       string // "text" | "thinking" | "tool_call" | "tool_result" | "user" | "done"
	Text       string // text/thinking chunk
	ToolID     string // tool_call / tool_result: the function call_id
	ToolName   string // tool_call / tool_result: the function name
	ToolArgs   string // tool_call: raw JSON arguments
	ToolOutput string // tool_result: dispatch output (truncated)
	ToolOK     bool   // tool_result: whether the dispatch reported success
}

// StreamEmit receives streaming events during a turn. It is always nil-safe at
// call sites (the kickoff path passes nil).
type StreamEmit func(StreamEvent)

// toolResultMaxBytes caps the tool_result output surfaced to the UI (the full
// output still feeds back to the model as the function_call_output).
const toolResultMaxBytes = 2048

// truncateForStream clips s to at most n bytes, appending an ellipsis marker.
func truncateForStream(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type inputItem struct {
	Type string `json:"type"`
	// type=message
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// type=function_call_output
	CallID string `json:"call_id,omitempty"`
	Output string `json:"output,omitempty"`
}

type responsesRequest struct {
	Model        string      `json:"model"`
	Input        []inputItem `json:"input"`
	Instructions string      `json:"instructions,omitempty"`
	Tools        []ToolDef   `json:"tools,omitempty"`
	ToolChoice   any         `json:"tool_choice,omitempty"`
	Stream       bool        `json:"stream"`
}

type responsesMessageContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesOutputItem struct {
	Type string `json:"type"`
	// type=message
	Role    string                        `json:"role,omitempty"`
	Content []responsesMessageContentPart `json:"content,omitempty"`
	// type=function_call
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type responsesResponse struct {
	ID     string                `json:"id"`
	Status string                `json:"status"`
	Output []responsesOutputItem `json:"output"`
}

func (r *responsesResponse) messageText() string {
	// Join distinct output_text segments with a blank line, never bare
	// concatenation: a turn can carry several message items (the model narrates
	// around its native in-pod tools), and gluing them directly fuses the words at
	// the boundary ("configGood", "the PR.PR created"). segsep only inserts the
	// separator between two non-empty segments, so single-segment turns are
	// unchanged.
	var segs []string
	for _, item := range r.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				segs = append(segs, part.Text)
			}
		}
	}
	return joinSegments(segs)
}

// joinSegments concatenates assistant text segments, inserting a blank-line
// separator only between two adjacent NON-empty segments so distinct output_text
// items read as separate paragraphs instead of fusing at the boundary. Empty
// segments contribute nothing (and force no separator).
func joinSegments(segs []string) string {
	var b strings.Builder
	for _, s := range segs {
		if s == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s)
	}
	return b.String()
}

// itemText concatenates the text of an output item's content parts (used for a
// reasoning item carried in the completed output, defensive).
func (i responsesOutputItem) itemText() string {
	var b strings.Builder
	for _, part := range i.Content {
		if part.Type == "output_text" || part.Type == "text" || part.Type == "reasoning_text" || part.Type == "summary_text" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func (r *responsesResponse) functionCalls() []responsesOutputItem {
	out := make([]responsesOutputItem, 0, len(r.Output))
	for _, item := range r.Output {
		if item.Type == "function_call" {
			out = append(out, item)
		}
	}
	return out
}

func callID(item responsesOutputItem) string {
	if item.CallID != "" {
		return item.CallID
	}
	return item.ID
}

// ResponsesURL is the agent runtime's /v1/responses URL, derived from wherever
// the provisioner says that instance is reachable.
//
// 🔴 IT TAKES AN ENDPOINT BECAUSE UPSTREAM IT TOOK A NAME AND BUILT THE ADDRESS
// ITSELF. The upstream body interpolated the agent's name twice into a fixed
// in-cluster DNS format string, prefixed "http://" and appended the path —
// one cluster's DNS layout, one port, one scheme, hardcoded with no override
// anywhere in the codebase: not an environment variable, not a config field,
// not a per-agent one. Reaching an agent through anything that was not that
// specific cluster was simply not expressible. provision.Endpoint is the
// replacement, and it has three layers behind it (a per-instance override on
// the Spec, the driver's configurable template, the driver's default), so this
// function's whole job is now the path suffix.
//
// The trailing-slash trim matters: Endpoint.Path is a BASE path and a caller
// that spells it "/agent/" would otherwise produce a double slash, which some
// routers treat as a different route.
func ResponsesURL(ep provision.Endpoint) string {
	return strings.TrimSuffix(ep.URL(), "/") + "/v1/responses"
}

// sseEvent is one parsed Server-Sent Event from the /v1/responses stream.
type sseEvent struct {
	name string
	data string
}

// streamResponses POSTs one /v1/responses turn with stream:true and parses the
// OpenAI-Responses-style SSE stream, invoking emit for text/thinking deltas as
// they arrive. It assembles the authoritative *responsesResponse from the
// response.completed event's data.response.output (so functionCalls() /
// messageText() behave exactly as the non-streaming path). emit is nil-safe (the
// kickoff path assembles only). 404 → ErrResponsesUnsupported, preserved.
func streamResponses(ctx context.Context, client *http.Client, url, token, sessionKey string, body responsesRequest, emit StreamEmit) (*responsesResponse, error) {
	body.Stream = true
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionKey != "" {
		req.Header.Set(sessionKeyHeader, sessionKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrResponsesUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		snippet := string(respBody)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "…"
		}
		return nil, fmt.Errorf("responses HTTP %d: %s", resp.StatusCode, snippet)
	}
	return parseResponsesSSE(resp.Body, emit)
}

// parseResponsesSSE parses the SSE body of a streaming /v1/responses turn. It is
// split out from the HTTP plumbing so it is unit-testable against a synthetic
// byte stream. It mirrors gateway.go's bufio.Scanner SSE approach with a raised
// buffer cap (a single response.completed line carries the full output array).
func parseResponsesSSE(body io.Reader, emit StreamEmit) (*responsesResponse, error) {
	var final *responsesResponse
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var ev sseEvent
	// lastTextIndex is the output_index of the most recent text delta emitted this
	// turn (-1 = none yet). A text delta whose index is higher belongs to a NEW
	// message item, so handleResponsesEvent injects a paragraph break before it —
	// this is what keeps distinct segments from fusing ("configGood").
	lastTextIndex := -1
	flush := func() {
		if ev.data == "" {
			ev = sseEvent{}
			return
		}
		handleResponsesEvent(ev, emit, &final, &lastTextIndex)
		ev = sseEvent{}
	}

	for sc.Scan() {
		line := sc.Text()
		// A blank line terminates an event.
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "event:") {
			ev.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if d == "[DONE]" {
				flush()
				break
			}
			if ev.data != "" {
				ev.data += "\n"
			}
			ev.data += d
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if final == nil {
		// No response.completed seen (defensive): return an empty response rather
		// than nil so callers can safely call functionCalls()/messageText().
		final = &responsesResponse{}
	}
	return final, nil
}

// handleResponsesEvent interprets one SSE event: text/reasoning deltas → emit;
// the response.completed event → assemble the authoritative *responsesResponse.
// The event "type" inside data takes precedence over the SSE event name; both
// are checked so the parser is robust to either being present.
func handleResponsesEvent(ev sseEvent, emit StreamEmit, final **responsesResponse, lastTextIndex *int) {
	var env struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Text     string `json:"text"`
		Index    int    `json:"output_index"`
		Response struct {
			Output []responsesOutputItem `json:"output"`
		} `json:"response"`
		Item responsesOutputItem `json:"item"`
	}
	if err := json.Unmarshal([]byte(ev.data), &env); err != nil {
		return
	}
	kind := env.Type
	if kind == "" {
		kind = ev.name
	}

	switch {
	case kind == "response.completed":
		r := &responsesResponse{Status: "completed", Output: env.Response.Output}
		*final = r
	case kind == "response.output_text.delta":
		if emit != nil && env.Delta != "" {
			// A turn can stream several message items (the model narrates around its
			// native in-pod tools). Their deltas carry increasing output_index; when
			// it advances past the previous text delta's item, inject a blank-line
			// separator so the segments render as distinct paragraphs instead of
			// fusing at the boundary. Guarded by lastTextIndex >= 0 so the first
			// segment of a turn never gets a leading break.
			if lastTextIndex != nil && *lastTextIndex >= 0 && env.Index > *lastTextIndex {
				emit(StreamEvent{Kind: "text", Text: "\n\n"})
			}
			if lastTextIndex != nil {
				*lastTextIndex = env.Index
			}
			emit(StreamEvent{Kind: "text", Text: env.Delta})
		}
	case strings.Contains(kind, "reasoning"):
		// Defensive reasoning handling (not observed on deepseek-v4-flash): any
		// event whose name/type mentions reasoning carries thinking text via its
		// delta (preferred) or text field. An output_item with type "reasoning" is
		// handled below.
		if emit != nil {
			t := env.Delta
			if t == "" {
				t = env.Text
			}
			if t == "" && env.Item.Type == "reasoning" {
				t = env.Item.itemText()
			}
			if t != "" {
				emit(StreamEvent{Kind: "thinking", Text: t})
			}
		}
	case env.Item.Type == "reasoning":
		// Some streams carry reasoning only as an output item (no reasoning-named
		// event). Surface its text as thinking.
		if emit != nil {
			if t := env.Item.itemText(); t != "" {
				emit(StreamEvent{Kind: "thinking", Text: t})
			}
		}
	}
}

// runToolLoop runs the /v1/responses call→execute→continue loop: POST {input,
// tools, instructions}; for each function_call, dispatch it and feed back a
// function_call_output; stop when the model emits no function_call (terminal
// text) or the iteration cap is hit. Each turn streams live via streamResponses
// — text/thinking deltas reach emit as they arrive; before each tool dispatch a
// "tool_call" event fires, after it a "tool_result" event. emit is OPTIONAL
// (nil-safe; the kickoff path passes nil and only the assembled text is used).
//
// Re-dispatch guard: the loop only continues while the model keeps emitting
// function_calls and is hard-capped at maxToolLoopIterations, so a mid-turn
// failure cannot cause unbounded re-dispatch. The terminal-text turn (no calls)
// always stops the loop.
// runToolLoop drives one user turn to completion: request, dispatch any
// function calls the model asks for, feed the outputs back, repeat.
//
// 🔴 model IS A PARAMETER RATHER THAN A CONSTANT, AND THE CALLER MUST SUPPLY
// THE ATTACHED RUNTIME'S OWN PASSTHROUGH SENTINEL. Upstream this was a
// hardcoded literal naming one specific agent image — a transport helper
// deciding what model a request names, which is neither its job nor portable.
// A runtime that runs whatever model its own configuration selects still
// requires the `model` field to be present and to carry the value IT expects;
// there is no neutral spelling this package can invent. See the carve note at
// the foot of this file.
func runToolLoop(
	ctx context.Context,
	client *http.Client,
	url, token, sessionKey, model, instructions, userMessage string,
	tools []ToolDef,
	dispatch ToolDispatch,
	emit StreamEmit,
) (string, error) {
	input := []inputItem{{Type: "message", Role: "user", Content: userMessage}}

	for i := 0; i < maxToolLoopIterations; i++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		resp, err := streamResponses(ctx, client, url, token, sessionKey, responsesRequest{
			Model:        model,
			Input:        input,
			Instructions: instructions,
			Tools:        tools,
		}, emit)
		if err != nil {
			return "", err
		}
		calls := resp.functionCalls()
		if len(calls) == 0 {
			// Terminal text turn: the text already streamed live via emit during
			// streamResponses, so just return the assembled text (don't re-emit).
			return resp.messageText(), nil
		}
		// Execute each call; surface tool_call/tool_result to the UI and feed the
		// outputs back as the next turn's input.
		input = make([]inputItem, 0, len(calls))
		for _, call := range calls {
			id := callID(call)
			if emit != nil {
				emit(StreamEvent{Kind: "tool_call", ToolID: id, ToolName: call.Name, ToolArgs: call.Arguments})
			}
			out := dispatch(call.Name, call.Arguments)
			if emit != nil {
				emit(StreamEvent{
					Kind:       "tool_result",
					ToolID:     id,
					ToolName:   call.Name,
					ToolOK:     !toolOutputIsError(out),
					ToolOutput: truncateForStream(out, toolResultMaxBytes),
				})
			}
			input = append(input, inputItem{
				Type:   "function_call_output",
				CallID: id,
				Output: out,
			})
		}
	}
	return "", fmt.Errorf("tool loop exceeded %d iterations", maxToolLoopIterations)
}

// toolOutputIsError best-effort detects whether a dispatch output represents an
// error, for the tool_result "ok" flag surfaced to the UI. Dispatch handlers
// return errors as a JSON object with an "error" field (the model reads them as
// strings); this is purely cosmetic for the UI and never affects the loop.
func toolOutputIsError(out string) bool {
	t := strings.TrimSpace(out)
	if t == "" {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(t), &obj) == nil {
		if _, ok := obj["error"]; ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 🔴 NOTHING IN THIS REPOSITORY CALLS runToolLoop YET, AND THIS IS THE RECORD
// OF IT.
//
// Upstream its two callers were methods on the provisioning type, which also
// owned the HTTP client and computed the agent's address from a hardcoded
// cluster DNS format string. Neither came across: the address is now
// provision.Endpoint (see ResponsesURL), and the chat surface itself belongs to
// a later carve.
//
// ⚠ OWED, AND NAMED SO IT IS NOT AN OBJECT NOBODY CAN CLOSE. TWO things this
// package cannot supply and a caller must:
//
//  1. the `model` argument — the attached runtime's passthrough sentinel. It is
//     a REQUIRED field on the wire and the value is the runtime's, not ours.
//  2. the endpoint — from provision.Provisioner.Endpoint, via ResponsesURL.
//
// CLOSING CONDITION: the pull request that adds muster's agent chat path wires
// both, and verifies one real turn against a live runtime — not against the
// test server in responses_test.go, which answers whatever it is asked. WHO
// CHECKS IT: the reviewer of that pull request, against this comment.
// ---------------------------------------------------------------------------
