package cctranscript

import "github.com/ZacxDev/muster/internal/askq"

// OpenAsk is an AskUserQuestion call a transcript shows OPEN (OpenAskOf): its
// tool_use id and its questions, as the call's Ask carries them.
type OpenAsk struct {
	ToolUseID string
	Questions []askq.Question
}

// OpenAskOf is the AskUserQuestion a WAITING session's transcript events
// (oldest first) show open: the newest tool call after the last prompt that
// has no result, when that call is an AskUserQuestion carrying its questions
// (Event.Ask). nil otherwise — no such call, a call with a result (answered or
// dismissed), or a newest unanswered call of another tool.
//
// Claude Code writes an AskUserQuestion's tool_use when its picker opens and
// its tool_result once the picker is answered or dismissed (measured on
// 2.1.289), so while the session waits with its picker on screen this is that
// call. It is the same call the host's PendingOf summarises
// (internal/mirrorhost); muster reads it from the transcript it streams
// (internal/api's open-question scan), so a question outlives a restart.
func OpenAskOf(events []Event) *OpenAsk {
	start := 0
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == KindPrompt {
			start = i + 1
			break
		}
	}
	answered := map[string]bool{}
	for _, ev := range events[start:] {
		if ev.Kind == KindToolResult {
			answered[ev.ToolID] = true
		}
	}
	for i := len(events) - 1; i >= start; i-- {
		ev := events[i]
		if ev.Kind != KindToolCall || answered[ev.ToolID] {
			continue
		}
		if ev.Tool != askq.ToolName || ev.ToolID == "" || ev.Ask == nil || len(ev.Ask.Questions) == 0 {
			return nil
		}
		return &OpenAsk{ToolUseID: ev.ToolID, Questions: append([]askq.Question(nil), ev.Ask.Questions...)}
	}
	return nil
}
