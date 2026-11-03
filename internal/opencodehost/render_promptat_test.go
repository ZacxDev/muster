package opencodehost

import (
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// TestAPromptCarriesItsOwnTime (REGRESSION): an opencode prompt is timed by
// its message's time.created (Unix ms), as a Claude Code prompt is by its
// record — PromptAt is what lets muster tell how recent a message the person
// typed is (the chief nudge sweep reads it). Before, it was 0, so no opencode
// instruction could ever be verified as recent. Two distinct times, so a
// hardcoded value cannot pass; a prompt with no time stays 0.
func TestAPromptCarriesItsOwnTime(t *testing.T) {
	for _, at := range []float64{1793730428557, 1700000000123} {
		var m Message
		m.ID, m.Type, m.Text = "msg_fixture", "user", "ship it"
		m.Time.Created = at
		got := Render(m)
		if len(got) != 1 || got[0].Event.Kind != cctranscript.KindPrompt || got[0].Event.PromptAt != int64(at) || got[0].Event.Line != int64(at) {
			t.Fatalf("created %v: %+v", at, got)
		}
	}
	var m Message
	m.Type, m.Text = "user", "no time"
	if got := Render(m); len(got) != 1 || got[0].Event.PromptAt != 0 {
		t.Fatalf("an untimed prompt: %+v", got)
	}
	// Only prompts are timed so.
	var a Message
	a.Type, a.Time.Created, a.Time.Completed = "assistant", 1793730428557, 1793730428999
	a.Content = []Content{{Type: "text", Text: "done"}}
	for _, k := range Render(a) {
		if k.Event.PromptAt != 0 {
			t.Fatalf("a non-prompt event is timed: %+v", k)
		}
	}
}
