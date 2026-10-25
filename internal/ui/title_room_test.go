package ui

import (
	"regexp"
	"strings"
	"testing"
)

// The Rendered | Terminal segments are px-1.5 (not px-2.5) and still 44px
// targets, and both pages that draw the switch use the one class.
func TestViewToggleSegmentsArePaddedLess(t *testing.T) {
	if !strings.Contains(" "+viewToggleSegClass, " px-1.5 ") || strings.Contains(viewToggleSegClass, "px-2.5") || !strings.Contains(viewToggleSegClass, "min-h-[44px]") {
		t.Errorf("viewToggleSegClass = %q", viewToggleSegClass)
	}
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@1", Interactive: true}))
	for _, id := range []string{"view-rendered", "view-terminal"} {
		if !regexp.MustCompile(`id="` + id + `"[^>]*class="` + regexp.QuoteMeta(viewToggleSegClass)).MatchString(page) {
			t.Errorf("%s does not wear viewToggleSegClass", id)
		}
	}
}

// A switcher entry and a Recent strip entry are up to 26rem wide, never
// wider than their row (they were 10rem and 14rem, then 20rem); a pinned chip is padded
// px-2.5 with gap-1 (it was px-4, gap-1.5).
func TestTitleEntriesAreWider(t *testing.T) {
	if titleEntryMaxW != "max-w-[min(26rem,100%)]" {
		t.Errorf("titleEntryMaxW = %q", titleEntryMaxW)
	}
	if !strings.Contains(switcherEntryBoxClass, " "+titleEntryMaxW+" ") || strings.Contains(switcherEntryBoxClass, "10rem") || strings.Contains(switcherEntryBoxClass, "20rem") {
		t.Errorf("switcherEntryBoxClass = %q", switcherEntryBoxClass)
	}
	recent := renderString(t, recentEntry(recentItem{Href: "/agents/a", Title: "cache-warmup-orchestrator:1"}))
	if !strings.Contains(recent, " "+titleEntryMaxW+" ") || strings.Contains(recent, "14rem") || strings.Contains(recent, "20rem") {
		t.Errorf("a Recent entry is not up to titleEntryMaxW:\n%s", recent)
	}
	for _, want := range []string{" px-2.5 ", " gap-1 ", "min-h-[44px]"} {
		if !strings.Contains(pinnedChipLinkClass+" ", want) {
			t.Errorf("pinnedChipLinkClass lacks %q: %s", want, pinnedChipLinkClass)
		}
	}
	for _, banned := range []string{"px-4", "gap-1.5"} {
		if strings.Contains(pinnedChipLinkClass, banned) {
			t.Errorf("pinnedChipLinkClass still carries %s", banned)
		}
	}
	got := renderString(t, switcherEntry(SwitcherItem{Href: "/agents/a", Label: "billing-reconcile-nightly:1", Kind: "agent"}))
	if !strings.Contains(got, switcherEntryBoxClass+`"`) {
		t.Errorf("an entry does not wear switcherEntryBoxClass:\n%s", got)
	}
}

// The rendered view's header title is a button inside the <h1> that targets
// the full-title popover, wired to agentTitleFitScript (the one clipping rule)
// in its disable mode; the popover is OUTSIDE the <h1> and holds the header
// line whole (name, qualifier, "Mirrored · <host>") and nothing else; the
// <title> is the same name (mirrorViewName).
func TestMirrorViewTitleOpensTheFullTitle(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "scratch2", Window: "@1", Label: "billing-reconcile-nightly:1", Qualifier: "tower", Interactive: true}))
	h1 := regexp.MustCompile(`(?s)<h1 data-agent-title="" data-title-fit="disable" class="min-w-0 text-sm font-semibold"><button id="mv-title" type="button" data-mv-title="" data-agent-title-text="" data-title-more="" popovertarget="mv-title-full" aria-haspopup="dialog" aria-controls="mv-title-full" aria-expanded="false" class="block w-full min-w-0 truncate[^"]*">(.*?)</button></h1>`).FindStringSubmatch(page)
	if h1 == nil {
		t.Fatalf("no title button in the header's h1:\n%s", page[strings.Index(page, "<header"):][:2000])
	}
	if !strings.Contains(h1[1], `data-mirror-name`) || !strings.Contains(h1[1], `data-term-badge`) {
		t.Errorf("the button does not hold the title and the badge:\n%s", h1[1])
	}
	if strings.Contains(h1[1], `data-title-full`) {
		t.Error("the popover is inside the <h1>")
	}
	pop := regexp.MustCompile(`(?s)<div id="mv-title-full" popover="auto"[^>]*data-floating-menu=""[^>]*>(.*?)</div>`).FindStringSubmatch(page)
	if pop == nil {
		t.Fatal("no full-title popover")
	}
	if pop[1] != `<span class="[overflow-wrap:anywhere] text-sm font-semibold">billing-reconcile-nightly:1 · tower · Mirrored · host-a</span>` {
		t.Errorf("the popover is not exactly the whole title:\n%s", pop[1])
	}
	if !strings.Contains(page, "<title>billing-reconcile-nightly:1 · tower · Mirrored · muster</title>") {
		t.Error("the <title> is not the header's name")
	}
	if !strings.Contains(page, "window.__musterFloatingMenu") || !strings.Contains(page, "window.__musterAgentTitleFit") || !strings.Contains(page, "data-title-fit') === 'disable'") {
		t.Error("the page does not carry the popover's placement script and the shared fit script")
	}
	plain := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@1", Title: "main:1", Interactive: true}))
	if !strings.Contains(plain, `<span class="[overflow-wrap:anywhere] text-sm font-semibold">main:1 · Mirrored · host-a</span></div>`) {
		t.Error("an unlabelled window's popover is not its header line")
	}
}
