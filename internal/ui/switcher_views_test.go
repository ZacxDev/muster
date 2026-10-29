package ui

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// waitingFixture is one of each way a card can — and cannot — wait on the
// viewer: an ask, a session at a prompt, a picker, an agent awaiting approval
// (waiting); a reply draft, and nothing at all — pinned (not). Names pairwise
// distinct.
func waitingFixture() ([]AgentCardView, []MirrorCardView) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	att := func(kind string) *CardAttention {
		return &CardAttention{Count: 1, Kind: kind, Kinds: []string{kind}, Href: "/attention#" + kind}
	}
	list := []AgentCardView{
		{ID: 1, Name: "ag-asks", LastActiveAt: now.Add(-1 * time.Minute), Attention: att("ask")},
		{ID: 2, Name: "ag-approval", LastActiveAt: now.Add(-2 * time.Minute), AwaitingApproval: true},
		{ID: 3, Name: "ag-draft", LastActiveAt: now.Add(-3 * time.Minute), Attention: att("plugin")},
		// Pinned and NOT waiting: a view built from the wrong set shows it.
		{ID: 4, Name: "ag-quiet", LastActiveAt: now.Add(-4 * time.Minute), Pinned: true},
	}
	mirrors := []MirrorCardView{
		{Host: "tower", Session: "w", Window: "@1", Label: "prompt:1", Online: true, ActiveAt: now.Add(-5 * time.Minute), Attention: att("session")},
		{Host: "tower", Session: "w", Window: "@2", Label: "picker:2", Online: true, ActiveAt: now.Add(-6 * time.Minute), Attention: att("question")},
		{Host: "tower", Session: "w", Window: "@3", Label: "drafty:3", Online: true, ActiveAt: now.Add(-7 * time.Minute), Attention: att("plugin")},
		{Host: "tower", Session: "w", Window: "@4", Label: "calm:4", Online: true, ActiveAt: now.Add(-8 * time.Minute), Pinned: true},
	}
	return list, mirrors
}

// svAttr is n's attribute key, and whether it has it.
func svAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// chipShownHrefs is the href of every card the Agents list's "Waiting on you"
// chip SHOWS in the rendered list partial: its selector (chipSelector — the
// script's own, chipSelectorsJSON), evaluated on each card article, and the
// card's own link. A card whose article carries no link fails the instrument.
func chipShownHrefs(t *testing.T, partial string) (shown, all []string) {
	t.Helper()
	sel := chipSelector([]string{"waiting"})
	m := regexp.MustCompile(`^\[data-([a-z]+)~="([^"]+)"\]$`).FindStringSubmatch(sel)
	if m == nil {
		t.Fatalf("instrument check FAILED: the chip's selector %q is not one [attr~=word] test this test can evaluate", sel)
	}
	attr, word := "data-"+m[1], m[2]
	doc, err := html.Parse(strings.NewReader(partial))
	if err != nil {
		t.Fatal(err)
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "article" {
			href := ""
			var find func(*html.Node)
			find = func(c *html.Node) {
				if href != "" {
					return
				}
				if c.Type == html.ElementNode && c.Data == "a" {
					if h, ok := svAttr(c, "href"); ok && (strings.HasPrefix(h, "/agents/") || strings.HasPrefix(h, "/mirror/")) {
						href = h
						return
					}
				}
				for k := c.FirstChild; k != nil; k = k.NextSibling {
					find(k)
				}
			}
			find(n)
			if href == "" {
				t.Fatalf("instrument check FAILED: a card article links nowhere")
			}
			all = append(all, href)
			if v, ok := svAttr(n, attr); ok {
				for _, f := range strings.Fields(v) {
					if f == word {
						shown = append(shown, href)
						break
					}
				}
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return shown, all
}

// TestTheWaitingViewIsTheWaitingChipsSet: 🔴 a SEAM guard, pinning a
// RELATIONSHIP. The session switcher's Waiting view holds EXACTLY the entries
// the Agents list's "Waiting on you" chip shows — the chip's real selector
// evaluated on the real rendered cards — in the list's order; and the sidebar
// marks the same set waiting. One of each way to wait and not to wait, so a
// predicate that drops a kind, or adds the reply draft, moves the sets apart.
//
// REGRESSION TEST for the Waiting view (operator ask: a bar view "that uses
// the same filter as the waiting for you filter on the agent's list page"):
// red at the commit before, which had no such view.
func TestTheWaitingViewIsTheWaitingChipsSet(t *testing.T) {
	list, mirrors := waitingFixture()
	shown, all := chipShownHrefs(t, listPartial(t, list, mirrors, nil))
	if len(shown) == 0 || len(shown) == len(all) {
		t.Fatalf("CONTROL: the chip shows %d of %d cards; the fixture must leave some out and keep some", len(shown), len(all))
	}
	s := NewSwitcher(list, mirrors, nil, SwitcherCurrent{})
	var view []string
	for _, it := range s.Waiting {
		view = append(view, strings.ReplaceAll(it.Href, "&amp;", "&"))
	}
	// The list partial's hrefs are parsed (unescaped) HTML; the items' are raw.
	if strings.Join(view, " | ") != strings.Join(shown, " | ") {
		t.Errorf("the Waiting view is\n%v\nthe chip shows\n%v\n(same set, same order)", view, shown)
	}
	// The ways to wait, by name: an ask, approval, a prompt, a picker.
	var labels []string
	for _, it := range s.Waiting {
		labels = append(labels, it.Label)
	}
	sort.Strings(labels)
	if got := strings.Join(labels, ","); got != "ag-approval,ag-asks,picker:2,prompt:1" {
		t.Errorf("Waiting holds %s", got)
	}
	// The sidebar marks the same entries waiting (data-nav-wait).
	nav := renderString(t, NavAgentsList(list, mirrors, "", time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)))
	var navWait []string
	for _, m := range regexp.MustCompile(`<a [^>]*data-nav-wait[^>]*title="([^"]*)"`).FindAllStringSubmatch(nav, -1) {
		navWait = append(navWait, m[1])
	}
	sort.Strings(navWait)
	if strings.Join(navWait, ",") != strings.Join(labels, ",") {
		t.Errorf("the sidebar marks %v waiting, the Waiting view holds %v", navWait, labels)
	}
}

// TestTheWaitingViewDrawsThePageAsCurrentAndSaysWhenEmpty: the page's own
// session, when it waits, is drawn current in Waiting (as in Pins and All);
// nothing waiting draws the row's empty line.
func TestTheWaitingViewDrawsThePageAsCurrentAndSaysWhenEmpty(t *testing.T) {
	list, mirrors := waitingFixture()
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{AgentID: 1}), false))
	got := switcherRowEntries(t, page, SwitcherWaiting)
	if len(got) != 4 || got[0] != "ag-asks (current)" {
		t.Errorf("Waiting = %v; want ag-asks drawn current, first", got)
	}
	quiet := renderString(t, switcherBar(NewSwitcher(list[3:], mirrors[3:], nil, SwitcherCurrent{}), false))
	row := quiet[strings.Index(quiet, `data-switcher-list="`+SwitcherWaiting+`"`):]
	row = row[:strings.Index(row, "</div>")]
	if !strings.Contains(row, "Nothing is waiting on you") {
		t.Errorf("an empty Waiting row does not say so:\n%s", row)
	}
}

