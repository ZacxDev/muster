package ui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	g "maragu.dev/gomponents"
)

// THE SIDEBAR'S AGENT LIST AND ITS "More" (nav_agents.go).

// navNow is the fixtures' clock: year 2000, so no real date is in the tree.
var navNow = time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)

// navAgentFixture and navMirrorFixture are one sidebar's entries. Every
// activity time is distinct, and the order the sidebar must draw them in is
// none of: the order given, orderList's (pins first), recency alone, or
// "waiting" alone — so a comparison that drops any one rule reorders them.
func navAgentFixture() []AgentCardView {
	ago := func(d time.Duration) time.Time { return navNow.Add(-d) }
	return []AgentCardView{
		{ID: 41, Name: "pinned-old", DisplayName: "pinned-old", Pinned: true, LastActiveAt: ago(50 * time.Minute)},
		{ID: 42, Name: "asker", DisplayName: "asker", LastActiveAt: ago(4 * time.Hour),
			Attention: &CardAttention{Count: 1, Kind: "ask", Kinds: []string{"ask"}, Href: "/attention#ask-1"}},
		{ID: 43, Name: "chief-a", DisplayName: "chief-a", IsChief: true, LastActiveAt: ago(5 * time.Hour)},
		{ID: 44, Name: "approver", DisplayName: "approver", AwaitingApproval: true, LastActiveAt: ago(6 * time.Hour)},
		// A reply draft is counted, but nothing is blocked on it: not waiting.
		{ID: 45, Name: "drafter", DisplayName: "drafter", LastActiveAt: ago(20 * time.Minute),
			Attention: &CardAttention{Count: 2, Kind: "plugin", Kinds: []string{"plugin"}, Href: "/attention#d-1"}},
	}
}

func navMirrorFixture() []MirrorCardView {
	return []MirrorCardView{
		{Host: "laptop", Session: "s-unknown", Window: "@7", Label: "unknown:1", Online: true},
		{Host: "laptop", Session: "s-fresh", Window: "@8", Label: "fresh:1", Online: true, Claude: true,
			ActiveAt: navNow.Add(-time.Minute), Color: "#3366cc"},
		{Host: "laptop", Session: "s-prompt", Window: "@9", Label: "prompt:1", Online: true, Claude: true,
			ActiveAt: navNow.Add(-3 * time.Hour), Attention: &CardAttention{Count: 1, Kind: "session", Kinds: []string{"session"}, Href: "/attention#w-1"}},
	}
}

var navRowRe = regexp.MustCompile(`<a href="([^"]*)"[^>]*\btitle="([^"]*)"`)

// navRows is the rendered list's rows, in order: each row's label.
func navRows(t *testing.T, html string) []string {
	t.Helper()
	var out []string
	for _, m := range navRowRe.FindAllStringSubmatch(html, -1) {
		out = append(out, m[2])
	}
	return out
}

// TestTheSidebarListsTheChiefThenWhoIsWaitingThenByRecency: the chief first;
// then every entry waiting on the operator, most recent first; then the rest,
// most recent first, a pin lifting nothing; an entry of unknown activity last.
// Read off the RENDERED partial, so it is the order a person sees.
func TestTheSidebarListsTheChiefThenWhoIsWaitingThenByRecency(t *testing.T) {
	html := renderString(t, NavAgentsList(navAgentFixture(), navMirrorFixture(), "", navNow))
	got := strings.Join(navRows(t, html), ",")
	want := "chief-a,prompt,asker,approver,fresh,drafter,pinned-old,unknown"
	if got != want {
		t.Fatalf("the sidebar's rows are %s\nwant %s\n(the chief, then waiting by recency, then the rest by recency, unknown activity last)", got, want)
	}
	// CONTROL: the Agents list's own order is a different one (pins first), so
	// the assertion above is about navOrder, not about the order the list
	// arrives in.
	var list []string
	for _, e := range orderList(navAgentFixture(), navMirrorFixture()) {
		s, _ := switcherName(e)
		list = append(list, s)
	}
	if strings.Join(list, ",") == want {
		t.Fatal("CONTROL: orderList already yields the sidebar's order, so this test cannot see navOrder")
	}
}

