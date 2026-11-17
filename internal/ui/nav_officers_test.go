package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// THE SIDEBAR'S OFFICERS GROUP (nav_officers.go).
//
// Fixture values are pairwise distinct so no hardcoded literal can pass: the
// queued counts (3, 5, 1) are none of the spends or budgets; the budgets
// ($7.50, $12, $9) differ from each other and from every spend ($6.40,
// $13.25); one officer is over its budget and one under; the team (900) is
// none of the agents' ids; and an ordinary agent is NAMED like an officer's
// instance but carries no Officer (the server's binding rule said no).

const (
	navOffDocs  = "scout-k1"  // own, running (an idle instance), under budget
	navOffWeb   = "triage-k2" // own, asleep, over budget (held)
	navOffAudit = "audit-k3"  // own, its instance waits on the operator
	navOffOps   = "ops:watch" // the team's, asleep, spend unreadable
	navOffTeam  = int64(900)
	navOffTName = "Platform"
)

func navIntP(n int) *int           { return &n }
func navFloatP(v float64) *float64 { return &v }

func navOfficerFixture() ([]AgentCardView, []NavOfficer) {
	ago := func(d time.Duration) time.Time { return navNow.Add(-d) }
	list := []AgentCardView{
		{ID: 7, Name: "chief-901", DisplayName: "chief", IsChief: true, PrimaryChief: true, LastActiveAt: ago(3 * time.Hour)},
		{ID: 42, Name: "own-active", DisplayName: "own-active", LastActiveAt: ago(time.Minute)},
		{ID: 43, Name: "own-idle", DisplayName: "own-idle", Idle: true, LastActiveAt: ago(48 * time.Hour)},
		// Named like the docs scout's second instance, but NOT one: no Officer.
		{ID: 44, Name: "officer-scout-k1-2", DisplayName: "decoy", LastActiveAt: ago(5 * time.Minute)},
		// The docs scout's instance: running, and idle — never in Idle.
		{ID: 51, Name: "officer-scout-k1-1", DisplayName: "docs scout", Status: "running", Officer: navOffDocs, SessionID: 610, Idle: true, LastActiveAt: ago(40 * time.Hour)},
		// The audit desk's instance: running, waiting at a permission prompt.
		{ID: 53, Name: "officer-audit-k3-1", DisplayName: "audit desk", Status: "running", Officer: navOffAudit, SessionID: 630, AwaitingApproval: true, LastActiveAt: ago(20 * time.Hour)},
		// The web triage's instance row, stopped: the officer is asleep.
		{ID: 52, Name: "officer-triage-k2-1", DisplayName: "web triage", Status: "stopped", Officer: navOffWeb, SessionID: 620, LastActiveAt: ago(30 * time.Hour)},
	}
	// Deliberately out of the group's order.
	offs := []NavOfficer{
		{Key: navOffOps, ID: "ops:watch", Name: "Ops watch", TeamOwner: navOffTeam, Team: navOffTName, Href: "/officers/ops:watch?owner=900", Budget: 9},
		{Key: navOffWeb, ID: "triage:web", Name: "Web triage", Href: "/officers/triage:web", Queued: navIntP(5), Budget: 12, Spent: navFloatP(13.25), Held: true},
		{Key: navOffDocs, ID: "scout:docs", Name: "Docs scout", Href: "/officers/scout:docs", Queued: navIntP(3), Budget: 7.5, Spent: navFloatP(6.4)},
		{Key: navOffAudit, ID: "audit:desk", Name: "Audit desk", Href: "/officers/audit:desk", Queued: navIntP(1)},
	}
	return list, offs
}

func navOfficersRender(t *testing.T, list []AgentCardView, offs []NavOfficer, current string) (*html.Node, string) {
	t.Helper()
	var b strings.Builder
	if err := RenderNavAgentsWithOfficers(&b, list, nil, offs, current, navNow); err != nil {
		t.Fatal(err)
	}
	return ccParse(t, b.String()), b.String()
}

// navOfficerRows is the Officers group's rows, by data-nav-officer, in order.
func navOfficerRows(root *html.Node) ([]string, map[string]*html.Node) {
	var keys []string
	by := map[string]*html.Node{}
	for _, a := range ccWithAttr(root, "data-nav-officer", "") {
		k, _ := ccAttr(a, "data-nav-officer")
		keys = append(keys, k)
		by[k] = a
	}
	return keys, by
}

