package cctranscript

import (
	"reflect"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
)

var openAskQs = []askq.Question{{Question: "Pick a fruit", Header: "Fruit", Options: []askq.Option{{Label: "Apple"}, {Label: "Pear"}}}}

func askCallEv(id string) Event {
	return Event{Kind: KindToolCall, ToolID: id, Tool: askq.ToolName, Ask: &Ask{Questions: openAskQs}}
}

// TestOpenAskOfIsTheWaitingSessionsUnansweredQuestion: the AskUserQuestion is
// open only while it is the newest unanswered call after the last prompt — and
// never once its result is recorded (answered or dismissed).
func TestOpenAskOfIsTheWaitingSessionsUnansweredQuestion(t *testing.T) {
	prompt := Event{Kind: KindPrompt, Text: "go"}
	result := func(id string) Event { return Event{Kind: KindToolResult, ToolID: id, OK: true} }
	dismissed := Event{Kind: KindToolResult, ToolID: "q1"}
	read := Event{Kind: KindToolCall, ToolID: "r", Tool: "Read"}
	bash := Event{Kind: KindToolCall, ToolID: "b", Tool: "Bash"}
	noAsk := Event{Kind: KindToolCall, ToolID: "q1", Tool: askq.ToolName} // its questions past askq's bounds: no Ask
	for _, c := range []struct {
		name string
		evs  []Event
		want string
	}{
		{"the open question", []Event{prompt, read, result("r"), askCallEv("q1")}, "q1"},
		{"no prompt in the read at all", []Event{askCallEv("q1")}, "q1"},
		{"answered", []Event{prompt, askCallEv("q1"), result("q1")}, ""},
		{"dismissed (a result with no answers)", []Event{prompt, askCallEv("q1"), dismissed}, ""},
		{"before the last prompt (interrupted)", []Event{prompt, askCallEv("q1"), prompt}, ""},
		{"a newer unanswered call of another tool", []Event{prompt, askCallEv("q1"), bash}, ""},
		{"an older answered one, then the open one", []Event{prompt, askCallEv("q0"), result("q0"), askCallEv("q1")}, "q1"},
		{"an open one, then an answered other call", []Event{prompt, askCallEv("q1"), read, result("r")}, "q1"},
		{"a question set past askq's bounds", []Event{prompt, noAsk}, ""},
		{"nothing", nil, ""},
	} {
		got := OpenAskOf(c.evs)
		switch {
		case c.want == "" && got != nil:
			t.Errorf("%s: open %+v, want none", c.name, got)
		case c.want != "" && (got == nil || got.ToolUseID != c.want || !reflect.DeepEqual(got.Questions, openAskQs)):
			t.Errorf("%s: open %+v, want %s", c.name, got, c.want)
		}
	}
}