// TestTheSwitcherAndTheSidebarShareTheWaitingPredicate: the switcher's waiting
// dot and the sidebar's waiting rows are listEntry.waiting — the same set.
func TestTheSwitcherAndTheSidebarShareTheWaitingPredicate(t *testing.T) {
	sw := NewSwitcher(navAgentFixture(), navMirrorFixture(), nil, SwitcherCurrent{})
	var swWait []string
	for _, it := range sw.All {
		if it.Waiting {
			swWait = append(swWait, it.Label)
		}
	}
	html := renderString(t, NavAgentsList(navAgentFixture(), navMirrorFixture(), "", navNow))
	var navWait []string
	for _, m := range regexp.MustCompile(`<a [^>]*data-nav-wait[^>]*title="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		navWait = append(navWait, m[1])
	}
	want := "prompt,asker,approver"
	if strings.Join(navWait, ",") != want {
		t.Errorf("the sidebar marks %v waiting, want %s", navWait, want)
	}
	if len(swWait) != 3 {
		t.Errorf("the switcher marks %v waiting, want the sidebar's three", swWait)
	}
	for _, l := range swWait {
		if !strings.Contains(","+want+",", ","+l+",") {
			t.Errorf("the switcher marks %q waiting and the sidebar does not", l)
		}
	}
}

// TestTheRowCarriesTheCardsSignals: a row has the card's identity colour on its
// edge (only with a colour), its recency dot, its attention count — not a
// link, since the row is one — and opens what its card opens.
func TestTheRowCarriesTheCardsSignals(t *testing.T) {
	html := renderString(t, NavAgentsList(navAgentFixture(), navMirrorFixture(), "", navNow))
	row := func(label string) string {
		i := strings.Index(html, `title="`+label+`"`)
		if i < 0 {
			t.Fatalf("no row %q", label)
		}
		start := strings.LastIndex(html[:i], "<a ")
		return html[start : start+strings.Index(html[start:], "</a>")]
	}
	fresh, drafter := row("fresh"), row("drafter")
	if !strings.Contains(fresh, `data-identity="#3366cc"`) || !strings.Contains(fresh, "border-l-[rgb(var(--id-bd))]") {
		t.Errorf("the coloured window's row is not tinted: %s", fresh)
	}
	if strings.Contains(drafter, "data-identity") || strings.Contains(drafter, "--id-bd") || !strings.Contains(drafter, "border-l-transparent") {
		t.Errorf("an uncoloured row is tinted: %s", drafter)
	}
	if !strings.Contains(fresh, `href="/mirror/view?`) || !strings.Contains(fresh, `data-recency="0"`) {
		t.Errorf("the fresh window's row neither opens its view nor carries the freshest dot: %s", fresh)
	}
	if !strings.Contains(drafter, `data-nav-attention`) || !strings.Contains(drafter, `data-count="2"`) || strings.Count(drafter, "<a ") != 1 {
		t.Errorf("the drafter's row does not carry its count as a non-link pill: %s", drafter)
	}
	if u := row("unknown"); strings.Contains(u, "data-recency") {
		t.Errorf("a row of unknown activity draws a recency dot: %s", u)
	}
	if !strings.Contains(row("chief-a"), "data-nav-chief") {
		t.Error("the chief's row is not marked")
	}
}

