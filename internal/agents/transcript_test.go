package agents

import (
	"reflect"
	"testing"
)

// TestPartCollectorOrdersPartsByArrival asserts the collector reconstructs a
// turn's ordered parts from the stream: text deltas accumulate into a segment
// that flushes at each tool boundary, so text→tool→text yields three parts in
// arrival order. 'user'/'done' are ignored (persisted separately / not a part).
func TestPartCollectorOrdersPartsByArrival(t *testing.T) {
	pc := &PartCollector{}
	for _, ev := range []StreamEvent{
		{Kind: "user", Text: "hi"}, // ignored (persisted separately)
		{Kind: "text", Text: "let me "},
		{Kind: "text", Text: "check"},
		{Kind: "tool_call", ToolID: "c1", ToolName: "run", ToolArgs: `{"x":1}`},
		{Kind: "tool_result", ToolID: "c1", ToolName: "run", ToolOK: true, ToolOutput: "ok"},
		{Kind: "text", Text: "done"},
		{Kind: "done"},
	} {
		pc.Observe(ev)
	}
	want := []TurnPart{
		{Kind: "text", Content: "let me check"},
		{Kind: "tool_call", ToolID: "c1", ToolName: "run", Content: `{"x":1}`},
		{Kind: "tool_result", ToolID: "c1", ToolName: "run", ToolOK: true, Content: "ok"},
		{Kind: "text", Content: "done"},
	}
	if got := pc.Parts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("parts =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestPartCollectorSkipsEmptyText asserts a whitespace-only / empty text segment
// is not persisted as a part (no empty bubbles), and thinking flushes pending
// text without creating a part of its own.
func TestPartCollectorSkipsEmptyText(t *testing.T) {
	pc := &PartCollector{}
	pc.Observe(StreamEvent{Kind: "text", Text: "   "})
	pc.Observe(StreamEvent{Kind: "thinking", Text: "hmm"})
	pc.Observe(StreamEvent{Kind: "text", Text: ""})
	if got := pc.Parts(); len(got) != 0 {
		t.Fatalf("whitespace/empty text should yield no parts, got %+v", got)
	}
}

// TestPartCollectorDropsSentinelReply asserts a gateway "no reply" sentinel is not
// persisted as a text part (so it never shows as a noise bubble), while a real
// reply in the same turn is kept.
func TestPartCollectorDropsSentinelReply(t *testing.T) {
	for _, s := range []string{"NO_REPLY", "NO_REPL", " no reply ", "(NO REPLY)"} {
		pc := &PartCollector{}
		pc.Observe(StreamEvent{Kind: "text", Text: s})
		if got := pc.Parts(); len(got) != 0 {
			t.Errorf("sentinel %q should yield no parts, got %+v", s, got)
		}
	}
	// A real reply is still kept.
	pc := &PartCollector{}
	pc.Observe(StreamEvent{Kind: "text", Text: "actually here is the answer"})
	if got := pc.Parts(); len(got) != 1 || got[0].Content != "actually here is the answer" {
		t.Errorf("real reply must be kept, got %+v", got)
	}
}
