package ui

import (
	"strings"
	"testing"
)

// TestTheIdChipIsLabelledPlainText: the ONE spelling of each id ("Ask #859",
// "Draft #484"), and the chip as the server draws it — whole: plain text in a
// <span> (the id script makes the button), its label the copied text. No id,
// no chip.
func TestTheIdChipIsLabelledPlainText(t *testing.T) {
	if AskIDLabel(859) != "Ask #859" || DraftIDLabel(484) != "Draft #484" {
		t.Fatalf("labels %q %q", AskIDLabel(859), DraftIDLabel(484))
	}
	const cls = "press inline-flex min-h-[44px] shrink-0 items-center whitespace-nowrap rounded-lg px-2 text-xs font-medium tabular-nums text-fg2 transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	if got, want := AskIDChipHTML(859), `<span class="`+cls+`" data-id-copy="Ask #859" data-id-kind="ask" data-id="859">Ask #859</span>`; got != want {
		t.Errorf("ask chip\n got %s\nwant %s", got, want)
	}
	if got, want := DraftIDChipHTML(484), `<span class="`+cls+`" data-id-copy="Draft #484" data-id-kind="draft" data-id="484">Draft #484</span>`; got != want {
		t.Errorf("draft chip\n got %s\nwant %s", got, want)
	}
	if AskIDChipHTML(0) != "" || DraftIDChipHTML(-1) != "" {
		t.Errorf("a chip for no id")
	}
	// The script makes a named button of it, and copies the label.
	for _, want := range []string{"document.createElement('button')", "b.type = 'button'", `'Copy ' + label`, "cgCopyText(label)", `'Copied'`, "MutationObserver",
		// The copy is announced in a polite live region; the button's name stays.
		"setAttribute('aria-live', 'polite')", "data-id-copy-status", `r.textContent = 'Copied ' + label`} {
		if !strings.Contains(idCopyJS, want) {
			t.Errorf("the id script lacks %q", want)
		}
	}
	for _, unwanted := range []string{`b.setAttribute('aria-label', 'Copied')`} {
		if strings.Contains(idCopyJS, unwanted) {
			t.Errorf("the id script still renames the focused button: %q", unwanted)
		}
	}
}

// TestACardTopRowHoldsTheToggleAndTheChipSideBySide: the chip is never inside
// the head; with no chip the head is drawn as it was.
func TestACardTopRowHoldsTheToggleAndTheChipSideBySide(t *testing.T) {
	if got := AsksCardTopHTML("<button>h</button>", ""); got != "<button>h</button>" {
		t.Errorf("no chip: %q", got)
	}
	if got, want := AsksCardTopHTML("<button>h</button>", "<span>c</span>"), `<div class="`+AsksCardTopClass+`" data-asks-top><button>h</button><span>c</span></div>`; got != want {
		t.Errorf("with a chip: %q", got)
	}
}
