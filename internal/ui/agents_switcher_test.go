package ui

import (
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/web"
)

// THE AGENTS LIST'S SESSION SWITCHER (switcher.go switcherDock): the operator
// asked for the switcher's bar on the Agents list too, superseding the list's
// "Recently viewed" strip, its pinned chips and its search box. Each test here
// renders the Agents panel, the shell or the list partial, so each compiles
// against origin/main and fails there on what it asserts.

// TestTheAgentsPanelDocksTheSwitcherAndReservesItsRoom: the panel draws the
// switcher's bar inside a dock FIXED to the bottom of the viewport (offset
// right of the desktop sidebar, as the content column is), and reserves the
// bar's room — its row and the safe-area inset — as its own bottom padding, in
// the panel's first byte, so the last card scrolls clear of the bar and its
// arrival moves nothing. The "Recently viewed" strip is gone. Red at
// origin/main: no dock, no switcher, and the strip.
func TestTheAgentsPanelDocksTheSwitcherAndReservesItsRoom(t *testing.T) {
	panel := renderNode(t, AgentsPanel(true))
	if strings.Contains(panel, `id="agents-recent"`) || strings.Contains(panel, "Recently viewed") {
		t.Error("the Agents panel still draws the Recently viewed strip")
	}
	open := panel[:strings.Index(panel, ">")]
	for _, want := range []string{`id="panel-agents"`, `data-switcher-room=""`, " " + switcherRoom(true)} {
		if !strings.Contains(open, want) {
			t.Errorf("the panel's own tag lacks %q: %s", want, open)
		}
	}
	dock := regexp.MustCompile(`<div id="` + SwitcherDockID + `" class="([^"]*)"><div class="([^"]*)"><nav id="` + SwitcherID + `"`).FindStringSubmatch(panel)
	if dock == nil {
		t.Fatalf("no switcher dock holding the bar in the panel:\n%s", panel)
	}
	for _, want := range []string{"fixed", "inset-x-0", "bottom-0", "lg:left-72", "bg-bg"} {
		if !strings.Contains(" "+dock[1]+" ", " "+want+" ") {
			t.Errorf("the dock lacks %s: %q", want, dock[1])
		}
	}
	if !strings.Contains(dock[2], "relative ") || !strings.Contains(dock[2], switcherRoom(true)) || !strings.Contains(dock[2], contentMaxW()) {
		t.Errorf("the dock's box is not the content column's width holding the bar's room: %q", dock[2])
	}
	// The pinned bar: its height includes the safe-area inset.
	if !strings.Contains(panel, switcherHeight(true)) {
		t.Errorf("the docked bar is not the pinned (safe-area) form")
	}
	// The dock is the panel's LAST child: after #agents-list, never inside it.
	if strings.Index(panel, `id="`+SwitcherDockID+`"`) < strings.Index(panel, `id="agents-list"`) {
		t.Error("the dock is drawn before the list")
	}
}

// TestAPanelWithoutTheListDrawsItsSwitcherPending: another tab's document (a
// client-side switch to Agents shows its panel) does not build the list, so
// its switcher's rows and counts are blank — not "Nothing pinned" — until the
// list's first load fills them out of band. The control: the Agents tab's own
// document draws its rows' empty lines.
func TestAPanelWithoutTheListDrawsItsSwitcherPending(t *testing.T) {
	pending := renderNode(t, AgentsPanel(false))
	for _, v := range switcherViews {
		if !strings.Contains(pending, `<div id="`+switcherRowID(v.key)+`" data-switcher-list="`+v.key+`"`) {
			t.Errorf("no %s row", v.key)
		}
		if strings.Contains(pending, v.empty) {
			t.Errorf("a pending bar claims %q before the list is known", v.empty)
		}
	}
	if !regexp.MustCompile(`data-switcher-count="recent" data-restore-refresh="switcher-count-recent"></span>`).MatchString(pending) {
		t.Errorf("a pending bar's count is not blank")
	}
	drawn := renderString(t, PageWith("agents", featuresAllOn, ShellData{}.WithList(nil, nil, nil, 0)))
	if !strings.Contains(drawn, "Nothing pinned") || !strings.Contains(drawn, "No other recent sessions") {
		t.Errorf("CONTROL: the Agents document's own bar should draw its empty lines")
	}
}

