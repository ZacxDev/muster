package mirror_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheMovedAndUnnamedRefusalsSayWhatToDo, pinned whole: a message written
// for another session is written again while viewing the one shown now (a
// Retry would name the old one again); one from an older page or row needs
// the page reloaded first.
func TestTheMovedAndUnnamedRefusalsSayWhatToDo(t *testing.T) {
	for got, want := range map[string]string{
		mirror.PasteRefusedOpenCodeMovedText: "Not sent: this opencode pane shows another session now than the one this message was written for, " +
			"so nothing was sent. To send it there, write it again while viewing the session shown now.",
		mirror.PasteRefusedOpenCodeUnnamedText: "Not sent: muster can't tell which opencode session this message was written for " +
			"(it was queued or typed on an older page), so nothing was sent. Reload the page, then write it again.",
	} {
		if got != want {
			t.Errorf("the refusal reads\n  %q\nwant\n  %q", got, want)
		}
	}
}

// TestAnAnswerOfOnesOwnIsBoundInCharacters: what the card's box takes —
// askq.MaxFreeText characters, in any script — the form takes; one more is
// refused. A three-byte character counts once.
//
// 🔴 REGRESSION TEST. At f726ef03 the bound was in bytes: about a third of a
// full box of CJK text was refused by the hub after the box took it.
func TestAnAnswerOfOnesOwnIsBoundInCharacters(t *testing.T) {
	f := fixtureForm()
	pick := func(other string) []mirror.OpenCodePick {
		return []mirror.OpenCodePick{{Key: "q0", Other: other}, {Key: "q1", Picks: []int{0}}}
	}
	if err := mirror.CheckOpenCodePicks(f, pick(strings.Repeat("字", askq.MaxFreeText))); err != nil {
		t.Fatalf("a full box of CJK text: %v", err)
	}
	if err := mirror.CheckOpenCodePicks(f, pick(strings.Repeat("字", askq.MaxFreeText+1))); !errors.Is(err, mirror.ErrOpenCodeFormInvalid) ||
		!strings.Contains(err.Error(), "characters") {
		t.Fatalf("one character over: %v", err)
	}
}
