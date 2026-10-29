package ui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RETIRED AGENTS LIST'S CAPABILITIES, WHERE THEY WENT (agent_actions.go,
// nav_agents.go, onboarding.go, livecopy.go). Each test here is red at the
// base (dd25e646): none of these elements existed outside the list page.

// pageCard is an agent with every line its page can draw, and distinct values
// in every field a test reads.
func pageCard() AgentCardView {
	return AgentCardView{
		ID: 41, Name: "keen-wren", DisplayName: "Keen wren", Status: "running", Namespace: "ns-keen",
		KickoffFailed: true, KickoffFailure: "the turn timed out", KickoffAwaitingGrant: true,
		GitHubProblem: "the installation does not cover the repository", AwaitingApproval: true,
		Work: mirror.WindowWork{Subagents: 2, Shells: 1},
	}
}

// headerRows splits the agent page's <header> into its first row (the title's)
// and the rest.
func headerRows(t *testing.T, page string) (first, rest string) {
	t.Helper()
	h := page[strings.Index(page, "<header"):]
	h = h[:strings.Index(h, "</header>")]
	i := strings.Index(h, `data-agent-title=""`)
	if i < 0 {
		t.Fatalf("no title in the header:\n%s", h)
	}
	// The first row is the flex row holding the title; it ends where the
	// next row (the strip's) starts.
	j := strings.Index(h[i:], `id="`+AgentStripID+`"`)
	if j < 0 {
		t.Fatalf("no status strip in the header:\n%s", h)
	}
	return h[:i+j], h[i+j:]
}

// TestTheAgentPageCarriesWhatItsCardDid: the agent page's header draws the
// status strip (status, the awaiting-approval badge, the kickoff badge, the
// work chip) and the ⋯ menu (Rename, the lifecycle action, Delete) on its
// SECOND row — never the title's first row, whose room the operator keeps —
// and the notices above the transcript; for both kinds of agent.
func TestTheAgentPageCarriesWhatItsCardDid(t *testing.T) {
	for _, kind := range []string{agents.KindGateway, agents.KindClaudeCode} {
		v := sampleAgentDetailView()
		v.Kind, v.CCAccount = kind, "alpha"
		v.Card = pageCard()
		v.ID, v.Name = v.Card.ID, v.Card.Name
		page := renderString(t, AgentDetailPage(v, featuresAllOn))
		first, rest := headerRows(t, page)
		for _, id := range []string{AgentStripID, AgentMenuSlot, AgentMenuID + "-open"} {
			if strings.Contains(first, `id="`+id+`"`) {
				t.Errorf("%s: #%s is on the title's row", kind, id)
			}
			if !strings.Contains(rest, `id="`+id+`"`) {
				t.Errorf("%s: #%s is not on the header's second row", kind, id)
			}
		}
		for _, want := range []string{`data-agent-status="running"`, `data-badge="` + AwaitingApprovalBadgeID + `"`, "data-kickoff-failed", `data-mirror-work=""`} {
			if !strings.Contains(rest, want) {
				t.Errorf("%s: the strip lacks %s", kind, want)
			}
		}
		menu := page[strings.Index(page, `id="`+AgentMenuID+`"`):]
		menu = menu[:strings.Index(menu, "</details>")]
		for _, want := range []string{`data-agent-action="rename"`, `data-agent-action="Stop"`, `hx-post="/agents/41/stop"`, `data-agent-action="delete"`, `hx-delete="/agents/41"`} {
			if !strings.Contains(menu, want) {
				t.Errorf("%s: the ⋯ menu lacks %s", kind, want)
			}
		}
		if strings.Contains(menu, `data-agent-action="Start"`) {
			t.Errorf("%s: a running agent's menu offers Start", kind)
		}
		if !strings.Contains(page, `id="`+AgentRenameID+`"`) || !strings.Contains(page, `hx-post="/agents/41/name"`) {
			t.Errorf("%s: no rename form", kind)
		}
		notices := page[strings.Index(page, `id="`+AgentNoticesID+`"`):]
		notices = notices[:strings.Index(notices, `id="chat-log"`)]
		for _, want := range []string{"data-kickoff-failure", "data-kickoff-remedy", "data-kickoff-awaiting-grant", "data-github-problem", `href="/connectors"`} {
			if !strings.Contains(notices, want) {
				t.Errorf("%s: the notices lack %s", kind, want)
			}
		}
	}
}

