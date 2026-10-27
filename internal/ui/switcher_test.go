package ui

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// switcherFixture is a list with every case the bar resolves: four agents (one
// pinned, one the page's own), two windows with codenames that differ only by
// their index (so a label is disambiguated), one window pinned, and refs that
// name a gone agent, a gone window and the same agent twice.
func switcherFixture() ([]AgentCardView, []MirrorCardView) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	list := []AgentCardView{
		{ID: 1, Name: "ag-one", Status: "running", LastActiveAt: now.Add(-1 * time.Minute)},
		{ID: 2, Name: "ag-two", DisplayName: "Two", Status: "running", Pinned: true, LastActiveAt: now.Add(-9 * time.Minute)},
		{ID: 3, Name: "ag-three", Status: "running", LastActiveAt: now.Add(-3 * time.Minute), Attention: &CardAttention{Count: 1, Kind: "ask", Kinds: []string{"ask"}}},
		{ID: 4, Name: "ag-four", Status: "stopped", LastActiveAt: now.Add(-4 * time.Minute)},
	}
	mirrors := []MirrorCardView{
		{Host: "tower", Session: "w", Window: "@1", Label: "mango:1", Online: true, Claude: true, ActiveAt: now.Add(-2 * time.Minute)},
		{Host: "tower", Session: "w", Window: "@2", Label: "mango:2", Online: true, Pinned: true, ActiveAt: now.Add(-8 * time.Minute)},
	}
	return list, mirrors
}

var switcherEntryRE = regexp.MustCompile(`<(a|span) (?:href="([^"]*)" hx-boost="false" (?:data-snap-warm="" )?)?data-switcher-entry="(agent|mirror)"[^>]*><span class="[^"]*">(?:<span data-switcher-wait[^>]*></span>)?(?:<span data-host-icon.*?</svg></span>)?<span class="whitespace-nowrap">([^<]*)</span>`)

// switcherRowEntries is the entries the row of view key draws: for each, "label"
// for a link to href, or "label (current)" for the page's own.
func switcherRowEntries(t *testing.T, page, key string) []string {
	t.Helper()
	at := strings.Index(page, `data-switcher-list="`+key+`"`)
	if at < 0 {
		t.Fatalf("no %s row in:\n%s", key, page)
	}
	row := page[at:]
	if end := strings.Index(row[1:], `data-switcher-list="`); end >= 0 {
		row = row[:end+1]
	}
	if end := strings.Index(row, "</nav>"); end >= 0 {
		row = row[:end]
	}
	out := []string{}
	for _, m := range switcherEntryRE.FindAllStringSubmatch(row, -1) {
		if m[1] == "span" {
			out = append(out, m[4]+" (current)")
			continue
		}
		out = append(out, m[4]+" "+m[2])
	}
	return out
}

