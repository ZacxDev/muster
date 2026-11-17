package ui

import (
	"sort"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SIDEBAR'S OFFICERS GROUP (operator decisions, officers in the sidebar):
//
//   - ONE ROW PER DECLARED OFFICER the viewer can see — their own and their
//     teams' — awake or asleep, all in ONE always-open "Officers" heading
//     group (the team groups' style, not a disclosure), drawn below the list
//     and the team groups and above Superseded and Idle. A team's officer is
//     labelled with its team's name. The roster is internal/api's
//     (navOfficers): cheap in-memory and batched reads, never a pod read.
//   - Each row says whether the officer is running or asleep, how many items
//     are queued for it, and — when it has a daily budget — the day's spend
//     against it ("$6.40 / $10") and "held" once the cap has stopped it for
//     the day. Nothing about spend when it has no budget, and no figure at all
//     when the spend could not be read (never a stale or zero one).
//   - It opens the live instance's chat while one runs, else the officer's
//     own page (/officers/...).
//   - 🔴 AN INSTANCE THAT WAITS ON THE OPERATOR IS DRAWN WHERE ANY WAITING
//     SESSION IS (the waiting rows at the top, or its team's group — both
//     above this group), and its officer's row stays, marked "↑ waiting". The
//     group thus always holds exactly one row per officer, and the waiting
//     session is never in it twice. An instance that does not wait has no row
//     of its own: its officer's row is it. Officers are never in Idle or
//     Superseded, and the chief is never treated as an officer.
//   - Which agent is an officer's instance is internal/api's ONE binding rule
//     (AgentCardView.Officer), never a name matched here.
//
// ⚠ A HOST OFFICER'S INSTANCES ARE WINDOWS, NOT AGENT ROWS (they are listed
// as mirrored sessions), so whether one runs is not known from the list: its
// row says neither running nor asleep (NavOfficer.OnHost) and opens its page.

// NavOfficersGroupID is the sidebar's Officers group (navOfficersGroup).
const NavOfficersGroupID = "sidebar-officers-group"

// navOfficersLabelID is the group's heading (its <ul> is labelled by it).
const navOfficersLabelID = "sidebar-officers-label"

// NavOfficer is one declared officer's row in the sidebar's Officers group,
// composed by internal/api's navOfficers.
type NavOfficer struct {
	// Key is the officer's registry key (officers.Officer.Key): what an
	// instance's AgentCardView.Officer names.
	Key string
	// ID is the officer's id (`<type>:<area>`); Name what it is called ("" is
	// its id).
	ID, Name string
	// TeamOwner is the team that owns it, 0 for the viewer's own; Team that
	// team's name.
	TeamOwner int64
	Team      string
	// Href is its page (/officers/{id}, with the team's owner= when a team's).
	Href string
	// OnHost: its instances are host windows, so running or asleep is not
	// known from the sidebar's list (see the header).
	OnHost bool
	// Queued is how many items are queued for it; nil when not known (an
	// officer on an external HQ, or the count could not be read).
	Queued *int
	// Budget is its daily budget in USD (officers.Budget.DailyUSD), 0 none.
	Budget float64
	// Spent is its spend for the UTC day, nil when it has no budget or the
	// spend could not be read.
	Spent *float64
	// Held: the budget has stopped it for the day — internal/api's answer of
	// officers.Officer.OverBudget over Spent, never re-derived here. Only ever
	// true with Spent known.
	Held bool
}

// navOfficerInstance is one officer instance's entry, as the list met it.
type navOfficerInstance struct {
	e  listEntry
	it SwitcherItem
}

// navOfficerKeys is the set of officer keys that have a row.
func navOfficerKeys(officers []NavOfficer) map[string]bool {
	out := make(map[string]bool, len(officers))
	for _, o := range officers {
		if o.Key != "" {
			out[o.Key] = true
		}
	}
	return out
}

// navOfficerInstanceDrawnHere reports whether an officer's instance is ALSO
// drawn as a session row of its own: exactly when it waits on the operator
// (listEntry.waiting, the one waiting predicate) — then it is lifted with
// every other waiting session (navOrder) and its officer's row marks it.
func navOfficerInstanceDrawnHere(e listEntry) bool { return e.waiting() }

// navOfficerRowStandsIn reports whether entry e has no session row of its own
// because its officer's row in the Officers group stands in for it: it is an
// officer's instance (AgentCardView.Officer), not the chief, and it does not
// wait on the operator. 🔴 ONE PREDICATE for the sidebar (NavAgentsWithOfficers,
// which also needs the officer to HAVE a row) and home's first-row fallback
// (HomeHref), so `/` never opens a row the sidebar does not draw.
func navOfficerRowStandsIn(e listEntry) bool {
	return e.officer() != "" && !e.chief() && !navOfficerInstanceDrawnHere(e)
}

// navOfficerAwake reports whether an instance's stored status is one of a
// running agent's (started, or starting).
func navOfficerAwake(status string) bool {
	switch status {
	case "running", "provisioning", "pending":
		return true
	}
	return false
}

// navOfficerOrder sorts officers for the group: the viewer's own first, then
// each team's by its name (then its id), each by name and then key — a stable
// order whatever order they arrive in.
func navOfficerOrder(officers []NavOfficer) []NavOfficer {
	out := append([]NavOfficer(nil), officers...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.TeamOwner == 0) != (b.TeamOwner == 0) {
			return a.TeamOwner == 0
		}
		if a.TeamOwner != b.TeamOwner {
			if ta, tb := strings.ToLower(a.Team), strings.ToLower(b.Team); ta != tb {
				return ta < tb
			}
			return a.TeamOwner < b.TeamOwner
		}
		if na, nb := strings.ToLower(navOfficerName(a)), strings.ToLower(navOfficerName(b)); na != nb {
			return na < nb
		}
		return a.Key < b.Key
	})
	return out
}

