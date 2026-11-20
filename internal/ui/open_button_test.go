package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheOpenButtonCountsPreviewsPlusPages: the one count is the window's
// previews (offers + open) PLUS the session's pages — pairwise distinct
// inputs, so a count of any one of them, or of two, cannot pass — its name
// says each, and its box is one fixed width whatever the count (so a new count
// moves nothing). Nothing to open: no button.
func TestTheOpenButtonCountsPreviewsPlusPages(t *testing.T) {
	b := renderString(t, OpenButton(OpenCounts{Previews: mirror.WindowPreviews{Offered: 2, Open: 1}, Pages: 4}))
	for _, want := range []string{
		`data-preview-count="7"`, `data-open-previews="3"`, `data-open-pages="4"`,
		`aria-label="Open: 1 preview open, 2 servers to preview, 4 pages"`,
		`>7</span>`,
		` w-11 `, ` sm:w-[5.5rem]`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("the button lacks %s:\n%s", want, b)
		}
	}
	// Pages alone, no previews: drawn (no viewer plugin needed).
	if b := renderString(t, OpenButton(OpenCounts{Pages: 5})); !strings.Contains(b, `data-preview-count="5"`) || !strings.Contains(b, `aria-label="Open: 5 pages"`) {
		t.Errorf("pages alone: %s", b)
	}
	// A count past the cap is drawn capped, in the same box.
	big := renderString(t, OpenButton(OpenCounts{Previews: mirror.WindowPreviews{Offered: mirror.MaxPreviewCount}, Pages: 6}))
	if !strings.Contains(big, `>`+previewCount(mirror.MaxPreviewCount+6)+`</span>`) || !strings.Contains(big, ` w-11 `) {
		t.Errorf("past the cap: %s", big)
	}
	if OpenButton(OpenCounts{}) != nil {
		t.Error("a button with nothing to open")
	}
}

// TestThePanelHasPreviewsAndPagesTabsWithoutAPlugin: with pages on and no
// viewer plugin, the slot polls and the panel is drawn — its Previews tab
// saying there is nothing to preview and minting nothing (no mint data), its
// Pages tab reading the session's list — the tabs stored per viewer, in the
// shared swipe's area.
func TestThePanelHasPreviewsAndPagesTabsWithoutAPlugin(t *testing.T) {
	c := &PluginChips{Host: "ws", Session: "main", Window: "@1", PagesOn: true, Pages: 2, Viewer: 9}
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1", Plugins: c}))
	for _, want := range []string{
		`id="preview-slot"`, `hx-get="` + PreviewButtonPath + `?host=ws&amp;session=main&amp;window=%401"`,
		`id="preview-toggle"`, `data-preview-count="2"`,
		`id="preview-panel"`, `data-pages-src="` + SessionPagesPath + `?host=ws&amp;session=main&amp;window=%401"`,
		`data-tabs-key="session-open:9"`, `data-open-empty="previews"`, NothingToPreview,
		`data-tab="previews"`, `data-tab="pages"`, `data-open-count="pages"`,
		TabSwipeAttr + `="open-tabs"`, `data-page-frame=""`, `sandbox="` + SessionPageSandbox + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	for _, not := range []string{`data-mint=`, `data-preview-frame=""`, `data-preview-newtab=""`} {
		if strings.Contains(page, not) {
			t.Errorf("with no viewer plugin the panel draws %s", not)
		}
	}
	// CONTROL: with a viewer plugin the same page draws them.
	c.Chips = []PluginChip{{ID: 4, Label: "Example"}}
	withPlugin := renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1", Plugins: c}))
	for _, want := range []string{`data-mint=`, `data-preview-frame=""`, `data-preview-newtab=""`} {
		if !strings.Contains(withPlugin, want) {
			t.Errorf("CONTROL: with a viewer plugin the panel lacks %s", want)
		}
	}
	// No pages feature and no plugin: nothing at all.
	if page := renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1", Plugins: nil})); strings.Contains(page, `id="preview-slot"`) || strings.Contains(page, `id="preview-panel"`) {
		t.Error("a page with neither draws the slot or the panel")
	}
}

// TestTheSessionPagesListIsPlainedAndDescribesEachPage: the Pages tab's list
// keeps the order given, draws each page's current version and share state,
// plains a title's markdown and escapes its markup, and says when there are
// none.
func TestTheSessionPagesListIsPlainedAndDescribesEachPage(t *testing.T) {
	out := renderString(t, SessionPagesList([]SessionPage{
		{ID: 31, Title: "**Launch** <plan>", Version: 3, Shared: true, Made: "2 hours ago"},
		{ID: 12, Title: "Intake", Version: 1},
	}))
	if a, b := strings.Index(out, `data-session-page="31"`), strings.Index(out, `data-session-page="12"`); a < 0 || b < a {
		t.Fatalf("order: %d %d\n%s", a, b, out)
	}
	for _, want := range []string{`data-page-title="Launch &lt;plan&gt;"`, `data-page-version="v3"`, `data-page-share="true"`, `Shared · live link`,
		`data-page-version="v1"`, `data-page-share="false"`, `Not shared`, `made 2 hours ago`,
		`data-page-embed="/pages/31/view?embed=1"`, `data-page-view="/pages/31/view"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the list lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<plan>") || strings.Contains(out, "**") {
		t.Errorf("a title's markup or markdown reached the list:\n%s", out)
	}
	if none := renderString(t, SessionPagesList(nil)); !strings.Contains(none, SessionPagesNone) {
		t.Errorf("no pages: %s", none)
	}
}
