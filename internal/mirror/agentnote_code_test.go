package mirror

import "testing"

// TestTheNoViewCodeIsKeptOnlyWhereItMeansSomething: NormalizeWindows keeps an
// AgentNoteCode only on a window that is not Claude and names no runtime, and
// only a known code — WITH or WITHOUT its words (the list-wide note bound may
// drop those) — and Equal tells two windows apart by it. ClaudeUnmatched is
// true for no_transcript and no_env only.
func TestTheNoViewCodeIsKeptOnlyWhereItMeansSomething(t *testing.T) {
	for _, c := range []struct {
		name string
		w    Window
		want string
	}{
		{"a Claude pane with no view", Window{Session: "s", ID: "@1", AgentNote: "x", AgentNoteCode: AgentNoteNotYet}, AgentNoteNotYet},
		{"its words dropped, its code kept", Window{Session: "s", ID: "@1", AgentNoteCode: AgentNoteNoTranscript}, AgentNoteNoTranscript},
		{"an unknown code", Window{Session: "s", ID: "@1", AgentNote: "x", AgentNoteCode: "later_reason"}, ""},
		{"on a Claude window", Window{Session: "s", ID: "@1", Claude: true, AgentNoteCode: AgentNoteNoEnv}, ""},
		{"on a runtime's window", Window{Session: "s", ID: "@1", Runtime: RuntimeOpenCode, AgentNote: "x", AgentNoteCode: AgentNoteNoEnv}, ""},
	} {
		got := NormalizeWindows([]Window{c.w}, 0)
		if len(got) != 1 || got[0].AgentNoteCode != c.want {
			t.Errorf("%s: %+v, want code %q", c.name, got, c.want)
		}
	}
	a := Window{Session: "s", ID: "@1", AgentNote: "x", AgentNoteCode: AgentNoteNotYet}
	b := a
	b.AgentNoteCode = AgentNoteNoTranscript
	if a.Equal(b) {
		t.Error("Equal ignores AgentNoteCode")
	}
	for code, want := range map[string]bool{AgentNoteNoTranscript: true, AgentNoteNoEnv: true, AgentNoteNotYet: false, AgentNoteNoProcs: false, "": false} {
		if got := (Window{AgentNoteCode: code}).ClaudeUnmatched(); got != want {
			t.Errorf("ClaudeUnmatched(%q) = %v, want %v", code, got, want)
		}
	}
}
