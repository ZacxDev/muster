package ui

import (
	"strings"
	"testing"
)

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
	composer := mustStripJSComments(t, jsSource(t, composerPinScript()))
	bar, dock := mustStripJSComments(t, switcherKeyboardJS), mustStripJSComments(t, dockJS)
	for name, src := range map[string]string{"composerPinScript": composer, "switcherKeyboardJS": bar, "dockJS": dock} {
		if n := strings.Count(src, keyboardViewportJS); n != 1 {
			t.Errorf("%s splices keyboardViewportJS %d times, want 1", name, n)
			continue
		}
		rest := strings.Replace(src, keyboardViewportJS, "", 1)
		for _, own := range []string{"vv.height + vv.offsetTop", "v.height - v.offsetTop", "scale - 1", "v.scale >", "hasAttribute('" + ViewFrameAttr + "')"} {
			if strings.Contains(rest, own) {
				t.Errorf("%s still carries its own keyboard reading: %q", name, own)
			}
		}
	}
	if strings.Count(switcherJS, switcherKeyboardJS) != 1 {
		t.Error("switcherJS does not splice switcherKeyboardJS once")
	}
	for name, src := range map[string]string{"composerPinScript": composer, "switcherKeyboardJS": bar} {
		if !strings.Contains(src, "vw = kbWin()") {
			t.Errorf("%s does not measure kbWin() (the top window inside the frame)", name)
		}
	}
	rest := strings.Replace(dock, keyboardViewportJS, "", 1)
	if !strings.Contains(rest, "kbMeasure(window)") || strings.Contains(rest, "kbWin()") {
		t.Error("the dock does not measure its own window")
	}
}
