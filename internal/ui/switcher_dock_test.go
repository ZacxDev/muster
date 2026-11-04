package ui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/recents"
)

// TestThePillsCarryTheDocksHostAndFocus: every pill carries its host and its
// current focus title as values of their own (data-switcher-host,
// data-switcher-focus), which the dock's row draws beside the label. The
// fixture's host and focus are spelled like no name, title or word of the
// entry, so an attribute that copied another field reads wrong.
func TestThePillsCarryTheDocksHostAndFocus(t *testing.T) {
	list := []AgentCardView{{ID: 1, Name: "ag-heron", DisplayName: "Heron", Status: "running", Focus: &FocusView{Title: "Rebalance the quorum ledger"}}}
	mirrors := []MirrorCardView{{Host: "box-zeta", Session: "w", Window: "@1", Label: "plum:1", Online: true, Focus: &FocusView{Title: "Sweep the stale shards"}}}
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{}), true))
	all := page[strings.Index(page, `data-switcher-list="all"`):strings.Index(page, `id="switcher-views"`)]
	for _, want := range []string{
		`data-switcher-code="plum:1" data-switcher-title="Sweep the stale shards" data-switcher-words="w box-zeta" data-switcher-host="box-zeta" data-switcher-focus="Sweep the stale shards" class=`,
		`data-switcher-code="ag-heron" data-switcher-title="Heron · Rebalance the quorum ledger" data-switcher-words="standard ` + podHost + `" data-switcher-host="` + podHost + `" data-switcher-focus="Rebalance the quorum ledger" class=`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the All row lacks\n%s\nin:\n%s", want, all)
		}
	}
	// CONTROL: an entry with no focus carries no focus attribute (not an empty one).
	none := renderString(t, switcherEntry(SwitcherItem{Href: "/x", Label: "x", Kind: "agent", Host: "h"}))
	if strings.Contains(none, "data-switcher-focus") || !strings.Contains(none, `data-switcher-host="h"`) {
		t.Errorf("an entry with no focus:\n%s", none)
	}
}

// dockFixture is n agents, ids 1..n, named ag-001…, all listed.
func dockFixture(n int) []AgentCardView {
	var list []AgentCardView
	for i := 1; i <= n; i++ {
		list = append(list, AgentCardView{ID: int64(i), Name: fmt.Sprintf("ag-%03d", i), Status: "running"})
	}
	return list
}

// TestRecentHoldsThirtyForTheDock (decision 6): 35 visits, newest first, two of
// them to sessions since deleted and one the page itself. The bar's Recent is
// exactly the first 8 eligible; RecentMore is the next 22 hrefs, in visit
// order, the deleted ones and the page absent; together 30. Control: with 5
// visits the bar shows 5 and there is nothing more.
func TestRecentHoldsThirtyForTheDock(t *testing.T) {
	list := dockFixture(40)
	// Visits: 40, 39, …, 6 (35 of them), newest first. 37 and 20 are deleted
	// (not listed); 33 is the page.
	var refs []RecentRef
	for id := int64(40); id >= 6; id-- {
		refs = append(refs, RecentRef{AgentID: id})
	}
	var listed []AgentCardView
	for _, a := range list {
		if a.ID != 37 && a.ID != 20 {
			listed = append(listed, a)
		}
	}
	s := NewSwitcher(listed, nil, refs, SwitcherCurrent{AgentID: 33})
	var want []string
	for id := int64(40); id >= 6 && len(want) < dockRecentMax; id-- {
		if id == 37 || id == 20 || id == 33 {
			continue
		}
		want = append(want, fmt.Sprintf("/agents/ag-%03d", id))
	}
	var bar []string
	for _, it := range s.Recent {
		bar = append(bar, it.Href)
	}
	if strings.Join(bar, " ") != strings.Join(want[:switcherRecentMax], " ") {
		t.Errorf("the bar's Recent = %v\nwant %v", bar, want[:switcherRecentMax])
	}
	if strings.Join(s.RecentMore, " ") != strings.Join(want[switcherRecentMax:], " ") {
		t.Errorf("RecentMore = %v\nwant %v", s.RecentMore, want[switcherRecentMax:])
	}
	if len(s.Recent)+len(s.RecentMore) != 30 {
		t.Errorf("the dock's Recent holds %d, want 30", len(s.Recent)+len(s.RecentMore))
	}
	// Every one of the extra hrefs is an All pill (the dock draws it from there).
	page := renderString(t, switcherBar(s, true))
	all := page[strings.Index(page, `data-switcher-list="all"`):strings.Index(page, `id="switcher-views"`)]
	for _, h := range s.RecentMore {
		if !strings.Contains(all, `<a href="`+h+`" hx-boost="false"`) {
			t.Errorf("%s is not a pill of the All row", h)
		}
	}

	small := NewSwitcher(list, nil, refs[:5], SwitcherCurrent{})
	if len(small.Recent) != 5 || small.RecentMore != nil {
		t.Errorf("CONTROL: 5 visits drew %d on the bar and %d more", len(small.Recent), len(small.RecentMore))
	}
}

// recentMoreRE is the Recent row's hidden list of the extra hrefs.
var recentMoreRE = regexp.MustCompile(`<span hidden="" ` + switcherRecentMoreAttr + `="([^"]*)"></span></div>`)

