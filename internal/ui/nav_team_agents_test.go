package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// A TEAM'S AGENTS IN THE SIDEBAR (team sharing 4.4, nav_agents.go): since
// 0131 the viewer's list holds their teams' agents, the team's chief
// included. They are the team's, drawn in the team's ONE group — the one its
// hosts' sessions are drawn in — never in the viewer's own list.

// Fixture ids: the team (navTeamOwner) is none of the agents' ids (7, 40, 41,
// 42, 43), so a mutant that keys a group by an agent id, or hardcodes one,
// cannot land a row in the right group.
const (
	navTeamOwner int64 = 900
	navTeamName        = "Platform"
)

func navTeamFixture() ([]AgentCardView, []MirrorCardView) {
	ago := func(d time.Duration) time.Time { return navNow.Add(-d) }
	list := []AgentCardView{
		// The viewer's own chief, and two own agents: one active, one idle.
		{ID: 7, Name: "chief-901", DisplayName: "chief", IsChief: true, PrimaryChief: true, LastActiveAt: ago(3 * time.Hour)},
		{ID: 42, Name: "own-active", DisplayName: "own-active", LastActiveAt: ago(time.Minute)},
		{ID: 43, Name: "own-idle", DisplayName: "own-idle", Idle: true, LastActiveAt: ago(48 * time.Hour)},
		// The team's chief (its reserved slug, labelled "chief" like the
		// viewer's) and a team agent that is IDLE: a team agent is never in
		// the viewer's Idle group.
		{ID: 40, Name: "chief-900", DisplayName: "chief", IsChief: true, TeamOwner: navTeamOwner, Team: navTeamName, LastActiveAt: ago(2 * time.Hour)},
		{ID: 41, Name: "keen-wren", DisplayName: "keen-wren", Idle: true, TeamOwner: navTeamOwner, Team: navTeamName, LastActiveAt: ago(30 * time.Hour)},
	}
	mirrors := []MirrorCardView{
		{Owner: navTeamOwner, Team: navTeamName, Host: "tower", Session: "build", Window: "@3", Label: "build:1", Online: true, ActiveAt: ago(10 * time.Minute)},
	}
	return list, mirrors
}

// navGroupsOf is the rendered list's rows (each anchor's id), by the group
// each is drawn in: "own" (the viewer's list), "team:<owner>", "idle",
// "superseded". Read off the parsed DOM — which container holds the row —
// never off the words.
func navGroupsOf(t *testing.T, markup string) map[string][]string {
	t.Helper()
	root := ccParse(t, markup)
	out := map[string][]string{}
	for _, a := range ccWithAttr(root, "data-nav-agent", "") {
		id, _ := ccAttr(a, "id")
		group := ""
		for p := a.Parent; p != nil && group == ""; p = p.Parent {
			if p.Type != html.ElementNode {
				continue
			}
			if v, ok := ccAttr(p, "data-nav-team"); ok {
				group = "team:" + v
			} else if v, _ := ccAttr(p, "id"); v == NavIdleID {
				group = "idle"
			} else if v == NavSupersededID {
				group = "superseded"
			} else if v, _ := ccAttr(p, "aria-labelledby"); v == navAgentsLabelID {
				group = "own"
			}
		}
		if group == "" {
			t.Fatalf("row %s is in no group", id)
		}
		out[group] = append(out[group], id)
	}
	return out
}

// TestATeamsAgentsAreDrawnInTheTeamsOneGroup: the viewer's own chief leads
// their own list (data-nav-chief); the team's chief and its agent are in the
// team's group, the SAME group as the team host's session (one heading for
// the team); the own list holds no team agent, and the team's idle agent is
// not in the viewer's Idle group. The team's chief does not carry
// data-nav-chief: a[data-nav-chief] is only the viewer's.
//
// 🔴 RED at origin/main 41f8bc34 (this test, with AgentCardView's TeamOwner
// and Team added): the team's chief and agent were in the viewer's own list
// and Idle group, two rows carried data-nav-chief.
func TestATeamsAgentsAreDrawnInTheTeamsOneGroup(t *testing.T) {
	list, mirrors := navTeamFixture()
	var b strings.Builder
	if err := RenderNavAgents(&b, list, mirrors, "", navNow); err != nil {
		t.Fatal(err)
	}
	markup := b.String()
	groups := navGroupsOf(t, markup)
	team := "team:900"
	mirrorRow := "nav-" + mirrorCardID(mirrors[0])

	if want := []string{"nav-agent-7", "nav-agent-42"}; !slices.Equal(groups["own"], want) {
		t.Errorf("the viewer's own list is %v, want %v (their chief first, no team agent)", groups["own"], want)
	}
	if want := []string{"nav-agent-43"}; !slices.Equal(groups["idle"], want) {
		t.Errorf("the Idle group is %v, want %v (the viewer's own idle agent only)", groups["idle"], want)
	}
	// The team's chief first (navOrder), then the rest of the team's by recency.
	if want := []string{"nav-agent-40", mirrorRow, "nav-agent-41"}; !slices.Equal(groups[team], want) {
		t.Errorf("the team's group is %v, want %v", groups[team], want)
	}
	for k := range groups {
		if k != "own" && k != "idle" && k != team {
			t.Errorf("unexpected group %s: %v", k, groups[k])
		}
	}
	root := ccParse(t, markup)
	if n := len(ccWithAttr(root, "data-nav-team", "")); n != 1 {
		t.Errorf("%d team groups (headings), want exactly 1 for the one team", n)
	}
	chiefs := ccWithAttr(root, "data-nav-chief", "")
	if len(chiefs) != 1 {
		t.Fatalf("%d rows carry data-nav-chief, want 1 (the viewer's own)", len(chiefs))
	}
	if id, _ := ccAttr(chiefs[0], "id"); id != "nav-agent-7" {
		t.Errorf("data-nav-chief is on %s, want the viewer's own chief nav-agent-7", id)
	}
	// The team's chief is told apart by its group (asserted above), never
	// by the viewer's marker.
	for _, a := range ccWithAttr(root, "id", "nav-agent-40") {
		if _, ok := ccAttr(a, "data-nav-chief"); ok {
			t.Errorf("the team's chief nav-agent-40 carries data-nav-chief, which marks only the viewer's own")
		}
	}
}

