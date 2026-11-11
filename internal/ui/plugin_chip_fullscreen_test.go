package ui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/web"
)

// TestThePreviewPanelOffersFullScreenInTheAppAndNewTabInABrowser: the panel
// draws BOTH header controls, and which one shows is decided by the panel's
// own marks, which only the script sets: PreviewAppAttr (the Android app's
// bridge exists) shows Full screen and hides New tab; with no mark, a browser,
// New tab shows and Full screen is hidden. PreviewFullAttr hides the header
// and shows the Exit button. The browser behaviour of both branches is
// e2e/tests/plugin-chip.spec.ts's; this pins the markup they act on.
func TestThePreviewPanelOffersFullScreenInTheAppAndNewTabInABrowser(t *testing.T) {
	page := renderRaw(t, previewPanel(&PluginChips{MintPath: "/plugin-handoffs", Host: "ws", Session: "main", Window: "@1",
		Chips: []PluginChip{{ID: 1, Label: "Example"}}, Previews: mirror.WindowPreviews{Offered: 1}}))
	tag := func(re string) string {
		t.Helper()
		m := regexp.MustCompile(re).FindString(page)
		if m == "" {
			t.Fatalf("no %s in the panel:\n%s", re, page)
		}
		return m
	}
	classes := func(tg string) map[string]bool {
		m := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(tg)
		out := map[string]bool{}
		if m != nil {
			for _, c := range strings.Fields(m[1]) {
				out[c] = true
			}
		}
		return out
	}
	// Tailwind's data-[x] variant is the attribute data-x: the class names
	// are literal (Tailwind reads them from the source), the marks constants.
	appV, fullV := strings.TrimPrefix(PreviewAppAttr, "data-"), strings.TrimPrefix(PreviewFullAttr, "data-")
	app, full := "group-data-["+appV+"]:", "group-data-["+fullV+"]:"
	for _, c := range []struct {
		what, re string
		want     []string
		not      []string
	}{
		{"the panel", `<aside id="preview-panel"[^>]*>`, []string{"group", "data-[" + fullV + "]:sm:left-0", "data-[" + fullV + "]:sm:w-full"}, nil},
		{"New tab (shown in a browser, hidden in the app)", `<a data-preview-newtab=""[^>]*>`, []string{app + "hidden"}, []string{"hidden"}},
		{"Full screen (hidden in a browser, shown in the app)", `<button[^>]*data-preview-fullscreen=""[^>]*>`, []string{"hidden", app + "inline-flex"}, nil},
		{"Exit (shown only full screen)", `<button[^>]*data-preview-exit-full=""[^>]*>`, []string{"hidden", full + "inline-flex", "absolute"}, nil},
		{"the header (hidden full screen)", `<div class="[^"]*"><h2 id="preview-panel-title"`, []string{"flex", full + "hidden"}, nil},
	} {
		got := classes(tag(c.re))
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: class %q missing (%v)", c.what, w, got)
			}
		}
		for _, n := range c.not {
			if got[n] {
				t.Errorf("%s: class %q present", c.what, n)
			}
		}
	}
	if !strings.Contains(tag(`<button[^>]*data-preview-fullscreen=""[^>]*>[^<]*`), ">Full screen") {
		t.Error("the app's control does not read Full screen")
	}
	if a := tag(`<a data-preview-newtab=""[^>]*>`); !strings.Contains(a, `target="_blank"`) {
		t.Errorf("New tab lost its target: %s", a)
	}

	// The script sets exactly these marks and reads the app's bridge by name.
	js, err := fs.ReadFile(web.Files, strings.TrimPrefix(previewPanelScriptSrc, "/"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"var APP = '" + PreviewAppAttr + "', FULL = '" + PreviewFullAttr + "'", "shellWin()." + AppBridgeName + ";", "type: 'back-claim'"} {
		if !strings.Contains(string(js), s) {
			t.Errorf("preview-panel.js lacks %q", s)
		}
	}
}
