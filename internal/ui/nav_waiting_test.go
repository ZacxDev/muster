package ui

import (
	"strings"
	"testing"
	"time"
)

// menuBadgeFixture: four entries whose wait state is pairwise distinct — an
// agent with an ask (waits), an agent at a permission prompt with NO item
// queued (waits: listEntry.waiting, not the attention queue), an agent with a
// notice only (an attention count, but nothing filed under "you": no wait),
// and a mirrored window with a session item (waits); plus a quiet agent and a
// quiet window.
func menuBadgeFixture() ([]AgentCardView, []MirrorCardView) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	list := []AgentCardView{
		{ID: 1, Name: "ag-asks", Status: "running", LastActiveAt: now.Add(-1 * time.Minute), Attention: &CardAttention{Count: 1, Kind: "ask", Kinds: []string{"ask"}}},
		{ID: 2, Name: "ag-prompt", Status: "running", LastActiveAt: now.Add(-2 * time.Minute), AwaitingApproval: true},
		{ID: 3, Name: "ag-notice", Status: "running", LastActiveAt: now.Add(-3 * time.Minute), Attention: &CardAttention{Count: 2, Kind: "notice", Kinds: []string{"notice"}}},
		{ID: 4, Name: "ag-quiet", Status: "running", LastActiveAt: now.Add(-4 * time.Minute)},
	}
	mirrors := []MirrorCardView{
		{Host: "tower", Session: "w", Window: "@1", Label: "plum:1", Online: true, Claude: true, ActiveAt: now.Add(-5 * time.Minute),
			Attention: &CardAttention{Count: 1, Kind: "session", Kinds: []string{"session"}}},
		{Host: "tower", Session: "w", Window: "@2", Label: "kiwi:1", Online: true, Claude: true, ActiveAt: now.Add(-6 * time.Minute)},
	}
	return list, mirrors
}

// TestTheMenuBadgeCountsTheSidebarsWaitingRows: the ☰ badge's count is the
// number of sidebar rows marked waiting (data-nav-wait, listEntry.waiting) —
// over the SAME entries, so the badge and the rows cannot disagree. The
// fixture's 3 is not the attention count (5 items) nor the entries (6).
func TestTheMenuBadgeCountsTheSidebarsWaitingRows(t *testing.T) {
	list, mirrors := menuBadgeFixture()
	rows := renderString(t, NavAgentsList(list, mirrors, "", time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)))
	marked := strings.Count(rows, `data-nav-wait=""`)
	if got := NavWaitingCount(list, mirrors); got != 3 || got != marked {
		t.Fatalf("NavWaitingCount = %d, the sidebar marks %d rows waiting; want both 3", got, marked)
	}
	if got := NavWaitingCount(nil, nil); got != 0 {
		t.Fatalf("no entries: %d, want 0", got)
	}
}

// TestTheMenuBadgeFragmentCannotRetriggerItself: the badge carries no htmx
// wiring (the slot does), is hidden at zero, and the slot inside the ☰ button
// listens for the sidebar rows' refresh events.
func TestTheMenuBadgeFragmentCannotRetriggerItself(t *testing.T) {
	zero, three := NavWaitingBadgeHTML(0), NavWaitingBadgeHTML(3)
	for _, b := range []string{zero, three, NavWaitingBadgeHTML(250)} {
		if strings.Contains(b, "hx-") {
			t.Fatalf("the fragment carries htmx wiring: %s", b)
		}
	}
	if !strings.Contains(zero, " hidden>") || strings.Contains(three, " hidden") || !strings.Contains(three, `data-count="3"`) || !strings.Contains(three, `<span aria-hidden="true">3</span>`) {
		t.Fatalf("zero %s / three %s", zero, three)
	}
	if b := NavWaitingBadgeHTML(250); !strings.Contains(b, `>99+</span>`) || !strings.Contains(b, `data-count="250"`) {
		t.Fatalf("a large count: %s", b)
	}
	btn := renderString(t, sidebarOpenButton())
	for _, want := range []string{`id="nav-waiting-slot"`, `hx-get="` + NavWaitingPath + `"`, `hx-swap="innerHTML"`, `aria-describedby="` + NavWaitingBadgeID + `"`, `id="` + NavWaitingBadgeID + `"`} {
		if !strings.Contains(btn, want) {
			t.Fatalf("the ☰ button lacks %s: %s", want, btn)
		}
	}
	for _, ev := range []string{"sse:attention_queue.changed", "sse:agent.changed", "agents:changed", "muster:resync", NavOpenEvent, "every 60s"} {
		if !strings.Contains(navWaitingTrigger, ev) {
			t.Errorf("the badge does not refresh on %s: %s", ev, navWaitingTrigger)
		}
	}
}