// TestTheAgentMenuFollowsTheStatusAndTheDeleteGuard: a stopped agent is
// offered Start, a running one Stop; the chief and a protected officer
// instance are offered no Delete (the server refuses it anyway).
func TestTheAgentMenuFollowsTheStatusAndTheDeleteGuard(t *testing.T) {
	for _, c := range []struct {
		name          string
		a             AgentCardView
		action        string
		deleteOffered bool
	}{
		{"stopped", AgentCardView{ID: 3, Name: "a3", Status: "stopped"}, "Start", true},
		{"error", AgentCardView{ID: 4, Name: "a4", Status: "error"}, "Start", true},
		{"provisioning", AgentCardView{ID: 5, Name: "a5", Status: "provisioning"}, "Stop", true},
		{"the chief", AgentCardView{ID: 6, Name: "a6", Status: "running", IsChief: true}, "Stop", false},
		{"an officer's instance", AgentCardView{ID: 7, Name: "a7", Status: "stopped", DeleteProtected: true}, "Start", false},
	} {
		menu := renderString(t, agentActionsMenu(c.a))
		if !strings.Contains(menu, `data-agent-action="`+c.action+`"`) {
			t.Errorf("%s: the menu does not offer %s:\n%s", c.name, c.action, menu)
		}
		if got := strings.Contains(menu, "data-agent-action=\"confirm-delete\""); got != c.deleteOffered {
			t.Errorf("%s: Delete offered = %v, want %v", c.name, got, c.deleteOffered)
		}
	}
}

// TestTheStripRefreshRedrawsWhatTheStripShows: the strip's refresh (GET
// AgentHeaderPath) answers the strip's body — every badge and the chip it
// draws, so its innerHTML swap erases nothing it should keep — and the ⋯ menu
// and the notices out of band, into their own elements. A relationship pin:
// the strip and its refresh are one renderer (agentStripBody).
func TestTheStripRefreshRedrawsWhatTheStripShows(t *testing.T) {
	a := pageCard()
	a.KickoffFailed, a.KickoffOwed = false, true
	strip := renderString(t, agentStrip(a))
	var b strings.Builder
	if err := RenderAgentHeader(&b, a); err != nil {
		t.Fatal(err)
	}
	refresh := b.String()
	inner := strip[strings.Index(strip, ">")+1 : strings.LastIndex(strip, "</span>")]
	if !strings.HasPrefix(refresh, inner) {
		t.Errorf("the refresh does not start with the strip's body:\n strip:   %s\n refresh: %s", inner, refresh)
	}
	for _, want := range []string{"data-kickoff-owed", `data-badge="` + AwaitingApprovalBadgeID + `"`, `data-mirror-work=""`} {
		if !strings.Contains(inner, want) {
			t.Errorf("CONTROL: the strip lacks %s", want)
		}
	}
	for _, oob := range []string{`<div id="` + AgentMenuSlot + `" hx-swap-oob="innerHTML">`, `<div id="` + AgentNoticesID + `" hx-swap-oob="innerHTML">`} {
		if !strings.Contains(refresh, oob) {
			t.Errorf("the refresh lacks %s:\n%s", oob, refresh)
		}
	}
	if !strings.Contains(strip, `hx-get="`+AgentHeaderPath(41)+`"`) || !strings.Contains(strip, "agents:changed from:body") || !strings.Contains(strip, AgentsAwaitingEvent+" from:body") {
		t.Errorf("the strip does not refresh itself on a lifecycle action and on a stale poll:\n%s", strip)
	}
}

// TestTheSidebarOffersDispatchOrSaysWhy: the sidebar's Agents head draws the
// dispatch "+" (the task-less sheet, GET /ui/agents/new into the sheet every
// sidebar document carries) on a server that can dispatch, and the "not set
// up" hint on one that cannot — never both, never neither.
func TestTheSidebarOffersDispatchOrSaysWhy(t *testing.T) {
	on := renderString(t, sidebar("", "", false, Features{Dispatch: true}))
	off := renderString(t, sidebar("", "", false, Features{}))
	if !strings.Contains(on, `id="`+NavDeployID+`"`) || !strings.Contains(on, `hx-get="/ui/agents/new"`) || strings.Contains(on, DispatchOffHintAttr) {
		t.Errorf("with dispatch: the + is missing or the hint is drawn:\n%s", on)
	}
	if strings.Contains(off, `id="`+NavDeployID+`"`) || !strings.Contains(off, DispatchOffHintAttr) {
		t.Errorf("without dispatch: the + is drawn or the hint is missing")
	}
	for name, doc := range map[string]string{"with": on, "without": off} {
		if strings.Count(doc, `id="agent-modal"`) != 1 {
			t.Errorf("%s dispatch: the sidebar does not carry the one dispatch sheet", name)
		}
	}
}