// navOfficerRowOf is officer k's row, failing the test when it has none.
func navOfficerRowOf(t *testing.T, by map[string]*html.Node, k string) *html.Node {
	t.Helper()
	a := by[k]
	if a == nil {
		t.Fatalf("no Officers row for %s", k)
	}
	return a
}

// navInGroup reports whether n is inside the Officers group.
func navInGroup(n *html.Node, id string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if v, _ := ccAttr(p, "id"); v == id {
			return true
		}
	}
	return false
}

// TestTheOfficersGroupHoldsOneRowPerOfficerInAStableOrder: one row per
// officer, in the group, the viewer's own first by name, then the team's —
// whatever order the roster arrives in; the group sits after the list and
// before Idle.
func TestTheOfficersGroupHoldsOneRowPerOfficerInAStableOrder(t *testing.T) {
	list, offs := navOfficerFixture()
	root, markup := navOfficersRender(t, list, offs, "")
	keys, by := navOfficerRows(root)
	if want := []string{navOffAudit, navOffDocs, navOffWeb, navOffOps}; !slices.Equal(keys, want) {
		t.Fatalf("the officer rows are %v, want %v", keys, want)
	}
	for k, a := range by {
		if !navInGroup(a, NavOfficersGroupID) {
			t.Errorf("officer %s's row is not in the Officers group", k)
		}
	}
	// Reversed input, same order.
	slices.Reverse(offs)
	root2, _ := navOfficersRender(t, list, offs, "")
	if keys2, _ := navOfficerRows(root2); !slices.Equal(keys2, keys) {
		t.Errorf("reversed roster drew %v, want %v", keys2, keys)
	}
	g, idle := strings.Index(markup, `id="`+NavOfficersGroupID+`"`), strings.Index(markup, `id="`+NavIdleID+`"`)
	own := strings.Index(markup, `aria-labelledby="`+navAgentsLabelID+`"`)
	if g < 0 || idle < 0 || own < 0 || !(own < g && g < idle) {
		t.Errorf("the group is not between the list and Idle (list %d, group %d, idle %d)", own, g, idle)
	}
	if h := ccWithAttr(root, "id", navOfficersLabelID); len(h) != 1 || ccText(h[0]) != "Officers" {
		t.Errorf("the group's heading is not one \"Officers\"")
	}
	if len(ccWithAttr(root, "data-nav-officers", "")) != 1 {
		t.Errorf("want exactly one Officers group")
	}
}

// TestAnOfficersInstanceIsNeverInIdleOrTheList: the docs scout's instance is
// idle, and is drawn neither in Idle nor as a session row — its officer's row
// is it, linking to its chat; the decoy NAMED like an instance is an ordinary
// row.
func TestAnOfficersInstanceIsNeverInIdleOrTheList(t *testing.T) {
	list, offs := navOfficerFixture()
	root, _ := navOfficersRender(t, list, offs, "")
	ids := map[string]*html.Node{}
	for _, a := range ccWithAttr(root, "data-nav-agent", "") {
		id, _ := ccAttr(a, "id")
		ids[id] = a
	}
	if _, ok := ids["nav-agent-51"]; ok {
		t.Error("the docs scout's idle instance has a session row of its own")
	}
	if _, ok := ids["nav-agent-52"]; ok {
		t.Error("the web triage's stopped instance has a session row of its own")
	}
	if a, ok := ids["nav-agent-44"]; !ok || navInGroup(a, NavOfficersGroupID) || navInGroup(a, NavIdleID) {
		t.Error("the agent named like an instance is not an ordinary row of the list")
	}
	idleRows := 0
	for _, a := range ccWithAttr(root, "data-nav-agent", "") {
		if navInGroup(a, NavIdleID) {
			idleRows++
			if id, _ := ccAttr(a, "id"); id != "nav-agent-43" {
				t.Errorf("Idle holds %s", id)
			}
		}
	}
	if idleRows != 1 {
		t.Errorf("Idle holds %d rows, want the one own idle agent", idleRows)
	}
	_, by := navOfficerRows(root)
	if h, _ := ccAttr(navOfficerRowOf(t, by, navOffDocs), "href"); h != AgentListHref("officer-scout-k1-1", 610) {
		t.Errorf("the running docs scout opens %q, want its instance's chat", h)
	}
	if s, _ := ccAttr(navOfficerRowOf(t, by, navOffDocs), "data-nav-officer-state"); s != "running" {
		t.Errorf("the docs scout reads %q, want running", s)
	}
}