// TestTheSwitcherTellsTheViewersChiefFromTheTeams: the switcher's items —
// what its pills, the dock and the command palette (data-switcher-name on
// each sidebar row) show — label the team's chief with its team, so two
// "chief" entries are not one name. The viewer's own is "chief" alone. The
// team's row under its heading shows the bare name (InTeam).
func TestTheSwitcherTellsTheViewersChiefFromTheTeams(t *testing.T) {
	list, mirrors := navTeamFixture()
	label := map[int64]SwitcherItem{}
	es := navOrder(list, mirrors)
	for i, it := range switcherItems(es, SwitcherCurrent{}, navNow) {
		if es[i].agent != nil {
			label[es[i].agent.ID] = it
		}
	}
	if got := label[7].Label; got != "chief" {
		t.Errorf("the viewer's chief is labelled %q, want \"chief\"", got)
	}
	if got := label[40].Label; got != "chief · Platform" {
		t.Errorf("the team's chief is labelled %q, want \"chief · Platform\"", got)
	}
	if got := label[40].InTeam; got != "chief" {
		t.Errorf("the team's chief under its heading is %q, want \"chief\"", got)
	}
	// A name no other entry shares is not qualified.
	if got := label[41].Label; got != "keen-wren" {
		t.Errorf("the team's unshared agent is labelled %q, want \"keen-wren\"", got)
	}
	if !strings.Contains(label[40].Words, "platform") {
		t.Errorf("the team's chief's search words %q do not carry its team", label[40].Words)
	}
}

// TestATeamsEntriesAreNamedForTheirTeam: the team's chief's sidebar row
// SHOWS the bare name under its team's heading (SwitcherItem.InTeam) while
// what the palette reads (data-switcher-name) keeps the team; a team HOST
// SESSION whose name clashes with the viewer's own window is labelled with
// its team too (listEntry.team covers both kinds); and the team's name is a
// search word of the team's host session.
//
// Fixture values are pairwise distinct: the team ("Platform") is none of the
// hosts, sessions or labels, and the clashing windows are on different hosts
// ("tower", "atlas") with distinct sessions ("build", "relay").
func TestATeamsEntriesAreNamedForTheirTeam(t *testing.T) {
	list, mirrors := navTeamFixture()
	own := MirrorCardView{Host: "atlas", Session: "relay", Window: "@5", Label: "build:1", Online: true, ActiveAt: navNow.Add(-5 * time.Minute)}
	mirrors = append(mirrors, own)
	var b strings.Builder
	if err := RenderNavAgents(&b, list, mirrors, "", navNow); err != nil {
		t.Fatal(err)
	}
	root := ccParse(t, b.String())
	row := func(id string) *html.Node {
		t.Helper()
		ns := ccWithAttr(root, "id", id)
		if len(ns) != 1 {
			t.Fatalf("%d rows with id %s, want 1", len(ns), id)
		}
		return ns[0]
	}
	chief := row("nav-agent-40")
	if got := ccText(chief); got != "chief" {
		t.Errorf("the team's chief's row shows %q under its team's heading, want the bare \"chief\"", got)
	}
	if got, _ := ccAttr(chief, "data-switcher-name"); got != "chief · Platform" {
		t.Errorf("the team's chief's palette name is %q, want \"chief · Platform\"", got)
	}
	teamWin := row("nav-" + mirrorCardID(mirrors[0]))
	if got, _ := ccAttr(teamWin, "data-switcher-name"); got != "build:1 · Platform" {
		t.Errorf("the team's clashing window is named %q, want \"build:1 · Platform\"", got)
	}
	if got, _ := ccAttr(row("nav-"+mirrorCardID(own)), "data-switcher-name"); got != "build:1" {
		t.Errorf("the viewer's own clashing window is named %q, want \"build:1\"", got)
	}
	words, _ := ccAttr(teamWin, "data-switcher-words")
	if !slices.Contains(strings.Fields(words), "platform") {
		t.Errorf("the team's window's search words %q do not carry its team \"platform\"", words)
	}
}
