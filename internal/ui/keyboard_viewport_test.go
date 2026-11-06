package ui

import (
	"regexp"
	"strings"
	"testing"
)

// squash is src with every run of white space removed, so a respaced copy of
// a reading matches the same as the original.
func squash(src string) string { return strings.Join(strings.Fields(src), "") }

// ownReadingRE is a keyboard reading of a script's own, in squashed source:
// the visual viewport's bottom (height plus or minus offsetTop), the pinch rule
// (scale compared or subtracted), or the view switch's frame check.
var ownReadingRE = regexp.MustCompile(`\.height[-+]\w+\.offsetTop|\.scale-1\b|\.scale[<>]|\.offsetTop[-+]|frameElement\.hasAttribute\(`)

// TestOneKeyboardMeasure (one rule, one place): the three scripts that measure
// a soft keyboard — the Rendered view's pinned composer, the bar's keyboard
// mark and the dock — each splice keyboardViewportJS exactly once and carry no
// reading of their own (the visual viewport's bottom, the pinch rule, the
// frame check). The composer and the bar measure kbWin() (the TOP window
// inside the view switch's frame); the dock measures its OWN window, the top
// document it is always drawn in (switcher-dock.spec.ts 13k pins that from a
// frame too).
func TestOneKeyboardMeasure(t *testing.T) {
	// The bar's keyboard block (switcherKeyboardJS, spliced once into
	// switcherJS, TestOneKeyboardMeasure's second loop checks that); the rest
	// of switcherJS places its menu by the visual viewport, which is not a
	// keyboard reading.
	composer := squash(mustStripJSComments(t, jsSource(t, composerPinScript())))
	bar, dock := squash(mustStripJSComments(t, switcherKeyboardJS)), squash(mustStripJSComments(t, dockJS))
	helper := squash(keyboardViewportJS)
	// POSITIVE CONTROL: the pattern finds the helper's own readings.
	if n := len(ownReadingRE.FindAllString(helper, -1)); n < 3 {
		t.Fatalf("ownReadingRE finds %d readings in the helper itself, want 3 or more: it cannot see one", n)
	}
	for name, src := range map[string]string{"composerPinScript": composer, "switcherKeyboardJS": bar, "dockJS": dock} {
		if n := strings.Count(src, helper); n != 1 {
			t.Errorf("%s splices keyboardViewportJS %d times, want 1", name, n)
			continue
		}
		if own := ownReadingRE.FindString(strings.Replace(src, helper, "", 1)); own != "" {
			t.Errorf("%s still carries its own keyboard reading: %q", name, own)
		}
	}
	if strings.Count(switcherJS, switcherKeyboardJS) != 1 {
		t.Error("switcherJS does not splice switcherKeyboardJS once")
	}
	for name, src := range map[string]string{"composerPinScript": composer, "switcherKeyboardJS": bar} {
		if !strings.Contains(src, "vw=kbWin()") {
			t.Errorf("%s does not measure kbWin() (the top window inside the frame)", name)
		}
	}
	rest := strings.Replace(dock, helper, "", 1)
	if !strings.Contains(rest, "kbMeasure(window)") || strings.Contains(rest, "kbWin()") {
		t.Error("the dock does not measure its own window")
	}
}