// TestTheRowOfThePageIsCurrent: the page the list was asked from (htmx's
// HX-Current-URL) marks exactly its own row; any other page marks none.
func TestTheRowOfThePageIsCurrent(t *testing.T) {
	cases := []struct{ from, want string }{
		{"http://x.test/agents/asker?session=3&from=list", "asker"},
		{"http://x.test/agents/approver/terminal", "approver"},
		{"http://x.test/mirror/view?host=laptop&session=s-fresh&window=%408", "fresh"},
		{"http://x.test/mirror/terminal?host=laptop&session=s-prompt&window=%409", "prompt"},
		{"http://x.test/mirror/view?host=laptop&session=s-fresh&window=%409", ""},
		{"http://x.test/agents", ""},
		{"http://x.test/agents/no-such-agent", ""},
		{"http://x.test/tasks", ""},
		{"%%", ""},
	}
	for _, tc := range cases {
		html := renderString(t, NavAgentsList(navAgentFixture(), navMirrorFixture(), tc.from, navNow))
		var cur []string
		for _, m := range regexp.MustCompile(`<a [^>]*data-nav-current[^>]*title="([^"]*)"`).FindAllStringSubmatch(html, -1) {
			cur = append(cur, m[1])
		}
		if n := strings.Count(html, `aria-current="true"`); n != len(cur) {
			t.Errorf("from %q: %d aria-current, %d marked rows", tc.from, n, len(cur))
		}
		if strings.Contains(html, `aria-current="page"`) {
			t.Errorf("from %q: a row claims aria-current=page (the nav entry's)", tc.from)
		}
		if got := strings.Join(cur, ","); got != tc.want {
			t.Errorf("from %q: current rows %q, want %q", tc.from, got, tc.want)
		}
	}
}

// navFeatures turns on every optional entry the sidebar can draw.
var navFeatures = Features{GitHub: true, Connectors: true, Admin: true, Attention: true, Push: true}

// navDocuments is every kind of document that draws the nav, with every entry on.
func navDocuments() map[string]g.Node {
	nav := NavChrome{Feat: navFeatures}
	return map[string]g.Node{
		"shell /tasks":   Page("tasks", navFeatures),
		"shell /agents":  Page("agents", navFeatures),
		"NavDocument":    NavDocument(NavDocumentView{Title: "Account", Entry: NavAccount, Feat: navFeatures, Main: "<h1>Account</h1>"}),
		"AgentDetail":    AgentDetailPage(sampleAgentDetailView(), navFeatures),
		"MirrorViewPage": MirrorViewPage(MirrorViewView{Host: "laptop", Session: "work", Interactive: true, Nav: nav}),
		"AgentTerminal":  AgentTerminalPage(AgentTerminalView{Name: "keen-wren", RenderedHref: "/agents/keen-wren", Nav: nav}),
	}
}

// moreParts splits a document's "More" into its open tag, its <summary> (what
// shows while it is shut) and its body, and returns the sidebar panel with
// More cut out.
func moreParts(t *testing.T, src string) (open, summary, body, rest string) {
	t.Helper()
	panel := sidebarPanel(t, src)
	i := strings.Index(panel, `<details id="`+navMoreID+`"`)
	if i < 0 {
		t.Fatalf("the sidebar has no <details id=%q>", navMoreID)
	}
	end := strings.Index(panel[i:], "</details>")
	if end < 0 {
		t.Fatal("the More <details> is not closed")
	}
	more := panel[i : i+end]
	open = more[:strings.Index(more, ">")+1]
	s, e := strings.Index(more, "<summary"), strings.Index(more, "</summary>")
	if s < 0 || e < s {
		t.Fatalf("the More <details> has no <summary>: %.300s", more)
	}
	return open, more[s:e], more[e:], panel[:i] + panel[i+end:]
}

