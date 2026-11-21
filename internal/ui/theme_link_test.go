package ui

import (
	"strings"
	"testing"
)

// TestTheThemeIsLinkedOnlyWhenItIsNotTheDefault: a document head links the
// server's theme stylesheet at its versioned URL when Brand names one, and
// links nothing for the default theme — every document, through docHead.
// The fixture ETag (9f8e7d6c5b4a3921) is no real theme's.
func TestTheThemeIsLinkedOnlyWhenItIsNotTheDefault(t *testing.T) {
	if got := DocHeadHTML(Brand{}); strings.Contains(got, ThemeCSSPath) {
		t.Errorf("the default theme links a theme stylesheet: %s", got)
	}
	want := `<link rel="stylesheet" data-theme-css="" href="/ui-kit/v1/theme.css?v=9f8e7d6c5b4a3921">`
	if got := DocHeadHTML(Brand{ThemeETag: "9f8e7d6c5b4a3921"}); strings.Count(got, want) != 1 {
		t.Errorf("a set theme is not linked once as %s:\n%s", want, got)
	}
	docs := shellDocuments(t, featuresAllOn)
	if len(docs) == 0 {
		t.Fatal("no documents rendered")
	}
	for name, html := range docs {
		if strings.Contains(html, ThemeCSSPath) {
			t.Errorf("%s links a theme with no theme set", name)
		}
	}
}