// TestTheSwitcherResolvesEachView: Recent is the refs, newest first, that are
// still there — pinned ones included, the page's own, gone ones and repeats
// left out — capped at switcherRecentMax; Pins is the pinned entries in the
// list's order; All is every entry in the list's order with the page's own
// drawn as current. Labels are sessionSwitcherLabels' (a codename without its
// index, unless another entry of the SAME view shares it).
func TestTheSwitcherResolvesEachView(t *testing.T) {
	list, mirrors := switcherFixture()
	w := func(id string) RecentRef {
		return RecentRef{Mirror: MirrorRef{Host: "tower", Session: "w", Window: id}}
	}
	a := func(id int64) RecentRef { return RecentRef{AgentID: id} }
	// The page is ag-one (its visit is the newest ref, as the server records it).
	refs := []RecentRef{a(1), a(77), w("@2"), a(3), w("@9"), a(3), a(2), w("@1")}
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, refs, SwitcherCurrent{AgentID: 1}), false))
	view := MirrorRef{Host: "tower", Session: "w", Window: "@1"}.ViewPath()
	term := MirrorRef{Host: "tower", Session: "w", Window: "@2"}.TerminalPath()
	for key, want := range map[string][]string{
		// mango:2 is the only "mango" until mango:1 (later in Recent) joins it,
		// so both read in full.
		SwitcherRecent: {"mango:2 " + amp(term), "ag-three /agents/ag-three", "Two /agents/ag-two", "mango:1 " + amp(view)},
		// Alone in its view, the same window reads "mango".
		SwitcherPins: {"mango " + amp(term), "Two /agents/ag-two"},
		SwitcherAll: {"mango:2 " + amp(term), "Two /agents/ag-two", "ag-one (current)", "mango:1 " + amp(view),
			"ag-three /agents/ag-three", "ag-four /agents/ag-four"},
	} {
		if got := switcherRowEntries(t, page, key); strings.Join(got, " | ") != strings.Join(want, " | ") {
			t.Errorf("%s =\n%v\nwant\n%v", key, got, want)
		}
	}
	// A codename alone in its view reads without its index: Pins with only
	// one window pinned.
	solo := renderString(t, switcherBar(NewSwitcher(list[:1], mirrors[1:], nil, SwitcherCurrent{}), false))
	if got := switcherRowEntries(t, solo, SwitcherPins); len(got) != 1 || !strings.HasPrefix(got[0], "mango ") {
		t.Errorf("a lone codename is not shortened: %v", got)
	}
	// A link to a window's Rendered view is a snapshot warming source
	// (a[data-snap-warm]); a terminal link and an agent's are not.
	warm := regexp.MustCompile(`<a href="([^"]*)" hx-boost="false" data-snap-warm=""`).FindAllStringSubmatch(page, -1)
	if len(warm) == 0 {
		t.Error("no Rendered-view link carries data-snap-warm")
	}
	for _, m := range warm {
		if !strings.HasPrefix(m[1], "/mirror/view?") {
			t.Errorf("data-snap-warm on a link that is not a Rendered view: %s", m[1])
		}
	}
	if !strings.Contains(page, `<a href="`+amp(term)+`" hx-boost="false" data-switcher-entry="mirror"`) {
		t.Error("the terminal link is not drawn plain (without data-snap-warm)")
	}

	// Every entry draws its host's icon, as its pinned chip does (#252): the
	// window's host for a window, the cluster for an agent — before the label.
	if !regexp.MustCompile(`data-switcher-entry="mirror"[^>]*><span class="[^"]*"><span data-host-icon="[^"]*" role="img" aria-label="Host: tower"`).MatchString(page) {
		t.Error("a window's entry does not draw its host's icon")
	}
	if !regexp.MustCompile(`href="/agents/ag-three"[^>]*><span class="[^"]*"><span data-switcher-wait[^>]*></span><span data-host-icon="` + clusterIcon.Key + `" role="img"`).MatchString(page) {
		t.Error("an agent's entry does not draw the cluster's icon after its waiting dot")
	}
	// A label that already names the host: the icon is decorative.
	named := renderString(t, switcherEntry(SwitcherItem{Href: "/x", Label: "mango:1 · tower", Kind: "mirror", Host: "tower", HostNamed: true}))
	if !strings.Contains(named, `<span data-host-icon="`) || !strings.Contains(named, `aria-hidden="true" title="Host: tower"`) || strings.Contains(named, `aria-label="Host: tower"`) {
		t.Errorf("a host-naming label's icon is not decorative:\n%s", named)
	}

	// …and NewSwitcher says so for a codename shared across two hosts, whose
	// labels carry the host-leading qualifier (pinnedChipLabelNamesHost).
	twins := NewSwitcher(nil, []MirrorCardView{
		{Host: "tower", Session: "w", Window: "@1", Label: "mango:1", Qualifier: "tower", Online: true},
		{Host: "other", Session: "w", Window: "@1", Label: "mango:1", Qualifier: "other", Online: true},
	}, nil, SwitcherCurrent{})
	for _, it := range twins.All {
		if !it.HostNamed || !strings.Contains(it.Label, it.Host) {
			t.Errorf("a host-qualified label is not marked as naming its host: %+v", it)
		}
	}
	if solo := NewSwitcher(nil, mirrors[:1], nil, SwitcherCurrent{}); solo.All[0].HostNamed {
		t.Error("CONTROL: a lone window's label is marked as naming its host")
	}

	// The waiting entry carries its dot.
	if !regexp.MustCompile(`href="/agents/ag-three"[^>]*><span class="[^"]*"><span data-switcher-wait`).MatchString(page) {
		t.Error("ag-three waits on the viewer, and its entry has no waiting dot")
	}
}