// navOfficerName is what an officer's row calls it: its name, else its id.
func navOfficerName(o NavOfficer) string {
	if n := strings.TrimSpace(o.Name); n != "" {
		return n
	}
	return o.ID
}

// navOfficersGroup is the Officers group: its heading and one row per officer
// (navOfficerRow), its instances (as the list met them) by officer. Nothing
// when there is no officer.
func navOfficersGroup(officers []NavOfficer, instances []navOfficerInstance) g.Node {
	if len(officers) == 0 {
		return nil
	}
	byKey := map[string][]navOfficerInstance{}
	for _, in := range instances {
		k := in.e.officer()
		byKey[k] = append(byKey[k], in)
	}
	var rows []g.Node
	for _, o := range navOfficerOrder(officers) {
		rows = append(rows, navOfficerRow(o, byKey[o.Key]))
	}
	return Div(
		ID(NavOfficersGroupID),
		g.Attr("data-nav-officers", ""),
		Class("mt-1 border-t border-line pt-1"),
		P(ID(navOfficersLabelID), Class("flex min-h-[44px] items-center px-3 text-xs font-medium uppercase tracking-wide text-muted"), g.Text("Officers")),
		Ul(g.Attr("aria-labelledby", navOfficersLabelID), Class("flex flex-col gap-0.5"), g.Group(rows)),
	)
}