// TestAnAsleepOfficerOpensItsPage: no awake instance (a stopped row, or none
// at all) — the row says asleep and opens /officers/....
func TestAnAsleepOfficerOpensItsPage(t *testing.T) {
	list, offs := navOfficerFixture()
	root, _ := navOfficersRender(t, list, offs, "")
	_, by := navOfficerRows(root)
	for k, want := range map[string]string{navOffWeb: "/officers/triage:web", navOffOps: "/officers/ops:watch?owner=900"} {
		a := navOfficerRowOf(t, by, k)
		if h, _ := ccAttr(a, "href"); h != want {
			t.Errorf("%s opens %q, want %q", k, h, want)
		}
		if s, _ := ccAttr(a, "data-nav-officer-state"); s != "asleep" {
			t.Errorf("%s reads %q, want asleep", k, s)
		}
	}
}

// TestAWaitingOfficerInstanceIsLiftedAndItsRowMarked: the audit desk's
// instance waits — it is drawn once, as a session row right under the chief,
// and its officer's row stays in the group, marked "↑ waiting". No other
// officer's row is marked.
func TestAWaitingOfficerInstanceIsLiftedAndItsRowMarked(t *testing.T) {
	list, offs := navOfficerFixture()
	root, _ := navOfficersRender(t, list, offs, "")
	var own []string
	for _, a := range ccWithAttr(root, "data-nav-agent", "") {
		if navInGroup(a, NavIdleID) {
			continue
		}
		id, _ := ccAttr(a, "id")
		own = append(own, id)
	}
	if want := []string{"nav-agent-7", "nav-agent-53", "nav-agent-42", "nav-agent-44"}; !slices.Equal(own, want) {
		t.Errorf("the list is %v, want %v (chief, the waiting instance, then by recency)", own, want)
	}
	_, by := navOfficerRows(root)
	for k, a := range by {
		marked := len(ccWithAttr(a, "data-nav-officer-waiting", "")) == 1
		if marked != (k == navOffAudit) {
			t.Errorf("%s waiting mark = %v", k, marked)
		}
	}
	if w := ccWithAttr(navOfficerRowOf(t, by, navOffAudit), "data-nav-officer-waiting", ""); len(w) == 1 && ccText(w[0]) != "↑ waiting" {
		t.Errorf("the mark reads %q", ccText(w[0]))
	}
}

// TestAnOfficerRowShowsItsTeamQueueSpendAndHold: the team's officer carries
// its team's name; each figure is the roster's own; the held mark only on the
// officer over its budget; nothing about spend with no budget or an
// unreadable spend.
func TestAnOfficerRowShowsItsTeamQueueSpendAndHold(t *testing.T) {
	list, offs := navOfficerFixture()
	root, _ := navOfficersRender(t, list, offs, "")
	_, by := navOfficerRows(root)
	facts := func(k string) string {
		f := ccWithAttr(navOfficerRowOf(t, by, k), "data-nav-officer-facts", "")
		if len(f) != 1 {
			return ""
		}
		return ccText(f[0])
	}
	spend := func(k string) (string, bool) {
		s := ccWithAttr(navOfficerRowOf(t, by, k), "data-nav-officer-spend", "")
		if len(s) != 1 {
			return "", false
		}
		return ccText(s[0]), true
	}
	held := func(k string) bool { return len(ccWithAttr(navOfficerRowOf(t, by, k), "data-nav-officer-held", "")) == 1 }

	cases := []struct {
		key, facts, spend string
		held              bool
	}{
		{navOffDocs, "running · 3 queued", "$6.40 / $7.50", false},
		{navOffWeb, "asleep · 5 queued", "$13.25 / $12", true},
		{navOffAudit, "running · 1 queued", "", false},
		{navOffOps, navOffTName + " · asleep", "", false},
	}
	for _, c := range cases {
		if got := facts(c.key); got != c.facts {
			t.Errorf("%s facts %q, want %q", c.key, got, c.facts)
		}
		got, ok := spend(c.key)
		if (c.spend != "") != ok || got != c.spend {
			t.Errorf("%s spend %q (drawn %v), want %q", c.key, got, ok, c.spend)
		}
		if held(c.key) != c.held {
			t.Errorf("%s held = %v, want %v", c.key, held(c.key), c.held)
		}
	}
	if v, _ := ccAttr(navOfficerRowOf(t, by, navOffOps), "data-nav-officer-team"); v != "900" {
		t.Errorf("the team's officer names team %q", v)
	}
	if _, ok := ccAttr(navOfficerRowOf(t, by, navOffDocs), "data-nav-officer-team"); ok {
		t.Error("an own officer names a team")
	}
}