// TestTheSwitcherRecentIsCapped: more eligible refs than the cap draw exactly
// the cap, newest first.
func TestTheSwitcherRecentIsCapped(t *testing.T) {
	var list []AgentCardView
	var refs []RecentRef
	for i := int64(1); i <= switcherRecentMax+3; i++ {
		list = append(list, AgentCardView{ID: i, Name: "ag-" + string(rune('a'+i)), Status: "running"})
		refs = append(refs, RecentRef{AgentID: i})
	}
	s := NewSwitcher(list, nil, refs, SwitcherCurrent{})
	if len(s.Recent) != switcherRecentMax || s.Recent[0].Href != "/agents/ag-b" || s.Recent[switcherRecentMax-1].Href != "/agents/ag-"+string(rune('a'+switcherRecentMax)) {
		t.Errorf("Recent = %+v; want the first %d refs", s.Recent, switcherRecentMax)
	}
}

// TestTheSwitcherMarksTheCurrentWindow: a window's page is current against the
// card of the same address; a page addressed by its session alone (Window "")
// is current against that session's windows — and an agent with the same id
// space is never taken for it.
func TestTheSwitcherMarksTheCurrentWindow(t *testing.T) {
	list, mirrors := switcherFixture()
	for _, c := range []struct {
		name string
		cur  SwitcherCurrent
		want int
	}{
		{"the window's own address", SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@1"}}, 1},
		{"its session alone", SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w"}}, 2},
		{"another host", SwitcherCurrent{Mirror: MirrorRef{Host: "other", Session: "w", Window: "@1"}}, 0},
		{"no page", SwitcherCurrent{}, 0},
	} {
		n := 0
		for _, it := range NewSwitcher(list, mirrors, nil, c.cur).All {
			if it.Current {
				n++
				if it.Kind != "mirror" {
					t.Errorf("%s: an agent is marked current", c.name)
				}
			}
		}
		if n != c.want {
			t.Errorf("%s: %d current, want %d", c.name, n, c.want)
		}
	}
}

// TestTheSwitcherBarIsServerRenderedAtAFixedHeight: the toggle is first (the
// bar's left), Recent is shown and the other rows are drawn hidden, every row
// is a fixed-height sideways scroller inside a min-w-0 box, each empty view
// says so, and the page's own entry is not a link. The script that applies the
// stored view follows the bar (it must run before the bar is painted).
func TestTheSwitcherBarIsServerRenderedAtAFixedHeight(t *testing.T) {
	page := renderString(t, switcherBar(NewSwitcher(nil, nil, nil, SwitcherCurrent{}), true))
	nav := page[:strings.Index(page, "<script>")]
	if !strings.HasPrefix(nav, `<nav id="`+SwitcherID+`" aria-label="Switch session" data-switcher-view="recent" data-own-swipe=""`) {
		t.Errorf("the bar's head:\n%s", nav)
	}
	if !strings.Contains(nav, " px-3 pb-[env(safe-area-inset-bottom)]") {
		t.Error("the pinned composer's last row is not inset, or does not pad for the safe area")
	}
	toggle := strings.Index(nav, `id="switcher-toggle"`)
	first := strings.Index(nav, `data-switcher-list=`)
	if toggle < 0 || first < 0 || toggle > first {
		t.Fatalf("the toggle is not the bar's first control (toggle at %d, first row at %d)", toggle, first)
	}
	if !strings.Contains(nav, `<div class="flex h-11 min-w-0 items-center gap-2">`) {
		t.Error("the bar's row is not a fixed h-11")
	}
	for _, v := range switcherViews {
		row := regexp.MustCompile(`<div id="` + switcherRowID(v.key) + `" data-switcher-list="` + v.key + `" data-restore-refresh="switcher-` + v.key + `"( hidden="")? class="flex min-w-0 flex-1 items-center gap-1.5 overflow-x-auto"><p data-switcher-empty="" class="truncate text-xs text-muted">` + v.empty + `</p></div>`).FindStringSubmatch(nav)
		if row == nil {
			t.Errorf("%s: no empty row saying %q in:\n%s", v.key, v.empty, nav)
			continue
		}
		if hidden := row[1] != ""; hidden != (v.key != SwitcherRecent) {
			t.Errorf("%s: hidden = %v", v.key, hidden)
		}
	}
	if !strings.Contains(page, `localStorage.getItem(KEY)`) || !strings.Contains(page, `'`+switcherViewKey+`'`) {
		t.Error("the script does not apply the stored view")
	}
	if !strings.Contains(page, "bar.__musterSwitcherBound") || strings.Contains(page, "data-switcher-bound") {
		t.Error("the switcher's bind guard is not a JS property (an htmx history snapshot carries an attribute)")
	}
	// The page's own entry: a span with aria-current, never a link.
	list, mirrors := switcherFixture()
	all := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{AgentID: 4}), false))
	if !strings.Contains(all, `<span data-switcher-entry="agent" data-switcher-current="" aria-current="page" title="ag-four (this session)"`) ||
		strings.Contains(all, `href="/agents/ag-four"`) {
		t.Error("the page's own entry is not drawn as current, or is a link")
	}
	if !strings.Contains(all, `data-own-swipe="" class="absolute inset-x-0 bottom-0 border-t border-line bg-bg h-[2.8125rem] [&amp;[data-switcher-pending]]:invisible" data-switcher-pending="">`) {
		t.Error("the agent page's bar is not absolutely placed at its fixed height, or pads for the safe area / insets itself inside its pane")
	}
	if switcherBar(nil, true) != nil {
		t.Error("a nil switcher draws a bar")
	}
}