// TestThePinToggleIsOnASessionsBarOnly: a session's page — an agent's chat, a
// mirrored window's view — draws a pin toggle for ITS session in the bar,
// before the search: posting that subject's fields to pin (unpressed) or unpin
// (pressed), with view=switcher and the page to come back to. The Agents
// list's dock, which has no page session, draws none. Red at the commit
// before: no toggle anywhere on the bar.
func TestThePinToggleIsOnASessionsBarOnly(t *testing.T) {
	list, mirrors := switcherFixture() // ag-two and mango:2 (@2) pinned
	field := func(form, name string) string {
		m := regexp.MustCompile(`<input type="hidden" name="` + name + `" value="([^"]*)">`).FindStringSubmatch(form)
		if m == nil {
			return "<none>"
		}
		return m[1]
	}
	toggle := func(page string) string {
		m := regexp.MustCompile(`<form method="post" action="[^"]*" data-switcher-pin=""[^>]*>.*?</form>`).FindString(page)
		if m == "" {
			t.Fatalf("no pin toggle in:\n%s", page)
		}
		return m
	}
	for _, c := range []struct {
		name    string
		cur     SwitcherCurrent
		action  string
		pressed string
		fields  map[string]string
	}{
		{"an unpinned agent", SwitcherCurrent{AgentID: 1}, PinPath, "false",
			map[string]string{"kind": "agent", "agent_id": "1", "view": SwitcherPinView, "back": "/agents/ag-one"}},
		{"a pinned agent", SwitcherCurrent{AgentID: 2}, UnpinPath, "true",
			map[string]string{"kind": "agent", "agent_id": "2", "back": "/agents/ag-two"}},
		{"a pinned window", SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@2"}}, UnpinPath, "true",
			map[string]string{"kind": "mirror", "host": "tower", "session": "w", "window": "@2"}},
		{"an unpinned window", SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@1"}}, PinPath, "false",
			map[string]string{"kind": "mirror", "window": "@1"}},
	} {
		page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, c.cur), true))
		form := toggle(page)
		if !strings.Contains(form, `action="`+c.action+`"`) || !strings.Contains(form, `hx-post="`+c.action+`"`) {
			t.Errorf("%s: the toggle does not post to %s:\n%s", c.name, c.action, form)
		}
		if !strings.Contains(form, `id="`+SwitcherPinID+`" type="submit" aria-pressed="`+c.pressed+`"`) {
			t.Errorf("%s: the toggle is not aria-pressed=%s:\n%s", c.name, c.pressed, form)
		}
		for k, v := range c.fields {
			if got := field(form, k); got != v {
				t.Errorf("%s: field %s = %q, want %q", c.name, k, got, v)
			}
		}
		if strings.Index(page, `data-switcher-pin=""`) > strings.Index(page, `id="`+switcherSearchOpenID+`"`) {
			t.Errorf("%s: the toggle is not before the search button", c.name)
		}
	}
	dock := renderString(t, switcherDock(NewSwitcher(list, mirrors, nil, SwitcherCurrent{}), true))
	if strings.Contains(dock, "data-switcher-pin") || strings.Contains(dock, `id="`+SwitcherPinID+`"`) {
		t.Error("the Agents list's dock (no page session) draws a pin toggle")
	}
	// A page whose session is not among the entries draws none either.
	gone := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{AgentID: 99}), false))
	if strings.Contains(gone, "data-switcher-pin") {
		t.Error("a page whose session is not listed draws a pin toggle")
	}
}