// TestTheRecentRowCarriesTheExtraHrefsAsItsLastChild: the hrefs ride as the
// Recent row's hidden LAST CHILD, one per line — in the drawn bar and in the
// pin toggle's out-of-band copy, which replaces the row's children — so a
// refresh keeps them in step with the row. None when there are none.
func TestTheRecentRowCarriesTheExtraHrefsAsItsLastChild(t *testing.T) {
	list := dockFixture(12)
	var refs []RecentRef
	for id := int64(12); id >= 1; id-- {
		refs = append(refs, RecentRef{AgentID: id})
	}
	s := NewSwitcher(list, nil, refs, SwitcherCurrent{})
	want := "/agents/ag-004\n/agents/ag-003\n/agents/ag-002\n/agents/ag-001"
	for name, html := range map[string]string{
		"the bar":              renderString(t, switcherBar(s, true)),
		"the out-of-band copy": renderString(t, switcherRefresh(s)),
	} {
		m := recentMoreRE.FindAllStringSubmatch(html, -1)
		if len(m) != 1 || m[0][1] != want {
			t.Errorf("%s: the Recent row's extra hrefs = %q, want one %q", name, m, want)
			continue
		}
		// It is in the RECENT row, after its last pill.
		at := strings.Index(html, m[0][0])
		row := strings.LastIndex(html[:at], `data-switcher-list="recent"`)
		if row < 0 {
			row = strings.LastIndex(html[:at], `id="`+switcherRowID(SwitcherRecent)+`"`)
		}
		if row < 0 || strings.Contains(html[row:at], `data-switcher-list="all"`) || strings.Contains(html[row:at], `id="`+switcherRowID(SwitcherAll)+`"`) {
			t.Errorf("%s: the extra hrefs are not in the Recent row", name)
		}
	}
	if html := renderString(t, switcherBar(NewSwitcher(list, nil, refs[:8], SwitcherCurrent{}), true)); strings.Contains(html, switcherRecentMoreAttr) {
		t.Error("CONTROL: 8 visits carry a list of more")
	}
}

// TestTheRecentsStoreKeepsEnoughForTheDock: the store keeps more rows per
// viewer than the dock shows, so some of them naming gone sessions cannot
// leave the dock's Recent short. ⚠ AN INVARIANT GUARD, not regression coverage:
// it pins a relationship between two constants that holds before this change.
func TestTheRecentsStoreKeepsEnoughForTheDock(t *testing.T) {
	if recents.MaxPerOwner <= dockRecentMax {
		t.Errorf("recents.MaxPerOwner = %d, not above dockRecentMax = %d", recents.MaxPerOwner, dockRecentMax)
	}
}

// TestTheDockIsOneClosedDialogInTheSidebarRoot: every document with the
// sidebar draws the dock once, in #sidebar-root beside the palette (never in
// the bar, whose ancestors can hide it), closed, marked data-own-swipe (the
// sidebar's edge swipe stands down over it), its list a tabpanel of the four
// views' tabs and its name a heading.
func TestTheDockIsOneClosedDialogInTheSidebarRoot(t *testing.T) {
	page := renderString(t, AgentDetailPage(AgentDetailView{Name: "docs-sweeper", Kind: "gateway"}, Features{}))
	if n := strings.Count(page, `<dialog id="`+DockID+`"`); n != 1 {
		t.Fatalf("the agent page draws %d docks, want 1", n)
	}
	dock := page[strings.Index(page, `<dialog id="`+DockID+`"`):]
	dock = dock[:strings.Index(dock, "</dialog>")]
	if !strings.HasPrefix(dock, `<dialog id="switcher-dock" aria-labelledby="switcher-dock-title" data-own-swipe="" data-snap="half" class="`) {
		t.Errorf("the dock's head:\n%.300s", dock)
	}
	if regexp.MustCompile(`<dialog id="switcher-dock"[^>]*\bopen\b`).MatchString(page) {
		t.Error("the dock is drawn open")
	}
	root := strings.Index(page, `id="sidebar-root"`)
	pal := strings.Index(page, `<div id="`+paletteID+`"`)
	at := strings.Index(page, `<dialog id="`+DockID+`"`)
	bar := strings.Index(page, `<nav id="`+SwitcherID+`"`)
	if root < 0 || pal < 0 || at < pal || at < root {
		t.Errorf("the dock is not after the palette in #sidebar-root (root %d, palette %d, dock %d)", root, pal, at)
	}
	if bar >= 0 && at > bar && !strings.Contains(page[bar:at], "</nav>") {
		t.Error("the dock is inside the bar")
	}
	for _, want := range []string{
		`<h2 id="switcher-dock-title" class="sr-only">Switch session</h2>`,
		`role="tablist" aria-label="Session views"`,
		`<ul id="dock-list" role="tabpanel" aria-labelledby="dock-tab-recent" data-dock-list=""`,
		`data-dock-grip="" aria-label="Expand"`,
		`data-dock-close="" aria-label="Close"`,
	} {
		if !strings.Contains(dock, want) {
			t.Errorf("the dock lacks %s", want)
		}
	}
	var tabs []string
	for _, m := range regexp.MustCompile(`id="dock-tab-(\w+)" role="tab" aria-controls="dock-list" aria-selected="(true|false)" tabindex="(0|-1)"`).FindAllStringSubmatch(dock, -1) {
		tabs = append(tabs, m[1]+":"+m[2]+":"+m[3])
	}
	if strings.Join(tabs, " ") != "recent:true:0 waiting:false:-1 pins:false:-1 all:false:-1" {
		t.Errorf("the dock's tabs = %v", tabs)
	}
}

// TestTheDocksScriptBindsNoTouchListener: the dock's script is part of
// appScript, whose document-level touch listeners the sidebar's swipe test
// reads by their TEXT (touchListenerBodies, swipe_test.go); the dock binds
// none, so that extract stays a claim about the sidebar's code alone.
func TestTheDocksScriptBindsNoTouchListener(t *testing.T) {
	if strings.Contains(dockJS, "'touch") {
		t.Error("the dock binds a touch listener")
	}
}
