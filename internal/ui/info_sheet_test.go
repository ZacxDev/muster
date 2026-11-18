package ui

import (
	"strings"
	"testing"
)

// TestTheInfoSheetIsAKeyboardReachableNamedDialogWithCopy: the ⓘ is a real
// <button> (focusable, Enter/Space) that toggles a native popover by id, named
// for a screen reader; the sheet is a dialog labelled by its title, holds the
// exact text verbatim and a Copy button carrying exactly that text, and closes
// from its own Close button. Pinned literally.
func TestTheInfoSheetIsAKeyboardReachableNamedDialogWithCopy(t *testing.T) {
	out := renderString(t, SessionPlaceSheet("place-ask-7", "Billing migration", "atlas", "tmux fern:1 @ atlas").Node())
	for _, want := range []string{
		`<button type="button" id="place-ask-7-open" popovertarget="place-ask-7" aria-haspopup="dialog" aria-controls="place-ask-7" aria-label="Where Billing migration runs" title="Where Billing migration runs" data-info-open="place-ask-7"`,
		`<div id="place-ask-7" popover="auto" role="dialog" aria-labelledby="place-ask-7-title" data-info-sheet=""`,
		`<p id="place-ask-7-title" class="flex-1 text-sm font-semibold text-fg">Where it runs</p>`,
		`<p class="text-sm text-fg2">This session runs in a tmux window on atlas. To open it in a terminal on that computer, use this address:</p>`,
		`data-info-code="">tmux fern:1 @ atlas</code>`,
		// Copy copies the sheet's <code> (cgCopy reads it): the code is never
		// drawn a second time in an attribute.
		`<button type="button" data-info-copy="" onclick="window.cgCopy &amp;&amp; window.cgCopy(this)"`,
		`popovertarget="place-ask-7" popovertargetaction="hide" aria-label="Close"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the sheet lacks %s in:\n%s", want, out)
		}
	}
	// The trigger is not inside the sheet, and the sheet is a sibling after it.
	if i, j := strings.Index(out, `id="place-ask-7-open"`), strings.Index(out, `id="place-ask-7" popover`); i < 0 || j < i {
		t.Errorf("the trigger does not come before its sheet:\n%s", out)
	}
	// Its text is escaped.
	esc := renderString(t, SessionPlaceSheet("p", "", "a<b", `tmux x"y:1 @ a<b`).Node())
	if strings.Contains(esc, "a<b") || !strings.Contains(esc, `data-info-code="">tmux x&#34;y:1 @ a&lt;b</code>`) || strings.Contains(esc, "data-copy-text") {
		t.Errorf("the sheet's text is not escaped:\n%s", esc)
	}
}

// TestAWithheldInfoSheetSaysAskYourAdminAndCarriesNoCode: a SETUP detail
// shown to someone who is not an admin — the sheet says to ask the admin, and
// the code is absent from the markup (not hidden), Copy included.
func TestAWithheldInfoSheetSaysAskYourAdminAndCarriesNoCode(t *testing.T) {
	s := InfoSheet{ID: "setup", Label: "Setup details", Heading: "Setup", Lead: "This needs a setting on the server.", Code: "MUSTER_EXAMPLE_FLAG=1"}
	open := renderString(t, s.Node())
	if !strings.Contains(open, `data-info-code="">MUSTER_EXAMPLE_FLAG=1</code>`) {
		t.Fatalf("CONTROL: the admin's sheet lacks its code:\n%s", open)
	}
	s.Withheld = true
	out := renderString(t, s.Node())
	if !strings.Contains(out, `<p class="text-sm text-fg" data-info-withheld="">Ask your admin for these details.</p>`) {
		t.Errorf("the withheld sheet does not say to ask the admin:\n%s", out)
	}
	if strings.Contains(out, "MUSTER_EXAMPLE_FLAG") || strings.Contains(out, "data-info-copy") {
		t.Errorf("the withheld sheet still carries the code or Copy:\n%s", out)
	}
}

// TestAQuestionCardIsHeadedQuestionNotTheToolName: the card's head says what
// it is in words ("Question"), never Claude Code's tool identifier
// ("AskUserQuestion"). REGRESSION TEST for the jargon audit's U-04: red at
// origin/main, whose head drew the identifier in monospace.
func TestAQuestionCardIsHeadedQuestionNotTheToolName(t *testing.T) {
	out := renderString(t, askCardHead("asking…"))
	if !strings.Contains(out, `<span data-ask-kind="" class="min-w-0 truncate text-st-review-fg">Question</span>`) {
		t.Errorf("the head does not say Question:\n%s", out)
	}
	if strings.Contains(out, "AskUserQuestion") || strings.Contains(out, "font-mono") {
		t.Errorf("the head still spells the tool's identifier:\n%s", out)
	}
}