// TestTheSwitcherIsBelowTheInputOnBothPages: on the Rendered view the bar is
// inside the pinned composer AFTER the input row (so composerPinScript's
// measure includes it), the row gives up the safe-area inset to it, and the
// body reserves the taller fallback; on an agent's chat page it follows the
// chat form; the chief panel's copy of the pane draws none; and a page given
// no switcher draws none.
func TestTheSwitcherIsBelowTheInputOnBothPages(t *testing.T) {
	sw := NewSwitcher([]AgentCardView{{ID: 9, Name: "ag-nine", Status: "running"}}, nil, nil, SwitcherCurrent{})
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true, Switcher: sw}))
	compose, form, bar := strings.Index(view, `id="mv-compose"`), strings.Index(view, `id="mv-form"`), strings.Index(view, `id="`+SwitcherID+`"`)
	formEnd := form + strings.Index(view[form:], "</form>")
	// composerPinScript is the composer's last child: the bar must be before it.
	pin := strings.Index(view, `var box = document.getElementById('mv-compose');`)
	if compose < 0 || form < compose || bar < formEnd || pin < bar {
		t.Fatalf("the bar is not after the input row inside the composer (compose %d, form %d–%d, bar %d, pin script %d)", compose, form, formEnd, bar, pin)
	}
	// 🔴 After the draft's script: a restored draft sizes the box before
	// anything below the form is parsed (mirrorComposer's "the draft's is
	// FIRST among them" rule).
	if d := strings.Index(view, "window.musterDraft = {"); d < 0 || d > bar {
		t.Errorf("the bar is drawn before the draft's script (draft %d, bar %d)", d, bar)
	}
	formTag := view[form:]
	formTag = formTag[:strings.Index(formTag, ">")]
	if !strings.Contains(formTag, "pb-2") || strings.Contains(formTag, "safe-area-inset-bottom") {
		t.Errorf("with the bar below, the input row still pads for the safe area: %s", formTag)
	}
	// 🔴 THE ROOM IS RESERVED BY THE CONTAINER, THE BAR IS ABSOLUTE IN IT: a
	// paint with the form parsed and the bar not yet must not grow the
	// composer (measured 61→106px, a 0.0047–0.012 shift at 1280×800).
	composeTag := view[compose:]
	composeTag = composeTag[:strings.Index(composeTag, ">")]
	if !strings.Contains(composeTag, switcherRoom(true)) || !strings.Contains(view, `class="absolute inset-x-0 bottom-0 border-t border-line bg-bg h-[calc(2.8125rem+env(safe-area-inset-bottom))] px-3 pb-[env(safe-area-inset-bottom)] [&amp;[data-switcher-pending]]:invisible"`) {
		t.Errorf("the composer does not reserve the bar's room, or the bar is not absolute in it: %s", composeTag)
	}
	if !strings.Contains(view, `<body class="`+pinnedShellSwitcherClass) {
		t.Error("the body does not reserve the composer-with-switcher's fallback height")
	}
	plain := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true}))
	if strings.Contains(plain, `id="`+SwitcherID+`"`) || !strings.Contains(plain, `<body class="`+pinnedShellClass) ||
		!strings.Contains(plain, "pb-[max(0.75rem,env(safe-area-inset-bottom))]") {
		t.Error("CONTROL: a view with no switcher changed")
	}

	pane := renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1, Switcher: sw}))
	if f, b := strings.Index(pane, "</form>"), strings.Index(pane, `id="`+SwitcherID+`"`); b < 0 || b < f {
		t.Errorf("the agent page's bar is not after its chat form (form ends %d, bar %d)", f, b)
	}
	if d, b := strings.Index(pane, "window.musterDraft = {"), strings.Index(pane, `id="`+SwitcherID+`"`); d < 0 || d > b {
		t.Errorf("the agent page's bar is drawn before the draft's script (draft %d, bar %d)", d, b)
	}
	if form := pane[strings.Index(pane, `id="chat-form"`):]; !strings.Contains(form[:strings.Index(form, ">")], " "+switcherRoom(false)) {
		t.Error("the chat form does not reserve the bar's room")
	}
	if !strings.Contains(pane, `class="absolute inset-x-0 bottom-0 border-t border-line bg-bg h-[2.8125rem] [&amp;[data-switcher-pending]]:invisible"`) {
		t.Error("the agent page's bar is not absolute")
	}
	if !strings.Contains(pane, `class="flex flex-1 flex-col gap-2 min-h-0 relative"`) {
		t.Error("the agent pane is not relative (the bar's containing block)")
	}
	if strings.Contains(renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1})), switcherRoom(false)) {
		t.Error("CONTROL: a pane with no switcher reserves its room")
	}
	if !strings.Contains(pane, `href="/agents/ag-nine"`) {
		t.Error("the agent page's bar does not link the other agent")
	}
	panel := renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1, Switcher: sw, Surface: ChatSurfaceChiefPanel}))
	if strings.Contains(panel, SwitcherID) {
		t.Error("the chief panel draws the switcher")
	}
}

