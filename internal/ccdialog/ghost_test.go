package ccdialog

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func ansiFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestInputBoxReadsAGhostSuggestionAsEmpty: Claude Code's ghost suggestion —
// a suggested next prompt drawn DIM in an empty box — is not text in the box,
// so a paste over it is not refused. Both escape forms the real CLI's panes
// show (one dim run; a dim run per word, plain spaces between, the last run
// left open and closed by the \e[0m that starts the rule row below).
// The other direction: the same words typed (drawn plain) are text, and a box
// holding anything not dim is read whole, dim characters included.
func TestInputBoxReadsAGhostSuggestionAsEmpty(t *testing.T) {
	for _, c := range []struct {
		name, text string
	}{
		{"pane_idle_ghost_2.1.289.ansi", ""},
		{"pane_idle_ghost_wordruns_2.1.289.ansi", ""},
		{"pane_idle_typed_2.1.289.ansi", "run the tests again"},
		// tmux states a style where it CHANGES, so a dim run left open at the
		// end of a row is still dim on the next, which starts with no escape.
		{"pane_idle_ghost_tworows_2.1.289.ansi", ""},
		// A dim row ABOVE the box ends its dim — by a full reset (0), or by
		// normal intensity (22) alone — and the typed text below is not dim.
		{"pane_typed_dimabove_reset0_2.1.289.ansi", "run the tests again"},
		{"pane_typed_dimabove_reset22_2.1.289.ansi", "run the tests again"},
	} {
		text, found := InputBox(ansiFixture(t, c.name))
		if !found || text != c.text {
			t.Errorf("%s: InputBox = %q, %t; want %q, true", c.name, text, found, c.text)
		}
	}
	ghost := ansiFixture(t, "pane_idle_ghost_2.1.289.ansi")
	mixed := strings.Replace(ghost, "❯ \x1b[2m", "❯ typed \x1b[2m", 1)
	if text, found := InputBox(mixed); !found || text != "typed run the tests again" {
		t.Errorf("a box holding typed text before dim text: InputBox = %q, %t; want the whole box as text", text, found)
	}
	if text, _ := InputBox(stripSGR(ghost)); text != "run the tests again" {
		t.Errorf("a suggestion captured WITHOUT styles: InputBox = %q; want it read as text (no style to tell)", text)
	}
}

// TestOnScreenReadsAStyledCapture: with capture-pane -e the screen carries
// escape sequences; a dialog is still recognised through them and an idle
// prompt (a ghost suggestion in its box) is still no dialog.
func TestOnScreenReadsAStyledCapture(t *testing.T) {
	var styled []string
	for _, l := range strings.Split(fixture(t, "pane_permission_2.1.296.txt"), "\n") {
		styled = append(styled, "\x1b[38;5;246m"+l+"\x1b[39m")
	}
	perm := strings.Join(styled, "\n")
	if what, shown := OnScreen(perm); !shown {
		t.Errorf("a styled capture of the permission prompt was not read as a dialog (%q)", what)
	}
	if _, found := InputBox(perm); found {
		t.Error("a styled capture of the permission prompt was read as an input box")
	}
	// The AskUserQuestion picker is recognised by askdrive.Parse, which reads
	// plain text: OnScreen hands it the capture with its styles stripped.
	for _, n := range []string{"single-q.txt", "multi-review.txt"} {
		b, err := os.ReadFile("../askdrive/testdata/" + n)
		if err != nil {
			t.Fatal(err)
		}
		if _, shown := OnScreen(string(b)); !shown {
			t.Fatalf("%s: the plain capture is not read as the picker (the control)", n)
		}
		var rows []string
		for _, l := range strings.Split(string(b), "\n") {
			rows = append(rows, "\x1b[38;5;246m"+l+"\x1b[39m")
		}
		if what, shown := OnScreen(strings.Join(rows, "\n")); !shown || what != "the AskUserQuestion picker" {
			t.Errorf("%s: a styled capture of the picker was read as %q, %t; want the AskUserQuestion picker", n, what, shown)
		}
	}
	for _, n := range []string{"pane_idle_ghost_2.1.289.ansi", "pane_idle_ghost_wordruns_2.1.289.ansi", "pane_idle_typed_2.1.289.ansi"} {
		if what, shown := OnScreen(ansiFixture(t, n)); shown {
			t.Errorf("%s: read as a dialog (%q)", n, what)
		}
	}
}

// stripSGR is capture with its escape sequences removed (what capture-pane
// prints without -e).
func stripSGR(capture string) string {
	return regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(capture, "")
}
