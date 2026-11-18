package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestAnAppScreenIsAPhoneScreen: NavDocument with NavApp (the ask page) is one
// dynamic-viewport column — top bar, a content area that scrolls by itself and
// never sideways, the composer as its LAST row inside the bottom safe-area
// inset — under a viewport whose keyboard resizes the content (so the
// composer sits above it); it keeps exactly one side nav and one hamburger,
// as every signed-in page does. The control is the same document without
// NavApp: the column layout, the default viewport, no composer row.
func TestAnAppScreenIsAPhoneScreen(t *testing.T) {
	var b bytes.Buffer
	if err := RenderNavDocument(&b, sampleAppScreen()); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	for _, want := range []string{
		`content="width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content"`,
		`data-app-screen="" class="flex h-dvh flex-col lg:pl-72"`,
		`id="app-main" class="min-h-0 flex-1 overflow-y-auto overflow-x-hidden overscroll-contain"`,
		`id="app-compose" class="shrink-0 border-t border-line bg-bg"`,
		`pb-[max(0.75rem,env(safe-area-inset-bottom))]`,
		`pt-[env(safe-area-inset-top)]`,
		`href="/attention" aria-label="Back to Asks" id="app-back"`,
		`>worker-7 is asking</h1>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the app screen lacks %q", want)
		}
	}
	if strings.Count(page, `id="sidebar-root"`) != 1 || strings.Count(page, `id="sidebar-open"`) != 1 {
		t.Error("the app screen does not carry exactly one side nav and one hamburger")
	}
	// The composer is the column's last row: nothing after it inside the column.
	col := page[strings.Index(page, `data-app-screen`):]
	if i, j := strings.Index(col, `id="app-main"`), strings.Index(col, `id="app-compose"`); i < 0 || j < i {
		t.Error("the composer is not after the content")
	}

	b.Reset()
	v := sampleAppScreen()
	v.App = nil
	if err := RenderNavDocument(&b, v); err != nil {
		t.Fatal(err)
	}
	if c := b.String(); strings.Contains(c, "interactive-widget") || strings.Contains(c, "data-app-screen") || strings.Contains(c, `id="app-compose"`) {
		t.Error("CONTROL: the column layout carries the app screen's markup")
	}
}
