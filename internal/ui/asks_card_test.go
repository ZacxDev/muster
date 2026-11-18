package ui

import (
	"strings"
	"testing"
)

// TestARefusalReleasesTheHeldRefresh (audit of #787): the live refresh holds
// while a card carries AsksCollapsingAttr and runs on AsksCollapsedEvent. A
// refusal ends the collapse early (refused() clears the attribute and the
// collapse's own end then never fires), so refused() must dispatch the event
// itself, or the held refresh waits for the next queue change. A SPELLED
// guard on the script (the e2e could not isolate the timing: the queue fires
// a second event that runs the refresh anyway).
func TestARefusalReleasesTheHeldRefresh(t *testing.T) {
	body := asksFeedJS[strings.Index(asksFeedJS, "function refused(id, x) {"):]
	body = body[:strings.Index(body, "\n  function ")]
	clear := strings.Index(body, "c.removeAttribute('"+AsksCollapsingAttr+"')")
	fire := strings.Index(body, "new Event('"+AsksCollapsedEvent+"'")
	if clear < 0 || fire < clear {
		t.Fatalf("refused() does not dispatch %s after clearing %s:\n%s", AsksCollapsedEvent, AsksCollapsingAttr, body)
	}
}