// TestIdleEntriesAreInTheSidebarsIdleGroup: the sidebar draws the Idle
// section's entries last, in a shut "Idle (N)" group — but never one that is
// waiting on the operator, the chief, or the page's own session; and draws no
// group when nothing is idle.
func TestIdleEntriesAreInTheSidebarsIdleGroup(t *testing.T) {
	now := time.Date(2000, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	list := []AgentCardView{
		{ID: 11, Name: "idle-one", Status: "stopped", LastActiveAt: old.Add(time.Hour), Idle: true},
		{ID: 12, Name: "idle-waits", Status: "running", LastActiveAt: old, Idle: true, AwaitingApproval: true},
		{ID: 13, Name: "idle-chief", Status: "running", LastActiveAt: old, Idle: true, IsChief: true, PrimaryChief: true},
		{ID: 14, Name: "idle-here", Status: "running", LastActiveAt: old, Idle: true},
		{ID: 15, Name: "busy", Status: "running", LastActiveAt: now.Add(-time.Minute)},
	}
	mirrors := []MirrorCardView{{Host: "tower", Session: "w", Window: "@3", Label: "mango:3", Online: true, ActiveAt: old.Add(2 * time.Hour), Idle: true}}
	out := renderString(t, NavAgentsList(list, mirrors, "/agents/idle-here?session=4", now))
	i := strings.Index(out, `<details id="`+NavIdleID+`"`)
	if i < 0 {
		t.Fatalf("no Idle group:\n%s", out)
	}
	group := out[i:]
	if strings.Contains(group[:strings.Index(group, ">")], " open") {
		t.Error("the Idle group is drawn open")
	}
	var in []string
	for _, m := range regexp.MustCompile(`<a id="(nav-[^"]+)"`).FindAllStringSubmatch(group, -1) {
		in = append(in, m[1])
	}
	if got := strings.Join(in, ","); got != "nav-"+mirrorCardID(mirrors[0])+",nav-agent-11" {
		t.Errorf("the Idle group holds %s; want the idle window then idle-one (most recent first), and not the waiting, chief or current entries", got)
	}
	if !strings.Contains(group, `data-nav-idle-count="" class="tabular-nums">(2)<`) {
		t.Errorf("the group does not count 2")
	}
	if !strings.Contains(group, "'"+agentsIdleOpenKey+"'") {
		t.Errorf("the group's open state is not kept under %s", agentsIdleOpenKey)
	}
	none := renderString(t, NavAgentsList(list[4:], nil, "", now))
	if strings.Contains(none, NavIdleID) {
		t.Error("a sidebar with nothing idle draws an Idle group")
	}
}

// TestASidebarRowSaysOfflineAndWhatRuns: a mirrored row whose host is offline
// says "offline" (the retired card's chip), and a row whose entry has
// subagents or shells running carries the work mark with its whole sentence;
// an online, idle-handed row carries neither.
func TestASidebarRowSaysOfflineAndWhatRuns(t *testing.T) {
	now := time.Now()
	mirrors := []MirrorCardView{
		{Host: "tower", Session: "w", Window: "@1", Label: "mango:1", Online: false, ActiveAt: now},
		{Host: "tower", Session: "w", Window: "@2", Label: "wheat:2", Online: true, ActiveAt: now.Add(-time.Minute), Work: mirror.WindowWork{Subagents: 2, Shells: 1}},
		{Host: "tower", Session: "w", Window: "@3", Label: "vapor:3", Online: true, ActiveAt: now.Add(-2 * time.Minute)},
	}
	list := []AgentCardView{{ID: 9, Name: "worker", Status: "running", LastActiveAt: now.Add(-3 * time.Minute), Work: mirror.WindowWork{Shells: 1, ShellsAtLeast: true}}}
	out := renderString(t, NavAgentsList(list, mirrors, "", now))
	row := func(id string) string {
		r := out[strings.Index(out, `<a id="`+id+`"`):]
		return r[:strings.Index(r, "</a>")]
	}
	if r := row("nav-" + mirrorCardID(mirrors[0])); !strings.Contains(r, "data-nav-offline") {
		t.Errorf("an offline window's row does not say so:\n%s", r)
	}
	if r := row("nav-" + mirrorCardID(mirrors[1])); !strings.Contains(r, `data-nav-work="2 subagents · 1 shell"`) || !strings.Contains(r, `aria-label="2 subagents and 1 background shell running"`) || strings.Contains(r, "data-nav-offline") {
		t.Errorf("a working window's row lacks its work mark (or says offline):\n%s", r)
	}
	if r := row("nav-agent-9"); !strings.Contains(r, `data-nav-work="1+ shell"`) || !strings.Contains(r, ">1+<") {
		t.Errorf("an agent's row lacks its lower-bound work mark:\n%s", r)
	}
	if r := row("nav-" + mirrorCardID(mirrors[2])); strings.Contains(r, "data-nav-offline") || strings.Contains(r, "data-nav-work") {
		t.Errorf("a quiet online row carries a mark:\n%s", r)
	}
}

// TestHomeOpensTheLastSessionStillListed: with no chief, home is the session
// the viewer opened last that is still listed (refs newest first, a gone one
// skipped), else the sidebar's first row (whoever waits first), else nothing.
func TestHomeOpensTheLastSessionStillListed(t *testing.T) {
	now := time.Date(2000, 3, 1, 12, 0, 0, 0, time.UTC)
	list := []AgentCardView{
		{ID: 21, Name: "recent-one", SessionID: 71, LastActiveAt: now.Add(-time.Hour)},
		{ID: 22, Name: "waits", SessionID: 72, LastActiveAt: now.Add(-2 * time.Hour), AwaitingApproval: true},
	}
	mirrors := []MirrorCardView{{Host: "tower", Session: "w", Window: "@5", Label: "mango:5", Online: true, Claude: true, ActiveAt: now}}
	win := RecentRef{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@5"}}
	for _, c := range []struct {
		name string
		refs []RecentRef
		want string
	}{
		{"the newest ref, a window", []RecentRef{win, {AgentID: 21}}, MirrorCardHref(mirrors[0])},
		{"the newest ref, an agent", []RecentRef{{AgentID: 21}, win}, AgentListHref("recent-one", 71)},
		{"a gone ref is skipped", []RecentRef{{AgentID: 99}, {AgentID: 21}}, AgentListHref("recent-one", 71)},
		{"no refs: the sidebar's first row (who waits)", nil, AgentListHref("waits", 72)},
	} {
		if got := HomeHref(list, mirrors, c.refs); got != c.want {
			t.Errorf("%s: home = %q, want %q", c.name, got, c.want)
		}
	}
	if got := HomeHref(nil, nil, []RecentRef{{AgentID: 21}}); got != "" {
		t.Errorf("nothing listed: home = %q, want none", got)
	}
}

// TestAMirroredWindowsPagesSayTheHostIsOfflineAndItsNote: the Rendered view
// and the terminal draw the line under the header that the retired card's
// "offline" chip and host note were; an online window with no note draws none.
func TestAMirroredWindowsPagesSayTheHostIsOfflineAndItsNote(t *testing.T) {
	const note = "the view's token expires in 2 days: pair the host again"
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "w", Window: "@1", Label: "mango:1", Offline: true, Note: note}))
	term := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "w", DisplayName: "mango:1", Offline: true, Note: note}))
	for name, page := range map[string]string{"rendered view": view, "terminal": term} {
		if !strings.Contains(page, "data-mirror-offline") || !strings.Contains(page, escapeForHTML(MirrorOfflineText)) || !strings.Contains(page, ">"+escapeForHTML(note)+"<") {
			t.Errorf("%s: no offline line or no note", name)
		}
	}
	quiet := renderString(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "w", Window: "@1", Label: "mango:1"}))
	if strings.Contains(quiet, "mv-host-note") {
		t.Error("an online window with no note draws the line")
	}
}
