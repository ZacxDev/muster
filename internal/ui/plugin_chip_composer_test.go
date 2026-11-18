package ui

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// The message bar beside the open preview panel (plugin_chip.go's file
// comment). What a browser does with it — the bar live under the panel, the
// panel ending at its top, the side panel narrowing the page, full screen's
// bar button, the new-reply dot — is e2e/tests/preview-composer.spec.ts's;
// these hold the markup it relies on, and the one part headless Chromium
// cannot reach (its safe-area insets are 0).

func previewChipsFixture() *PluginChips {
	return &PluginChips{MintPath: "/plugin-handoffs", Host: "ws", Session: "main", Window: "@1",
		Chips: []PluginChip{{ID: 1, Label: "Example"}}, Previews: mirror.WindowPreviews{Offered: 1}}
}

// classesOf is the class list of the first start tag in page that matches tag.
func classesOf(t *testing.T, page, tag string) []string {
	t.Helper()
	el := regexp.MustCompile(tag).FindString(page)
	if el == "" {
		t.Fatalf("no %s in:\n%s", tag, page)
	}
	m := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(el)
	if m == nil {
		return nil
	}
	return strings.Fields(m[1])
}

// TestThePreviewsBarButtonIsAFaintThumbTargetInsideTheSafeAreas: full
// screen's message-bar button is named "Show message bar", is a 44px target,
// sits inside the bottom and right safe-area insets, is semi-transparent, and
// is drawn only while full screen.
func TestThePreviewsBarButtonIsAFaintThumbTargetInsideTheSafeAreas(t *testing.T) {
	page := renderRaw(t, previewPanel(previewChipsFixture()))
	btn := regexp.MustCompile(`<button[^>]*data-preview-bar-toggle=""[^>]*>`).FindString(page)
	if btn == "" {
		t.Fatalf("no message-bar button in the panel:\n%s", page)
	}
	if !strings.Contains(btn, `aria-label="Show message bar"`) {
		t.Errorf("the button is not named Show message bar: %s", btn)
	}
	cls := classesOf(t, page, `<button[^>]*data-preview-bar-toggle=""[^>]*>`)
	for _, want := range []string{
		"h-11", "w-11", // 44px
		"bottom-[max(0.75rem,env(safe-area-inset-bottom))]",
		"right-[max(0.75rem,env(safe-area-inset-right))]",
		"opacity-60",                                      // semi-transparent
		"hidden", "group-data-[preview-full]:inline-flex", // only while full screen
	} {
		if !slices.Contains(cls, want) {
			t.Errorf("the button's classes %v lack %s", cls, want)
		}
	}
}

// TestThePreviewSidePanelTakesItsOwnWidth: the room the page gives up beside
// the side panel is the panel's own width — one value, previewSideWidth, in
// the panel's class and in the stylesheet — and the page's layout changes
// apply only under the attributes the script sets while the panel is open.
func TestThePreviewSidePanelTakesItsOwnWidth(t *testing.T) {
	page := renderRaw(t, previewPanel(previewChipsFixture()))
	if cls := classesOf(t, page, `<aside[^>]*id="preview-panel"[^>]*>`); !slices.Contains(cls, "sm:w-["+previewSideWidth+"]") {
		t.Errorf("the panel's classes %v lack sm:w-[%s]", cls, previewSideWidth)
	}
	style := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(page)
	if style == nil {
		t.Fatalf("no stylesheet with the panel:\n%s", page)
	}
	const want = `html[data-preview-bar] #mv-compose{z-index:41}` +
		`html[data-preview-nobar] #mv-compose{visibility:hidden}` +
		`html[data-preview-side] body{padding-right:min(40rem,60vw)}`
	if style[1] != want {
		t.Errorf("the panel's stylesheet is\n%s\nwant\n%s", style[1], want)
	}
	// The bar over the panel: its z-index is above the panel's own.
	if cls := classesOf(t, page, `<aside[^>]*id="preview-panel"[^>]*>`); !slices.Contains(cls, "z-40") {
		t.Errorf("the panel's classes %v lack z-40 (the bar's z-index:41 is set against it)", cls)
	}
}

// TestTheViewsWithAPreviewKeepOneMessageBar: a Rendered and a Terminal view
// with the preview panel draw the panel AND exactly one message bar — the
// panel adds none of its own (TestEveryMessageBoxIsOneComposer).
func TestTheViewsWithAPreviewKeepOneMessageBar(t *testing.T) {
	pages := map[string]string{
		"rendered": renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1", Interactive: true, Plugins: previewChipsFixture()})),
		"terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", Plugins: previewChipsFixture(),
			Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "ws", Session: "main"}})),
	}
	for name, page := range pages {
		if n := strings.Count(page, `id="preview-panel"`); n != 1 {
			t.Errorf("%s: %d preview panels, want 1", name, n)
		}
		if n := strings.Count(page, `id="mv-compose"`); n != 1 {
			t.Errorf("%s: %d message bars, want 1", name, n)
		}
		if n := strings.Count(page, `<form id="mv-form"`); n != 1 {
			t.Errorf("%s: %d composer forms, want 1", name, n)
		}
	}
}
