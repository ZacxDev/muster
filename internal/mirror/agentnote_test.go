package mirror_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAnAgentNoteIsKeptForAWindowWithNoViewAndBounded: the host's line about
// an opencode window reaches the list (and the card) as it was written, with
// control characters made spaces; so does a window of no wire runtime that is
// NOT Claude (the host's reason a Claude Code pane there has no rendered view,
// mirrorhost's NoView* lines); a Claude window keeps none; a note is cut to
// MaxAgentNote bytes on a rune boundary; and at most MaxAgentNoteWindows
// windows of a list carry one, in the list's order.
func TestAnAgentNoteIsKeptForAWindowWithNoViewAndBounded(t *testing.T) {
	long := strings.Repeat("é", mirror.MaxAgentNote) // 2 bytes a rune
	in := []mirror.Window{
		{Session: "a", ID: "@1", Runtime: mirror.RuntimeOpenCode, AgentNote: "opencode is not paired on this host:\nrun muster-mirror opencode-pair"},
		{Session: "a", ID: "@2", Command: "claude", AgentNote: "Claude Code has not written this conversation yet."},
		{Session: "a", ID: "@3", Claude: true, ClaudeSession: "abc", AgentNote: "nor has Claude Code"},
		{Session: "a", ID: "@4", Runtime: mirror.RuntimeOpenCode, AgentNote: long},
	}
	for i := 0; i < mirror.MaxAgentNoteWindows+3; i++ {
		in = append(in, mirror.Window{Session: "b", ID: "@" + strconv.Itoa(10+i), Runtime: mirror.RuntimeOpenCode, AgentNote: "note"})
	}
	got := mirror.NormalizeWindows(in, 0)
	if got[0].AgentNote != "opencode is not paired on this host: run muster-mirror opencode-pair" {
		t.Errorf("the opencode note: %q", got[0].AgentNote)
	}
	if got[1].AgentNote != "Claude Code has not written this conversation yet." {
		t.Errorf("a terminal-only window lost its reason: %q", got[1].AgentNote)
	}
	if got[2].AgentNote != "" {
		t.Errorf("a Claude window kept a note: %q", got[2].AgentNote)
	}
	if n := len(got[3].AgentNote); n > mirror.MaxAgentNote || n < mirror.MaxAgentNote-1 || !strings.HasPrefix(long, got[3].AgentNote) {
		t.Errorf("a long note is %d bytes (bound %d): %q", n, mirror.MaxAgentNote, got[3].AgentNote)
	}
	notes := 0
	for _, w := range got {
		if w.AgentNote != "" {
			notes++
		}
	}
	if notes != mirror.MaxAgentNoteWindows {
		t.Errorf("%d windows carry a note, want %d", notes, mirror.MaxAgentNoteWindows)
	}
	// The SessionView carries it to the card.
	h := mirror.HostView{Host: "h", Online: true, Windows: got[:1]}
	if e := h.Entries(); len(e) != 1 || e[0].AgentNote != got[0].AgentNote {
		t.Errorf("entries %+v", e)
	}
}

// TestTheTerminalOnlyReasonSaysWhy: an entry with no rendered view says why
// in one line, in order — a host too old for rendered views, the host's own
// reason, a runtime with no view yet, else no Claude Code found — and an entry
// with a view, or an offline one, says nothing. Each case's expected line is
// distinct, so no branch can stand in for another. Red at origin/main:
// TerminalOnlyReason did not exist and a terminal-only window said nothing.
func TestTheTerminalOnlyReasonSaysWhy(t *testing.T) {
	const why = "Claude Code has not written this conversation yet. The Rendered view appears after its first message."
	for _, c := range []struct {
		name string
		e    mirror.SessionView
		want string
	}{
		{"has a view", mirror.SessionView{Online: true, Rendered: true, Claude: true}, ""},
		{"offline", mirror.SessionView{Online: false, Rendered: true, Command: "zsh"}, ""},
		{"old host", mirror.SessionView{Online: true, Rendered: false, Command: "claude"},
			"This host's muster-mirror is too old to send the Rendered view. Update it to read Claude Code as a chat."},
		{"host's reason", mirror.SessionView{Online: true, Rendered: true, Command: "claude", AgentNote: why}, why},
		{"runtime, no view", mirror.SessionView{Online: true, Rendered: true, Runtime: mirror.RuntimeHermes, Command: "hermes"},
			"This Hermes window has no conversation muster can show yet."},
		{"no Claude Code", mirror.SessionView{Online: true, Rendered: true, Command: "zsh"},
			"muster found no Claude Code conversation in this window."},
	} {
		if got := c.e.TerminalOnlyReason(); got != c.want {
			t.Errorf("%s: TerminalOnlyReason = %q, want %q", c.name, got, c.want)
		}
	}
}
