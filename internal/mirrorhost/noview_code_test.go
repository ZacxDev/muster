package mirrorhost

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestEveryNoViewLineHasItsCodeOnTheWire: each NoView* line maps to exactly
// its own reason code (noViewCode), "" for no line, and a Claude Code pane
// with no rendered view is SENT with both — its words and its code — through
// WireWindows and NormalizeWindows.
func TestEveryNoViewLineHasItsCodeOnTheWire(t *testing.T) {
	want := map[string]string{
		NoViewNotYet:       mirror.AgentNoteNotYet,
		NoViewNoTranscript: mirror.AgentNoteNoTranscript,
		NoViewNoEnv:        mirror.AgentNoteNoEnv,
		NoViewNoProcs:      mirror.AgentNoteNoProcs,
		"":                 "",
	}
	for line, code := range want {
		if got := noViewCode(line); got != code {
			t.Errorf("noViewCode(%q) = %q, want %q", line, got, code)
		}
		if line == "" {
			continue
		}
		w := WindowInfo{Session: "main", ID: "@1", Index: 1, Pane: "%1", PanePID: 42, Command: "claude", NoView: line}
		got := WireWindows([]WindowInfo{w}, nil, true, time.Unix(1_760_000_000, 0))
		if len(got) != 1 || got[0].AgentNote != line || got[0].AgentNoteCode != code {
			t.Errorf("on the wire for %q: %+v, want note and code %q", line, got, code)
		}
	}
}