// TestThePaperclipIsSmallAndInsideTheInput: the button is a 24px square,
// absolutely placed inside a relative box that holds it and the input, BEFORE
// the input; the input is padded past it. No uploads: no box, no padding.
func TestThePaperclipIsSmallAndInsideTheInput(t *testing.T) {
	for _, want := range []string{"absolute", "h-6", "w-6", "left-1.5", "bottom-2.5"} {
		if !strings.Contains(" "+attachButtonClass+" ", " "+want+" ") {
			t.Errorf("attachButtonClass lacks %s: %s", want, attachButtonClass)
		}
	}
	for _, banned := range []string{"min-h-[44px]", "min-w-[44px]"} {
		if strings.Contains(attachButtonClass, banned) {
			t.Errorf("attachButtonClass still carries %s", banned)
		}
	}
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true, Upload: UploadPath}))
	box := regexp.MustCompile(`<div class="relative flex min-w-0 flex-1"><input id="mv-file"[^>]*><button id="mv-attach"[^>]*class="` + regexp.QuoteMeta(attachButtonClass) + `"><svg class="h-4 w-4"[^>]*><path[^>]*/></svg></button><textarea id="mv-input"[^>]*class="` + regexp.QuoteMeta(composerInputClass+attachInputPad) + `"`)
	if !box.MatchString(page) {
		t.Errorf("the paperclip is not inside the input's box, before it, with the input padded:\n%s", page[strings.Index(page, `id="mv-form"`):][:1500])
	}
	if !strings.Contains(page, `class="`+composerInputClass+` pl-9"`) {
		t.Error("the input is not padded past the paperclip (pl-9)")
	}
	off := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true}))
	if strings.Contains(off, `relative flex min-w-0 flex-1`) || strings.Contains(off, composerInputClass+attachInputPad) || !strings.Contains(off, `class="`+composerInputClass+`"`) {
		t.Error("CONTROL: with no uploads the input is still boxed or padded")
	}
}

