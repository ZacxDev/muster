package mirrorhost

import (
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE SESSION A QUEUED CLAUDE MESSAGE WAS QUEUED FOR, HOST HALF (claudeReady):
// a Claude paste naming a session (Msg.AgentSession) is typed only while the
// pane's Claude Code session (ClaudePane.Gen), read NOW, is exactly it. This
// is the race the hub cannot close: muster's list still named the session, and
// Claude Code was restarted in the same pane before the paste arrived.
//
// 🔴 REGRESSION TEST. At origin/main 9d0fb60d claudeReady checked the pane id
// and that it still ran Claude Code, never its session: the message was typed
// into the new session.
func TestAClaudePasteForASessionThePaneNoLongerHoldsTypesNothing(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: guardWindows(mirror.StatusIdle)} // %21 holds Gen "g7"
	d.run(t, wrapped{b}, nil, true)
	pasted := func() []string { _, _, _, p, _ := b.snapshot(); return p }

	// Claude Code restarted in the same pane: same pane id, a new session.
	restarted := guardWindows(mirror.StatusIdle)
	restarted[1].Claude.Gen = "g8"
	b.set(restarted...)
	a := pasteAnswer(t, d, mirror.Msg{ID: 1, Session: "scratch2", Window: "@7", Text: "queued for g7", AgentSession: "g7"})
	if a.Refusal != mirror.PasteRefusalClaudeMoved || a.Error != mirror.ClaudeMovedText {
		t.Fatalf("a paste queued for g7 into the pane now on g8 was answered %+v; want the claude-moved refusal", a)
	}
	if got := pasted(); len(got) != 0 {
		t.Fatalf("a paste for another session typed %q", got)
	}

	// Controls on the restarted pane: a paste naming the session it holds,
	// and one naming none (a person's Send; an older muster), are typed.
	if a := pasteAnswer(t, d, mirror.Msg{ID: 2, Session: "scratch2", Window: "@7", Text: "for g8", AgentSession: "g8"}); a.Error != "" {
		t.Fatalf("a paste for the session held was refused: %s", a.Error)
	}
	if a := pasteAnswer(t, d, mirror.Msg{ID: 3, Session: "scratch2", Window: "@7", Text: "unpinned"}); a.Error != "" {
		t.Fatalf("a paste naming no session was refused: %s", a.Error)
	}
	if got := pasted(); !slices.Equal(got, []string{"%21 for g8", "%21 unpinned"}) {
		t.Fatalf("typed %q; want the two controls", got)
	}
}

// TestTheClaudeMovedRefusalIsNotAHoldingRefusal: the host's kind for it is its
// own, so the hub does not hold a queued message on it (waiting never clears
// it), and a change between the paste and the Enter is "pasted, not
// submitted", never "nothing typed".
func TestTheClaudeMovedRefusalIsNotAHoldingRefusal(t *testing.T) {
	if got := pasteRefusalOf(ErrClaudeMoved); got != mirror.PasteRefusalClaudeMoved {
		t.Fatalf("refusal kind %q, want %q", got, mirror.PasteRefusalClaudeMoved)
	}
	if got := pasteRefusalOf(&ClaudePastedError{Why: ErrClaudeMoved}); got != mirror.PasteRefusalClaudePasted {
		t.Fatalf("moved before the Enter: kind %q, want %q", got, mirror.PasteRefusalClaudePasted)
	}
}
