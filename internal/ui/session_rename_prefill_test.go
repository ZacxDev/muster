package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/sessionname"
)

// TestTheSwitcherRenameDialogPrefillsAManualNameAsTyped: the session's ⋯
// "Rename…" dialog is prefilled with a MANUAL name as it was typed (markdown
// and all), not as it draws; an auto name is prefilled as it draws.
func TestTheSwitcherRenameDialogPrefillsAManualNameAsTyped(t *testing.T) {
	cur := SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@2"}}
	dialog := func(m MirrorCardView) string {
		sw := NewSwitcher(nil, []MirrorCardView{m}, nil, cur)
		if sw.pin == nil {
			t.Fatal("no pin for the page's own session")
		}
		return renderString(t, switcherRenameDialog(sw.pin))
	}
	manual := MirrorCardView{Host: "tower", Session: "w", Window: "@2", Online: true, ManualName: "**Pay** run",
		SessionName: sessionname.Name{Text: "Pay run", Source: sessionname.SourceManual}}
	if got := dialog(manual); !strings.Contains(got, `value="**Pay** run"`) {
		t.Errorf("a manual name is not prefilled as typed:\n%s", got)
	}
	auto := MirrorCardView{Host: "tower", Session: "w", Window: "@2", Online: true, ManualName: "",
		SessionName: sessionname.Name{Text: "Billing migration", Source: sessionname.SourceAuto}}
	if got := dialog(auto); !strings.Contains(got, `value="Billing migration"`) {
		t.Errorf("an auto name is not prefilled as it draws:\n%s", got)
	}
}
