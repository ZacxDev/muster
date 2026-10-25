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

// A switcher entry and a Recent strip entry are up to 20rem wide, never
// wider than their row (they were 10rem and 14rem); a pinned chip is padded
// px-2.5 with gap-1 (it was px-4, gap-1.5).
func TestTitleEntriesAreWider(t *testing.T) {
	if titleEntryMaxW != "max-w-[min(20rem,100%)]" {
		t.Errorf("titleEntryMaxW = %q", titleEntryMaxW)
	}
	if !strings.Contains(switcherEntryBoxClass, " "+titleEntryMaxW+" ") || strings.Contains(switcherEntryBoxClass, "10rem") {
		t.Errorf("switcherEntryBoxClass = %q", switcherEntryBoxClass)
	}
	recent := renderString(t, recentEntry(recentItem{Href: "/agents/a", Title: "cache-warmup-orchestrator:1"}))
	if !strings.Contains(recent, " "+titleEntryMaxW+" ") || strings.Contains(recent, "14rem") {
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
// the full-title popover; the popover is OUTSIDE the <h1> and holds the whole
// title (name + qualifier) and, for a labelled window, its tmux session.
func TestMirrorViewTitleOpensTheFullTitle(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "scratch2", Window: "@1", Label: "billing-reconcile-nightly:1", Qualifier: "host-a", Interactive: true}))
	h1 := regexp.MustCompile(`(?s)<h1 class="min-w-0 text-sm font-semibold"><button id="mv-title" type="button" data-mv-title="" popovertarget="mv-title-full" aria-haspopup="dialog" aria-controls="mv-title-full" aria-expanded="false" class="block w-full min-w-0 truncate[^"]*">(.*?)</button></h1>`).FindStringSubmatch(page)
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
	if !strings.Contains(pop[1], ">billing-reconcile-nightly:1 · host-a</span>") || !strings.Contains(pop[1], ">tmux session scratch2</span>") {
		t.Errorf("the popover does not hold the whole title and its session:\n%s", pop[1])
	}
	if !strings.Contains(page, "window.__musterFloatingMenu") || !strings.Contains(page, "document.getElementById('mv-title')") {
		t.Error("the page does not carry the popover's placement and fit scripts")
	}
	// An unlabelled window: its title, and no session line (the title is it).
	plain := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@1", Title: "main:1", Interactive: true}))
	if !strings.Contains(plain, `data-title-full=""`) || !strings.Contains(plain, ">main:1</span></div>") || strings.Contains(plain, "data-title-full-session") {
		t.Error("an unlabelled window's popover is not just its title")
	}
}