// TestThePaletteCopiesTheSidebarsWaitingMark: a switcher pill carries its
// entry's waiting mark — the sidebar row's own markup, byte for byte — in a
// <template>, for the command palette's row; an entry with no mark carries
// none.
func TestThePaletteCopiesTheSidebarsWaitingMark(t *testing.T) {
	list, mirrors := menuBadgeFixture()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	items := switcherItems(orderList(list, mirrors), SwitcherCurrent{}, now)
	byLabel := map[string]SwitcherItem{}
	for _, it := range items {
		byLabel[it.Label] = it
	}
	for _, c := range []struct {
		label string
		want  string // a substring of the mark, "" for none
	}{
		{"ag-asks", `data-nav-attention="" data-kind="ask" data-count="1"`},
		{"ag-prompt", `data-nav-wait-dot=""`},
		// A count with nothing for "you": the sidebar draws the count, and so
		// does the palette (one call).
		{"ag-notice", `data-nav-attention="" data-kind="notice" data-count="2"`},
		{"plum:1", `data-nav-attention="" data-kind="session" data-count="1"`},
		{"ag-quiet", ""},
		{"kiwi:1", ""},
	} {
		it, ok := byLabel[c.label]
		if !ok {
			t.Fatalf("no item %q among %v", c.label, items)
		}
		pill := renderString(t, switcherEntry(it))
		if c.want == "" {
			if strings.Contains(pill, "<template") {
				t.Errorf("%s has no mark, its pill carries one: %s", c.label, pill)
			}
			continue
		}
		mark := renderString(t, entryWaitMark(it))
		if !strings.Contains(mark, c.want) {
			t.Errorf("%s's mark %s lacks %s", c.label, mark, c.want)
		}
		if want := `<template ` + paletteWaitMarkAttr + `="">` + mark + `</template>`; !strings.Contains(pill, want) {
			t.Errorf("%s's pill does not carry the sidebar's mark %s:\n%s", c.label, want, pill)
		}
	}
	// The sidebar row draws the same mark.
	rows := renderString(t, NavAgentsList(list, mirrors, "", now))
	if mark := renderString(t, entryWaitMark(byLabel["ag-asks"])); !strings.Contains(rows, mark) {
		t.Errorf("the sidebar row does not draw the mark the palette copies: %s", mark)
	}
	if !strings.Contains(paletteJS, `template[`+paletteWaitMarkAttr+`]`) || !strings.Contains(paletteJS, "[data-nav-attention], [data-nav-wait-dot]") {
		t.Error("the palette does not read the mark from a pill's template or a sidebar row")
	}
}

// TestTheOverlayBadgeIsDrawnWithThePage: on a page with a session switcher
// (the Rendered view, a Terminal view) the ☰ badge's first count is the
// switcher's Waiting view — drawn with the page, no load and no minute's poll —
// and it still refreshes on the events and each open of the drawer. A page with
// none loads it.
func TestTheOverlayBadgeIsDrawnWithThePage(t *testing.T) {
	list, mirrors := menuBadgeFixture()
	sw := NewSwitcher(list, mirrors, nil, SwitcherCurrent{})
	if len(sw.Waiting) != NavWaitingCount(list, mirrors) {
		t.Fatalf("the switcher's Waiting view holds %d, the badge counts %d", len(sw.Waiting), NavWaitingCount(list, mirrors))
	}
	over := renderString(t, overlayNavButton(sw))
	if !strings.Contains(over, `data-count="3"`) || strings.Contains(over, `data-count="3" class="pointer-events-none absolute right-0 top-0 inline-flex h-[1.125rem] min-w-[1.125rem] items-center justify-center rounded-full bg-accent px-1 text-[11px] font-semibold leading-none text-on-accent ring-2 ring-bg" hidden`) {
		t.Fatalf("the overlay ☰ does not draw the switcher's 3: %s", over)
	}
	for _, no := range []string{"load[", "every 60s"} {
		if strings.Contains(over, no) {
			t.Errorf("the overlay ☰ badge still has %q in its trigger: %s", no, over)
		}
	}
	for _, ev := range []string{NavOpenEvent, "sse:attention_queue.changed", "asks:changed"} {
		if !strings.Contains(over, ev) {
			t.Errorf("the overlay ☰ badge does not refresh on %s", ev)
		}
	}
	plain := renderString(t, overlayNavButton(nil))
	if !strings.Contains(plain, "load[") || !strings.Contains(plain, "every 60s") || !strings.Contains(plain, `data-count="0"`) {
		t.Errorf("without a switcher the badge must load its count: %s", plain)
	}
}