// TestTheAgentsDocumentDrawsItsSwitcherWithNothingCurrent: the Agents tab's
// own document draws the switcher's rows with the list on the first paint:
// Recent is the refs (pinned ones included, gone ones left out, newest first),
// Pins the pinned entries, All every entry — and there is no page session, so
// no entry is drawn current and none is left out. Red at origin/main: no bar.
func TestTheAgentsDocumentDrawsItsSwitcherWithNothingCurrent(t *testing.T) {
	list, mirrors := switcherFixture()
	refs := []RecentRef{{AgentID: 2}, {AgentID: 99}, {Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@1"}}, {AgentID: 1}}
	page := renderString(t, PageWith("agents", featuresAllOn, ShellData{}.WithList(list, mirrors, refs, 0)))
	if strings.Contains(page, `data-switcher-current`) || strings.Contains(page, `aria-current="page" title=`) {
		t.Error("an entry is drawn current on a page that has no session")
	}
	mango1 := amp(MirrorCardHref(mirrors[0]))
	if got, want := strings.Join(switcherRowEntries(t, page, SwitcherRecent), " | "), "Two /agents/ag-two | mango "+mango1+" | ag-one /agents/ag-one"; got != want {
		t.Errorf("Recent =\n%s\nwant\n%s", got, want)
	}
	if got := len(switcherRowEntries(t, page, SwitcherAll)); got != len(list)+len(mirrors) {
		t.Errorf("All draws %d entries, want every one of the %d", got, len(list)+len(mirrors))
	}
	if got := len(switcherRowEntries(t, page, SwitcherPins)); got != 2 {
		t.Errorf("Pins draws %d entries, want the 2 pinned", got)
	}
	for _, v := range switcherViews {
		n := map[string]int{SwitcherRecent: 3, SwitcherPins: 2, SwitcherAll: 6}[v.key]
		if !strings.Contains(page, `id="`+switcherCountID(v.key)+`" class="text-xs tabular-nums text-muted" data-switcher-count="`+v.key+`" data-restore-refresh="switcher-count-`+v.key+`">`+strconv.Itoa(n)+`</span>`) {
			t.Errorf("the %s count is not %d", v.key, n)
		}
	}
}

func amp(s string) string { return strings.ReplaceAll(s, "&", "&amp;") }

// TestTheListPartialRefreshesTheSwitcherOutOfBand: every list render carries
// the switcher's three rows and three counts out of band, each swapping its
// CHILDREN into the element of its id (innerHTML) — never the row itself, so
// the row the viewer chose stays shown — resolved against the SAME cards the
// list draws. A pin made on the page reaches the Pins view this way. Red at
// origin/main: the partial carried the Recently viewed strip instead.
func TestTheListPartialRefreshesTheSwitcherOutOfBand(t *testing.T) {
	list, mirrors := switcherFixture()
	part := listPartial(t, list, mirrors, []RecentRef{{AgentID: 3}})
	if strings.Contains(part, "agents-recent") || strings.Contains(part, "data-recent-entry") {
		t.Error("the partial still carries the Recently viewed strip")
	}
	for _, v := range switcherViews {
		for _, id := range []string{switcherRowID(v.key), switcherCountID(v.key)} {
			if n := strings.Count(part, `id="`+id+`" hx-swap-oob="innerHTML">`); n != 1 {
				t.Errorf("#%s is sent out of band (innerHTML) %d times, want 1", id, n)
			}
		}
	}
	if strings.Contains(part, `hx-swap-oob="true"`) || strings.Contains(part, `hx-swap-oob="outerHTML"`) || strings.Contains(part, `data-switcher-list=`) {
		t.Error("a row is replaced whole (its shown/hidden state with it), not its children")
	}
	pins := part[strings.Index(part, `id="`+switcherRowID(SwitcherPins)+`"`):]
	pins = pins[:strings.Index(pins, "</div>")]
	if !strings.Contains(pins, `href="/agents/ag-two"`) || strings.Contains(pins, `href="/agents/ag-one"`) {
		t.Errorf("the Pins row is not the pinned entries:\n%s", pins)
	}
	if !strings.Contains(part, `id="`+switcherCountID(SwitcherRecent)+`" hx-swap-oob="innerHTML">1</span>`) {
		t.Errorf("the Recent count is not the one ref")
	}
}

// listPartial is the /ui/agents partial (RenderAgentsList) as a string.
func listPartial(t *testing.T, list []AgentCardView, mirrors []MirrorCardView, recent []RecentRef) string {
	t.Helper()
	var b strings.Builder
	if err := RenderAgentsList(&b, list, mirrors, recent, 0); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestTheAgentsTabDrawsNoFAB: operator ask "hide the agent replies FAB and
// deploy agent FAB on agent list page". The Agents tab's document draws no
// dispatch FAB at all (#fab-agents), and draws the replies FAB (#notif-fab)
// HIDDEN — server-side, so it is not painted before the router runs — marked
// for the router to hide on a client-side switch to Agents and show again on
// the way out. The Tasks document is the control: its own FAB and the replies
// FAB are shown. Red at origin/main: both FABs drawn on /agents.
func TestTheAgentsTabDrawsNoFAB(t *testing.T) {
	notif := regexp.MustCompile(`<button id="notif-fab"[^>]*>`)
	agents := renderString(t, Page("agents", featuresAllOn))
	if strings.Contains(agents, `id="fab-agents"`) {
		t.Error("the Agents tab still draws the dispatch FAB")
	}
	tag := notif.FindString(agents)
	if tag == "" {
		t.Fatal("no replies FAB in the shell at all (it is still needed on the other tabs)")
	}
	if !strings.Contains(tag, ` hidden"`) || !strings.Contains(tag, fabHiddenOnTabAttr+`="agents"`) {
		t.Errorf("the Agents tab's replies FAB is not drawn hidden and marked for the router: %s", tag)
	}
	tasks := renderString(t, Page("tasks", featuresAllOn))
	if tag := notif.FindString(tasks); strings.Contains(tag, ` hidden"`) {
		t.Errorf("CONTROL: the Tasks tab hides the replies FAB: %s", tag)
	}
	if !regexp.MustCompile(`<button id="fab-tasks"[^>]*class="[^"]*"`).MatchString(tasks) || strings.Contains(regexp.MustCompile(`<button id="fab-tasks"[^>]*>`).FindString(tasks), " hidden") {
		t.Error("CONTROL: the Tasks tab's own FAB is not drawn shown")
	}
	// The router hides it on a switch to the tab its attribute names.
	b, err := fs.ReadFile(web.Files, strings.TrimPrefix(appScriptSrc, "/"))
	if err != nil {
		t.Fatal(err)
	}
	app := string(b)
	if !strings.Contains(app, `document.querySelectorAll('[`+fabHiddenOnTabAttr+`]')`) || !strings.Contains(app, `f.classList.toggle('hidden', off);`) {
		t.Error("the router does not toggle the launchers marked " + fabHiddenOnTabAttr)
	}
}

// TestTheDockedSwitchersScrollPaddingIsItsRoom: while the Agents panel is
// shown, the document's scroll padding at the bottom is exactly the room the
// panel reserves for the docked bar (switcherRoom), so an element scrolled into
// view — a focused card, a tapped one — stops above the bar, not under it.
// Read from the stylesheet source, because the rule is hand-written there.
func TestTheDockedSwitchersScrollPaddingIsItsRoom(t *testing.T) {
	css, err := os.ReadFile("../../web/css/input.css")
	if err != nil {
		t.Fatal(err)
	}
	room := strings.TrimSuffix(strings.TrimPrefix(switcherRoom(true), "pb-[calc("), ")]")
	if room != "2.8125rem+env(safe-area-inset-bottom)" {
		t.Fatalf("switcherRoom(true) = %q: update the stylesheet's scroll padding with it", switcherRoom(true))
	}
	want := "html:has([data-switcher-room]:not(.hidden)) {\n  scroll-padding-bottom: calc(" + strings.ReplaceAll(room, "+", " + ") + ");\n}"
	if !strings.Contains(string(css), want) {
		t.Errorf("web/css/input.css lacks the docked switcher's scroll padding:\n%s", want)
	}
}