// TestTheSidebarSearchOpensTheOnePalette: every page with the sidebar draws
// ONE command palette (in the sidebar's root, outside the sliding panel) and a
// magnifier in the sidebar's header that opens it (paletteOpenAttr) — the same
// attribute the session switcher's search carries, so the two open one box;
// the session pages, which also draw the bar, still draw one palette. Red at
// the commit before: no sidebar button, and the palette lived in the bar.
func TestTheSidebarSearchOpensTheOnePalette(t *testing.T) {
	for name, doc := range navDocuments() {
		page := renderString(t, doc)
		if n := strings.Count(page, `<div id="`+paletteID+`" popover=""`); n != 1 {
			t.Errorf("%s: %d palettes, want one", name, n)
		}
		btn := regexp.MustCompile(`<button id="` + SidebarSearchID + `"[^>]*>`).FindString(page)
		if !strings.Contains(btn, paletteOpenAttr+`=""`) || !strings.Contains(btn, `aria-controls="`+paletteID+`"`) || !strings.Contains(btn, `aria-label="Search sessions"`) {
			t.Errorf("%s: the sidebar's search button does not open the palette: %q", name, btn)
		}
		side := page[strings.Index(page, `<nav id="sidebar"`):]
		side = side[:strings.Index(side, `id="sidebar-agents"`)]
		if !strings.Contains(side, `id="`+SidebarSearchID+`"`) {
			t.Errorf("%s: the search button is not in the sidebar's header", name)
		}
		root := page[strings.Index(page, `<div id="sidebar-root"`):]
		if pal := strings.Index(root, `id="`+paletteID+`"`); pal < 0 || pal < strings.Index(root, `</nav>`) {
			t.Errorf("%s: the palette is not in the sidebar root after (outside) the sliding panel", name)
		}
		if strings.Contains(page, `id="`+switcherSearchOpenID+`"`) {
			bar := regexp.MustCompile(`<button id="` + switcherSearchOpenID + `"[^>]*>`).FindString(page)
			if !strings.Contains(bar, paletteOpenAttr+`=""`) {
				t.Errorf("%s: the bar's search does not open the same palette: %q", name, bar)
			}
		}
	}
	// The script reads the bar's All row first, then the sidebar's rows.
	bar, side := strings.Index(paletteJS, `[data-switcher-list="all"] a[data-switcher-entry]`), strings.Index(paletteJS, `#`+NavAgentsListID+` a[data-nav-agent]:not([data-nav-current])`)
	if bar < 0 || side < 0 || bar > side {
		t.Error("the palette does not read the bar's All row, then the sidebar's rows (the page's own left out)")
	}
}

// TestTheSidebarRowsCarryWhatThePaletteFindsThemBy: on a page with no bar the
// palette reads the sidebar's rows, so each row carries its code, title and
// words — the switcher's own (switcherItems), never a second derivation.
func TestTheSidebarRowsCarryWhatThePaletteFindsThemBy(t *testing.T) {
	list, mirrors := switcherFixture()
	mirrors[0].Focus = &FocusView{Title: "Migrate the billing ledger"}
	list[2].DisplayName = "Release tidy"
	nav := renderString(t, NavAgentsList(list, mirrors, "", time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)))
	for _, it := range NewSwitcher(list, mirrors, nil, SwitcherCurrent{}).All {
		row := regexp.MustCompile(`<a [^>]*href="` + regexp.QuoteMeta(amp(it.Href)) + `"[^>]*>`).FindString(nav)
		if row == "" {
			t.Fatalf("no sidebar row for %s", it.Href)
		}
		for k, v := range map[string]string{"data-switcher-code": it.Code, "data-switcher-title": it.Title, "data-switcher-words": it.Words} {
			has := strings.Contains(row, k+`="`+html.EscapeString(v)+`"`)
			if v == "" {
				has = !strings.Contains(row, k+`=`)
			}
			if !has {
				t.Errorf("row %s: %s is not %q: %s", it.Label, k, v, row)
			}
		}
	}
}
