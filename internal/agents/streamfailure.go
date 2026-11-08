package agents

import (
	"encoding/json"
	"strings"
)

// A TURN THE RUNTIME SAYS FAILED IS AN ERROR, NOT AN EMPTY ANSWER.
//
// 🔴 THE MEASURED SHAPE. An agent runtime that cannot run a turn — the case
// that started this was a model name it did not know ("FailoverError: Unknown
// model: …") — answers /v1/responses with 200 and a stream that reports the
// failure as an EVENT: `response.failed` carrying response.error, or a bare
// `error` event. The parser used to read only response.completed, so such a
// stream assembled to "" with a nil error: the kickoff recorded the generic
// "empty reply", a chat turn stored nothing, and the runtime's own sentence —
// the one thing that said what was wrong — was dropped on the floor.
//
// streamFailure reads both shapes (the OpenAI Responses stream's
// `response.failed` and `error` events, which the runtimes speak) and the
// parser returns it as a *StreamError, so every caller's existing error path
// carries the runtime's words.

// StreamError is a turn the runtime itself reported as failed, mid-stream.
type StreamError struct {
	// Code is the runtime's error code, when it sent one.
	Code string
	// Message is the runtime's own sentence.
	Message string
}

func (e *StreamError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = "the runtime reported the turn failed and gave no reason"
	}
	if e.Code != "" && !strings.Contains(msg, e.Code) {
		return "runtime turn failed (" + e.Code + "): " + msg
	}
	return "runtime turn failed: " + msg
}

// streamFailureMax bounds the runtime's sentence as stored (it reaches
// error_message and a push body).
const streamFailureMax = 512

// streamFailure is the failure one SSE event reports, or nil when it reports
// none, and whether the event is TERMINAL — the end of the turn in the stream
// protocol (response.completed, response.failed, response.incomplete), after
// which the reader stops. The type inside data wins over the event name, as for
// every event.
//
// 🔴 A COMPLETION WHOSE STATUS IS "failed" IS A FAILURE, NOT AN ANSWER. A
// gateway runtime whose run failed writes one, carrying a placeholder as its
// text, and read as an answer that placeholder was stored as the agent's reply.
func streamFailure(ev sseEvent) (*StreamError, bool) {
	var env struct {
		Type     string `json:"type"`
		Code     any    `json:"code"`
		Message  string `json:"message"`
		Error    any    `json:"error"`
		Response struct {
			Status string                `json:"status"`
			Output []responsesOutputItem `json:"output"`
			Error  *struct {
				Code    any    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(ev.data), &env); err != nil {
		return nil, false
	}
	kind := env.Type
	if kind == "" {
		kind = ev.name
	}
	switch kind {
	case "response.completed", "response.incomplete":
		if env.Response.Status != "failed" {
			return nil, true
		}
		fallthrough
	case "response.failed":
		f := &StreamError{}
		if e := env.Response.Error; e != nil {
			f.Code, f.Message = codeString(e.Code), e.Message
		}
		// 🔴 NO error OBJECT IS NOT NO REASON: a failed completion can carry its
		// reason (or the partial text it got to) as OUTPUT text. That text is the
		// failure's message, bounded like the runtime's other words; "gave no
		// reason" is only for a failure that carried neither.
		if strings.TrimSpace(f.Message) == "" {
			r := responsesResponse{Output: env.Response.Output}
			f.Message = strings.TrimSpace(r.messageText())
		}
		return bound(f), true
	case "error":
		f := &StreamError{Code: codeString(env.Code), Message: env.Message}
		// Some runtimes nest it: {"type":"error","error":{"message":…}}.
		if m, ok := env.Error.(map[string]any); ok {
			if f.Message == "" {
				f.Message, _ = m["message"].(string)
			}
			if f.Code == "" {
				f.Code = codeString(m["code"])
			}
		} else if s, ok := env.Error.(string); ok && f.Message == "" {
			f.Message = s
		}
		return bound(f), false
	}
	return nil, false
}

func codeString(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case float64:
		b, _ := json.Marshal(c)
		return string(b)
	}
	return ""
}

func bound(f *StreamError) *StreamError {
	if r := []rune(f.Message); len(r) > streamFailureMax {
		f.Message = string(r[:streamFailureMax]) + "…"
	}
	return f
}
