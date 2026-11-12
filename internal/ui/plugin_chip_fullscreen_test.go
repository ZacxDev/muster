package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestThePreviewPanelsExitClearsTheSafeAreaInset: full screen's Exit button
// sits below the top safe-area inset (a phone's status bar or notch). The
// rest of full screen (which control shows, the header hidden, the panel and
// frame the viewport) is e2e/tests/plugin-chip.spec.ts's; this is the one
// part headless Chromium cannot reach, since its safe-area insets are 0.
func TestThePreviewPanelsExitClearsTheSafeAreaInset(t *testing.T) {
	page := renderRaw(t, previewPanel(&PluginChips{MintPath: "/plugin-handoffs", Host: "ws", Session: "main", Window: "@1",
		Chips: []PluginChip{{ID: 1, Label: "Example"}}, Previews: mirror.WindowPreviews{Offered: 1}}))
	exit := regexp.MustCompile(`<button[^>]*data-preview-exit-full=""[^>]*>`).FindString(page)
	if exit == "" {
		t.Fatalf("no Exit button in the panel:\n%s", page)
	}
	m := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(exit)
	var classes []string
	if m != nil {
		classes = strings.Fields(m[1])
	}
	const want = "top-[max(0.5rem,env(safe-area-inset-top))]"
	for _, c := range classes {
		if c == want {
			return
		}
	}
	t.Errorf("the Exit button's classes %v lack %s", classes, want)
}
