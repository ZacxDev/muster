package ui

import (
	"slices"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// THE OFFICERS GROUP'S PLACE IN THE SIDEBAR (nav_officers.go's header): the
// top rows (the chief, then every own session waiting on the operator), THEN
// the Officers group, THEN the rest of the list, the team groups and Idle.
//
// Asserted on structure — ids and data attributes, never words — so a reword
// of a heading cannot walk it, and on BOTH the sections' order and which row
// sits in which, so a sidebar that drew the sections in order but put a
// waiting session below the group still fails.

// navSection is which sidebar section an element opens, "" for none: the top
// rows (NavTopRowsID), the Officers group, the rest of the list (the <ul>
// labelled by the "Agents" head that is not the top rows), a team's group,
// Superseded, Idle.
func navSection(n *html.Node) string {
	if n.Type != html.ElementNode {
		return ""
	}
	id, _ := ccAttr(n, "id")
	switch id {
	case NavTopRowsID:
		return "top"
	case NavOfficersGroupID:
		return "officers"
	case NavSupersededID:
		return "superseded"
	case NavIdleID:
		return "idle"
	}
	if _, ok := ccAttr(n, "data-nav-team"); ok {
		return "team"
	}
	if v, _ := ccAttr(n, "aria-labelledby"); v == navAgentsLabelID && n.Data == "ul" {
		return "rest"
	}
	return ""
}

// navSections is the sidebar's sections in document order, and each session
// row's (data-nav-agent's id) section.
func navSections(root *html.Node) ([]string, map[string][]string) {
	var order []string
	rows := map[string][]string{}
	var walk func(n *html.Node, in string)
	walk = func(n *html.Node, in string) {
		if s := navSection(n); s != "" {
			order = append(order, s)
			in = s
		}
		if n.Type == html.ElementNode {
			if _, ok := ccAttr(n, "data-nav-agent"); ok {
				id, _ := ccAttr(n, "id")
				rows[in] = append(rows[in], id)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, in)
		}
	}
	walk(root, "")
	return order, rows
}

// TestTheOfficersGroupIsBelowTheWaitingRowsAndAboveTheList: the chief and the
// two own sessions that wait (an ordinary agent at a permission prompt and an
// officer's instance) are drawn first, then the Officers group, then the rest
// of the viewer's list, then the team's group, then Idle. Every activity time
// is distinct, and the ordinary waiting agent (2h ago) is more recent than the
// waiting instance (20h ago) but older than both rest rows (1m, 5m), so a
// sidebar that sorted the top rows by recency alone would not pass either.
func TestTheOfficersGroupIsBelowTheWaitingRowsAndAboveTheList(t *testing.T) {
	list, offs := navOfficerFixture()
	ago := func(d time.Duration) time.Time { return navNow.Add(-d) }
	list = append(list,
		AgentCardView{ID: 45, Name: "own-blocked", DisplayName: "own-blocked", AwaitingApproval: true, LastActiveAt: ago(2 * time.Hour)},
		AgentCardView{ID: 62, Name: "team-helper", DisplayName: "team-helper", TeamOwner: navOffTeam, Team: navOffTName, LastActiveAt: ago(3 * time.Minute)},
	)
	root, _ := navOfficersRender(t, list, offs, "")
	order, rows := navSections(root)
	if want := []string{"top", "officers", "rest", "team", "idle"}; !slices.Equal(order, want) {
		t.Fatalf("the sidebar's sections are %v, want %v (the waiting rows, then the Officers group, then the list)", order, want)
	}
	for sec, want := range map[string][]string{
		"top":  {"nav-agent-7", "nav-agent-45", "nav-agent-53"},
		"rest": {"nav-agent-42", "nav-agent-44"},
		"team": {"nav-agent-62"},
		"idle": {"nav-agent-43"},
	} {
		if got := rows[sec]; !slices.Equal(got, want) {
			t.Errorf("the %s rows are %v, want %v", sec, got, want)
		}
	}
	if got := rows["officers"]; len(got) != 0 {
		t.Errorf("session rows %v are drawn inside the Officers group", got)
	}
	// The own waiting instance is above the group: its row's marker points up.
	_, by := navOfficerRows(root)
	if w := ccWithAttr(navOfficerRowOf(t, by, navOffAudit), "data-nav-officer-waiting", ""); len(w) != 1 || ccText(w[0]) != "↑ waiting" {
		t.Errorf("the audit desk's row is not marked \"↑ waiting\"")
	}
}

// TestWithNoWaitingRowsTheOfficersGroupLeads: with no chief and nobody
// waiting there are no top rows, and the Officers group is the first thing
// drawn, above the whole list.
func TestWithNoWaitingRowsTheOfficersGroupLeads(t *testing.T) {
	ago := func(d time.Duration) time.Time { return navNow.Add(-d) }
	list := []AgentCardView{
		{ID: 81, Name: "plain-a", DisplayName: "plain-a", LastActiveAt: ago(4 * time.Minute)},
		{ID: 82, Name: "plain-b", DisplayName: "plain-b", LastActiveAt: ago(9 * time.Minute)},
	}
	offs := []NavOfficer{{Key: navOffDocs, ID: "scout:docs", Name: "Docs scout", Href: "/officers/scout:docs"}}
	root, _ := navOfficersRender(t, list, offs, "")
	order, rows := navSections(root)
	if want := []string{"officers", "rest"}; !slices.Equal(order, want) {
		t.Fatalf("the sidebar's sections are %v, want %v", order, want)
	}
	if want := []string{"nav-agent-81", "nav-agent-82"}; !slices.Equal(rows["rest"], want) {
		t.Errorf("the list's rows are %v, want %v", rows["rest"], want)
	}
}