// TestSendHasLessPadding: EVERY Send — the plain one (the agent chat page's,
// the no-Queue fallback of sendSplit) and the split one beside the Queue caret
// — is px-2.5, not the px-4 it was; each is still a 44px-tall target. And the
// pages draw them: the agent chat's Send and a queue-less composer's.
func TestSendHasLessPadding(t *testing.T) {
	for name, cls := range map[string]string{"composerSendClass": composerSendClass, "composerSendSplitClass": composerSendSplitClass} {
		if !strings.Contains(cls, " px-2.5 ") || strings.Contains(cls, "px-4") || !strings.Contains(cls, "min-h-[44px]") {
			t.Errorf("%s = %q", name, cls)
		}
	}
	pane := renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1}))
	if !strings.Contains(pane, `id="chat-send" class="`+composerSendClass+`"`) {
		t.Error("the agent chat page's Send does not wear composerSendClass")
	}
	plain := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true}))
	if !strings.Contains(plain, `aria-label="Send" class="`+composerSendClass+`">Send</button>`) {
		t.Error("a queue-less composer's Send does not wear composerSendClass")
	}
}

// TestTheSwitcherViewButtonOpensAMenu: the bar's left button is a menu button
// (popovertarget, aria-haspopup="menu", aria-expanded="false" at first) for a
// popover menu inside the bar, AFTER the rows (out of flow: it moves nothing),
// holding one radio item per view in order, the drawn one (Recent) checked,
// each with how many sessions it holds.
func TestTheSwitcherViewButtonOpensAMenu(t *testing.T) {
	list, mirrors := switcherFixture()
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, []RecentRef{{AgentID: 3}}, SwitcherCurrent{AgentID: 1}), false))
	nav := page[:strings.Index(page, "<script>")]
	if !regexp.MustCompile(`<button id="switcher-toggle" type="button" popovertarget="switcher-views" aria-haspopup="menu" aria-expanded="false" aria-controls="switcher-views" aria-label="Showing Recent sessions; choose a view"`).MatchString(nav) {
		t.Errorf("the view button is not a menu button for #switcher-views:\n%s", nav[:strings.Index(nav, `data-switcher-list=`)])
	}
	menuAt, lastRow := strings.Index(nav, `<div id="switcher-views" popover="" role="menu" aria-label="Session views"`), strings.LastIndex(nav, `data-switcher-list=`)
	if menuAt < 0 || menuAt < lastRow {
		t.Fatalf("no popover menu after the rows (menu %d, last row %d)", menuAt, lastRow)
	}
	items := regexp.MustCompile(`<button type="button" role="menuitemradio" aria-checked="(true|false)" data-switcher-pick="([a-z]+)"[^>]*>.*?<span class="flex-1">([^<]+)</span><span id="switcher-count-[a-z]+" class="text-xs tabular-nums text-muted" data-switcher-count="([a-z]+)" data-restore-refresh="switcher-count-([a-z]+)">(\d+)</span></button>`).FindAllStringSubmatch(nav[menuAt:], -1)
	got := []string{}
	for _, m := range items {
		if m[4] != m[2] || m[5] != m[2] {
			t.Errorf("%s's count is marked for %q / restore-refreshed as %q", m[2], m[4], m[5])
		}
		got = append(got, m[2]+"="+m[3]+"/"+m[6]+"/"+m[1])
	}
	// Recent: ag-three; Pins: mango:2, Two; All: all six (the page's own included).
	if want := "recent=Recent/1/true pins=Pins/2/false all=All/6/false"; strings.Join(got, " ") != want {
		t.Errorf("menu items = %v, want %s", got, want)
	}
}