// TestMoreHoldsThePagesWithTheAsksCountOnItsHeader: on every nav document the
// pages — every visible tab, Asks, Install, the theme, Account, Admin — are
// inside the collapsed More and nowhere else in the sidebar; More is drawn
// shut; and the Asks count is in its <summary>, the part a shut <details>
// still shows — never in its body.
func TestMoreHoldsThePagesWithTheAsksCountOnItsHeader(t *testing.T) {
	pages := []string{`id="sidebar-attention"`, `data-install-app`, `data-theme-toggle`, `id="sidebar-account"`, `id="sidebar-admin"`}
	for _, tb := range visibleTabs(navFeatures) {
		pages = append(pages, `data-tab="`+tb.Key+`"`)
	}
	if len(pages) < 8 {
		t.Fatalf("CONTROL: only %d pages to look for", len(pages))
	}
	for name, doc := range navDocuments() {
		src := documentSource(t, doc)
		open, summary, body, rest := moreParts(t, src)
		if strings.Contains(open, " open") {
			t.Errorf("%s: More is drawn open: %s", name, open)
		}
		for _, p := range pages {
			if !strings.Contains(body, p) {
				t.Errorf("%s: More does not hold %s", name, p)
			}
			if strings.Contains(rest, p) || strings.Contains(summary, p) {
				t.Errorf("%s: %s is in the sidebar outside More's body", name, p)
			}
		}
		if !strings.Contains(summary, `id="attention-badge-slot"`) || !strings.Contains(summary, `id="attention-badge"`) {
			t.Errorf("%s: the Asks count is not on More's header: %s", name, summary)
		}
		if strings.Contains(body, "attention-badge") {
			t.Errorf("%s: the Asks count is inside More's collapsed body", name)
		}
		if n := strings.Count(src, `id="attention-badge-slot"`); n != 1 {
			t.Errorf("%s: %d Asks count slots, want 1", name, n)
		}
		if !strings.Contains(rest, `id="`+NavAgentsListID+`"`) {
			t.Errorf("%s: the agent list is not in the sidebar outside More", name)
		}
	}
	// With no attention queue there is no count to show, and the header is just
	// "More".
	_, summary, _, _ := moreParts(t, documentSource(t, Page("tasks", Features{})))
	if strings.Contains(summary, "attention-badge") {
		t.Error("with no attention queue, More's header still draws the Asks count")
	}
}

// TestMoreRemembersItsStateOnThisDevice: the script right after More restores
// its open state from this device's storage and saves each toggle, both inside
// try/catch (storage can throw).
func TestMoreRemembersItsStateOnThisDevice(t *testing.T) {
	src := renderString(t, Page("tasks", navFeatures))
	i := strings.Index(src, `</details>`+"<script>")
	if i < 0 {
		t.Fatal("no script straight after More")
	}
	js := src[i : i+strings.Index(src[i:], "</script>")]
	for _, want := range []string{"localStorage.getItem('" + navMoreKey + "')==='1'", "d.open=true", "addEventListener('toggle'", "localStorage.setItem('" + navMoreKey + "',d.open?'1':'0')"} {
		if !strings.Contains(js, want) {
			t.Errorf("More's script lacks %q: %s", want, js)
		}
	}
	if n := strings.Count(js, "try{"); n != 2 {
		t.Errorf("More's script has %d try blocks, want 2 (the read and the write): %s", n, js)
	}
}

// TestTheListLoadsWhenAnOverlaySidebarOpens: a page with the desktop rail
// loads the list on load (and keeps it live); a page whose sidebar is an
// overlay (the mirrored view, the terminal) only when it is opened — the event
// appScript's setOpen fires.
func TestTheListLoadsWhenAnOverlaySidebarOpens(t *testing.T) {
	trig := regexp.MustCompile(`id="` + NavAgentsListID + `"[^>]*hx-trigger="([^"]*)"`)
	for name, doc := range navDocuments() {
		src := renderString(t, doc)
		m := trig.FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("%s: the list slot has no hx-trigger", name)
		}
		overlay := name == "MirrorViewPage" || name == "AgentTerminal"
		want := navAgentsRailTrigger
		if overlay {
			want = navAgentsOverlayTrigger
		}
		if m[1] != want {
			t.Errorf("%s: the list's trigger is %q, want %q", name, m[1], want)
		}
		if !strings.Contains(src, "if (open) document.dispatchEvent(new CustomEvent('"+NavOpenEvent+"'));") {
			t.Errorf("%s: the sidebar's script does not fire %s when it opens", name, NavOpenEvent)
		}
	}
}
