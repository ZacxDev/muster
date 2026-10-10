package ui

import (
	"regexp"
	"strings"
	"testing"
)

// submitButtonRe captures the opening tag of every submit button in a fragment.
var submitButtonRe = regexp.MustCompile(`<button type="submit"[^>]*>`)

// TestEverySheetKeepsItsPrimaryActionInView pins the bottom sheets' submit
// buttons to the bottom of the sheet's scroll area.
//
// 🔴 WHY: each sheet scrolls inside an 85dvh cap (noteModalShell). Opening
// "Advanced" on a phone pushes the submit button below the fold, so the one
// action the sheet exists for was a scroll away while the keyboard covered the
// rest. `sticky bottom-0` keeps it on screen; the e2e layout spec checks the
// behaviour (the button stays inside the viewport with Advanced open), this
// checks every sheet carries the class so a new sheet cannot forget it.
func TestEverySheetKeepsItsPrimaryActionInView(t *testing.T) {
	sheets := map[string]string{
		"new task":      renderString(t, NotesModalBody([]string{"example"}, false)),
		"edit task":     renderString(t, NotesEditModalBody(sampleNoteEditView())),
		"dispatch":      renderString(t, DispatchModalBody()),
		"dispatch task": renderString(t, DispatchModalTaskBody(TaskDispatchView{NoteID: 1, Label: "x"})),
	}
	for name, html := range sheets {
		if len(submitButtonRe.FindAllString(html, -1)) == 0 {
			t.Errorf("%s: no submit button found; the fixture no longer renders the sheet", name)
			continue
		}
		// The sticky element is either the submit button itself or the action
		// row holding it; a row is sticky only if its own content carries the
		// submit button (a sticky element elsewhere in the sheet keeps nothing
		// useful in view).
		var stickies []string
		for _, tag := range regexp.MustCompile(`<(button|div)[^>]*class="[^"]*"[^>]*>`).FindAllString(html, -1) {
			cls := classOf(tag)
			if hasClass(cls, "sticky") && hasClass(cls, "bottom-0") {
				stickies = append(stickies, tag)
			}
		}
		if len(stickies) != 1 {
			t.Errorf("%s: want exactly one sticky action element, found %d", name, len(stickies))
			continue
		}
		tag := stickies[0]
		holds := strings.HasPrefix(tag, `<button type="submit"`) ||
			strings.Contains(between(html[strings.Index(html, tag):], tag, "</div>"), `<button type="submit"`)
		if !holds {
			t.Errorf("%s: the sticky element does not hold the submit button: %s", name, tag)
		}
	}
}

// TestTheStatusWordYieldsToTheSelectOnAPhone: the visible "Status" label is
// screen-reader-only below `sm`, so the select keeps its width at 360px.
func TestTheStatusWordYieldsToTheSelectOnAPhone(t *testing.T) {
	html := renderString(t, taskStatusSelect(1, "ready_for_review"))
	lbl := between(html, "<label", "</label>")
	if !strings.Contains(lbl, ">Status<") {
		t.Fatalf("the status label is missing: %q", lbl)
	}
	cls := classOf(lbl)
	if !hasClass(cls, "sr-only") || !hasClass(cls, "sm:not-sr-only") {
		t.Errorf("the status label should be sr-only below sm, got %q", cls)
	}
}
