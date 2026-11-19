package api

import (
	"html"
	"regexp"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/authz"
)

// featureSheetSentence is one of a feature ⓘ sheet's own sentences: its
// reason (why it is in its state) or its note (what turning it on takes).
var featureSheetSentence = regexp.MustCompile(`<p data-feature-(reason|note) [^>]*>([^<]*)</p>`)

// TestAFeatureSheetsReasonAndNoteStartWithACapital: the v0.250.0 phone check
// (F4) found admin-only ⓘ sentences starting lower-case ("the VAPID keys are
// unset"): a feature's reason and note are fragments the server assembles
// (cmd/muster-server/features.go), and the sheet drew them as written. They
// are now drawn through ui.SentenceStart. The fixture's reasons and note
// start lower-case the way the real catalog's do ("needs …", "mirrored …",
// "on each host, …"); every sentence is pinned whole. Red at origin/main,
// which drew "needs MUSTER_EXAMPLE_OPENROUTER_KEY".
func TestAFeatureSheetsReasonAndNoteStartWithACapital(t *testing.T) {
	s, _ := featureServer(t, Extensions{}, plainFeatureSet(t))
	page := featuresPageAs(t, s, authz.RoleAdmin)
	got := map[string]bool{}
	for _, m := range featureSheetSentence.FindAllStringSubmatch(page, -1) {
		text := html.UnescapeString(m[2])
		got[m[1]+": "+text] = true
		if r, _ := utf8.DecodeRuneInString(text); unicode.IsLower(r) {
			t.Errorf("a feature sheet's %s starts lower-case: %q", m[1], text)
		}
	}
	// CONTROL: the sweep sees every reason and the note (7 reasons: every
	// feature but the one that is on; 1 note).
	if len(got) != 8 {
		t.Fatalf("CONTROL: the sweep found %d sentence(s), want 8: %v", len(got), got)
	}
	for _, want := range []string{
		"reason: Needs MUSTER_EXAMPLE_OPENROUTER_KEY",
		"reason: Mirrored sessions are off (MUSTER_EXAMPLE_MIRROR)",
		"note: On each host, run muster-mirror with -mcp-listen 127.0.0.1:<port>.",
		// Already capitalised: kept as written.
		"reason: MUSTER_EXAMPLE_STT_TOKEN is unset",
	} {
		if !got[want] {
			t.Errorf("no %q among the sheets' sentences: %v", want, got)
		}
	}
}