// TestTheSwitcherSearchIsOnTheBar: a search button on the bar's right (after
// the rows) opens a popover palette — a combobox input over a listbox — and
// every entry carries what the search finds it by: a window its codename and
// focus title, an agent its name and its display name and focus title (each
// once, never repeating the codename).
func TestTheSwitcherSearchIsOnTheBar(t *testing.T) {
	list, mirrors := switcherFixture()
	mirrors[0].Focus = &FocusView{Title: "Migrate the billing ledger"}
	list[2].Focus = &FocusView{Title: "Tidy the release notes"}
	list[2].DisplayName = "Release tidy"
	list[3].DisplayName = "ag-four" // the same as its name: no title
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{}), true))
	nav := page[:strings.Index(page, "<script>")]
	open := strings.Index(nav, `<button id="switcher-search-open" type="button" aria-haspopup="dialog" aria-expanded="false" aria-controls="switcher-search" aria-keyshortcuts="/" aria-label="Search sessions"`)
	if open < 0 || open < strings.LastIndex(nav[:strings.Index(nav, `id="switcher-views"`)], `data-switcher-list=`) {
		t.Fatalf("no search button after the rows:\n%s", nav)
	}
	if !regexp.MustCompile(`<div id="switcher-search" popover="" role="dialog" aria-label="Search sessions"[^>]*><div[^>]*><svg[^>]*>.*?</svg><input id="switcher-search-input" type="text" role="combobox" aria-expanded="true" aria-controls="switcher-search-results" aria-autocomplete="list"[^>]*>.*?<ul id="switcher-search-results" role="listbox"`).MatchString(nav) {
		t.Error("the palette is not a popover dialog holding a combobox over a listbox")
	}
	all := nav[strings.Index(nav, `data-switcher-list="all"`):strings.Index(nav, `id="switcher-views"`)]
	for _, want := range []string{
		`data-switcher-entry="mirror" title="mango:1" data-switcher-code="mango:1" data-switcher-title="Migrate the billing ledger"`,
		`data-switcher-entry="mirror" title="mango:2" data-switcher-code="mango:2" class=`,
		`data-switcher-entry="agent" title="Release tidy" data-switcher-code="ag-three" data-switcher-title="Release tidy · Tidy the release notes"`,
		`data-switcher-entry="agent" title="ag-four" data-switcher-code="ag-four" class=`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the All row lacks %s in:\n%s", want, all)
		}
	}
	// The script wires "/" once per document, not in a terminal; it takes no
	// Ctrl/Cmd+K (the browser's own) and has no no-Popover-API fallback.
	for _, want := range []string{"window.__musterSwitcherKeys", "e.key !== '/'", "closest('.xterm')"} {
		if !strings.Contains(page, want) {
			t.Errorf("the script lacks %s", want)
		}
	}
	for _, banned := range []string{"'k'", "'K'", "Control+K", "Ctrl+K", "showPopover !== 'function'", "showPopover === 'function'", "!popover"} {
		if strings.Contains(page, banned) {
			t.Errorf("the bar still carries %s", banned)
		}
	}
}

// TestTheSwitcherNeverClampsALabel: an agent's long display name is drawn
// whole in the switcher (no "…"), while its card's title clamp (clampTitle)
// still cuts it to cardTitleMax — the CONTROL, so the fixture is long enough
// to be clamped at all.
func TestTheSwitcherNeverClampsALabel(t *testing.T) {
	long := "enterprise-customer-data-warehouse-schema-migration-validation-and-audit-report-v3"
	if len([]rune(long)) <= cardTitleMax {
		t.Fatalf("the fixture (%d runes) is not longer than cardTitleMax (%d)", len([]rune(long)), cardTitleMax)
	}
	list := []AgentCardView{{ID: 1, Name: "ag-long", DisplayName: long, Status: "running", Pinned: true}}
	s := NewSwitcher(list, nil, nil, SwitcherCurrent{})
	if s.All[0].Label != long || s.Pins[0].Label != long {
		t.Errorf("switcher labels = %q / %q, want the whole name", s.All[0].Label, s.Pins[0].Label)
	}
	page := renderString(t, switcherBar(s, false))
	rows := page[:strings.Index(page, `id="switcher-views"`)]
	if !strings.Contains(rows, `<span class="whitespace-nowrap">`+long+`</span>`) || strings.Contains(rows, "…") {
		t.Error("the switcher does not draw the whole name")
	}
	if short, clamped := clampTitle(long); !clamped || len([]rune(short)) != cardTitleMax {
		t.Errorf("CONTROL: the card's title clamp does not clamp the fixture: %q", short)
	}
}