// TestHeldNeedsAKnownSpend: a roster that says Held with no spend read (a
// composer bug) draws no hold — the mark is never drawn without its figure.
func TestHeldNeedsAKnownSpend(t *testing.T) {
	list, _ := navOfficerFixture()
	root, _ := navOfficersRender(t, list, []NavOfficer{{Key: "k", ID: "a:b", Href: "/officers/a:b", Budget: 4, Held: true}}, "")
	if len(ccWithAttr(root, "data-nav-officer-held", "")) != 0 || len(ccWithAttr(root, "data-nav-officer-spend", "")) != 0 {
		t.Error("a hold or a figure is drawn with the spend unknown")
	}
}

// TestNoOfficersNoGroup: no roster (no officers, or the server keeps none) —
// no group, and an instance is drawn as any agent is (here: idle, so in Idle).
func TestNoOfficersNoGroup(t *testing.T) {
	list, _ := navOfficerFixture()
	root, _ := navOfficersRender(t, list, nil, "")
	if len(ccWithAttr(root, "id", NavOfficersGroupID)) != 0 || len(ccWithAttr(root, "data-nav-officer", "")) != 0 {
		t.Fatal("an Officers group is drawn with no officers")
	}
	found := false
	for _, a := range ccWithAttr(root, "id", "nav-agent-51") {
		found = navInGroup(a, NavIdleID)
	}
	if !found {
		t.Error("with no group the idle instance is not an ordinary Idle row")
	}
}

// TestTheChiefIsNeverAnOfficerRow: even carrying an Officer, the chief stays
// pinned first in the list and is not taken into the group.
func TestTheChiefIsNeverAnOfficerRow(t *testing.T) {
	list, offs := navOfficerFixture()
	list[0].Officer = navOffDocs
	root, _ := navOfficersRender(t, list, offs, "")
	rows := ccWithAttr(root, "data-nav-agent", "")
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	if id, _ := ccAttr(rows[0], "id"); id != "nav-agent-7" {
		t.Errorf("the first row is %s, want the chief", id)
	}
}

// TestTheOfficersRowIsCurrentOnItsInstancesPage: on the docs scout's
// instance's page the officer's row is the current one.
func TestTheOfficersRowIsCurrentOnItsInstancesPage(t *testing.T) {
	list, offs := navOfficerFixture()
	root, _ := navOfficersRender(t, list, offs, "http://x/agents/officer-scout-k1-1?session=610")
	_, by := navOfficerRows(root)
	if v, _ := ccAttr(navOfficerRowOf(t, by, navOffDocs), "aria-current"); v != "true" {
		t.Errorf("the docs scout's row aria-current = %q, want true", v)
	}
	if v, ok := ccAttr(navOfficerRowOf(t, by, navOffWeb), "aria-current"); ok {
		t.Errorf("the web triage's row aria-current = %q", v)
	}
}

// TestOfficersWithNoAgentsStillDrawTheGroup: officers but no session yet.
func TestOfficersWithNoAgentsStillDrawTheGroup(t *testing.T) {
	_, offs := navOfficerFixture()
	root, _ := navOfficersRender(t, nil, offs, "")
	if keys, _ := navOfficerRows(root); len(keys) != len(offs) {
		t.Errorf("%d officer rows with no agents, want %d", len(keys), len(offs))
	}
}