// navOfficerRow is one officer's row: a real-load link, 44px tall, like a
// session row (navAgentRow).
//
//   - Its link: the live instance's chat (the most recently active awake
//     instance, its name breaking a tie) while one runs, else Href, the
//     officer's page.
//   - Its second line: the team's name (a team's officer), running or asleep
//     (not for a host officer), and the queued count when known.
//   - At its end: the spend against the budget, "held", and "↑ waiting" when
//     an instance waits (that instance is drawn above, navOfficerInstanceDrawnHere).
//   - It is the page's own (aria-current="true", as a session row) when the
//     page is an instance that is not drawn as a row of its own.
func navOfficerRow(o NavOfficer, instances []navOfficerInstance) g.Node {
	// The most recently active first (the live pick), the name breaking a tie.
	sort.SliceStable(instances, func(i, j int) bool {
		a, b := instances[i].e.agent, instances[j].e.agent
		if ta, tb := a.lastActive(), b.lastActive(); !ta.Equal(tb) {
			return ta.After(tb)
		}
		return a.Name < b.Name
	})
	var live *AgentCardView
	waiting, current := false, false
	for _, in := range instances {
		if live == nil && navOfficerAwake(in.e.agent.Status) {
			live = in.e.agent
		}
		if navOfficerInstanceDrawnHere(in.e) {
			waiting = true
		} else if in.it.Current {
			current = true
		}
	}
	href, state := o.Href, ""
	if !o.OnHost {
		state = "asleep"
		if live != nil {
			href, state = live.PageHref(), "running"
		}
	}
	name := navOfficerName(o)
	var facts []string
	if o.TeamOwner != 0 && strings.TrimSpace(o.Team) != "" {
		facts = append(facts, o.Team)
	}
	if state != "" {
		facts = append(facts, state)
	}
	if o.Queued != nil {
		facts = append(facts, strconv.Itoa(*o.Queued)+" queued")
	}
	tip := "Officer " + o.ID
	if o.TeamOwner != 0 && o.Team != "" {
		tip += " (" + o.Team + ")"
	}
	return Li(
		Class("min-w-0"),
		A(
			Href(href),
			hx("hx-boost", "false"),
			g.Attr("data-nav-officer", o.Key),
			g.If(state != "", g.Attr("data-nav-officer-state", state)),
			g.If(o.TeamOwner != 0, g.Attr("data-nav-officer-team", strconv.FormatInt(o.TeamOwner, 10))),
			g.If(current, g.Group([]g.Node{g.Attr("aria-current", "true"), g.Attr("data-nav-current", "")})),
			g.Attr("title", tip),
			// What the command palette finds it by on a page with no switcher
			// (palette.go's fallback reads a[data-nav-officer] too): its name,
			// and its id and team as words.
			g.Attr("data-switcher-name", name),
			g.Attr("data-switcher-words", strings.TrimSpace(o.ID+" "+o.Team)),
			Class("press flex min-h-[44px] w-full min-w-0 items-center gap-2 rounded-lg border-l-4 px-3 text-left text-sm font-medium transition border-l-transparent "+navRowTone(current)),
			Span(
				Class("flex min-w-0 flex-1 flex-col leading-tight"),
				Span(g.Attr("data-nav-name", ""), Class("truncate"), g.Text(name)),
				g.If(len(facts) > 0, Span(g.Attr("data-nav-officer-facts", ""), Class(navTmuxLineClass), g.Text(strings.Join(facts, " · ")))),
			),
			navOfficerSpend(o),
			g.If(o.Held && o.Spent != nil, Span(g.Attr("data-nav-officer-held", ""),
				g.Attr("title", "Its daily budget is spent: it is not woken again until the next UTC midnight"),
				Class("shrink-0 text-[11px] font-semibold text-st-error-fg"), g.Text("held"))),
			g.If(waiting, Span(g.Attr("data-nav-officer-waiting", ""),
				g.Attr("title", "One of its sessions waits on you: it is listed above"),
				Class("shrink-0 text-[11px] font-normal text-muted"), g.Text("↑ waiting"))),
		),
	)
}

// navOfficerSpend is an officer's spend against its daily budget ("$6.40 /
// $10"): nothing with no budget, and nothing when the spend is not known.
func navOfficerSpend(o NavOfficer) g.Node {
	if o.Budget <= 0 || o.Spent == nil {
		return nil
	}
	text := "$" + strconv.FormatFloat(*o.Spent, 'f', 2, 64) + " / " + navUSD(o.Budget)
	return Span(
		g.Attr("data-nav-officer-spend", text),
		g.Attr("title", "Spent today (UTC) against its daily budget"),
		Class("shrink-0 text-[11px] font-normal tabular-nums text-muted"),
		g.Text(text),
	)
}

// navUSD is a budget in dollars: whole dollars without cents ("$10"), else
// with two places ("$7.50").
func navUSD(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return "$" + strings.TrimSuffix(s, ".00")
}
