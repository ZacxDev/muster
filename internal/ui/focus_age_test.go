package ui

import (
	"strings"
	"testing"
	"time"
)

// TestAFocusTitlesAgeIsShownPastADayAndStalePastFour pins THE focus age rule
// (FocusAgeOf) at both boundaries and on each side of them. The ages
// OVERSHOOT each boundary by an amount no step can round onto it (30h, 50h,
// 4d+1h, 6d+13h), and the at-boundary rows pin which side the boundary itself
// is on: exactly a day shows nothing, exactly four days is not yet stale.
func TestAFocusTitlesAgeIsShownPastADayAndStalePastFour(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		age  time.Duration
		want FocusAge
	}{
		{"10 minutes", 10 * time.Minute, FocusAge{}},
		{"23 hours", 23 * time.Hour, FocusAge{}},
		{"exactly one day", 24 * time.Hour, FocusAge{}},
		{"30 hours", 30 * time.Hour, FocusAge{Text: "1d"}},
		{"50 hours", 50 * time.Hour, FocusAge{Text: "2d"}},
		{"exactly four days", 96 * time.Hour, FocusAge{Text: "4d"}},
		{"four days and an hour", 97 * time.Hour, FocusAge{Text: "4d", Stale: true}},
		{"six and a half days", 6*24*time.Hour + 13*time.Hour, FocusAge{Text: "6d", Stale: true}},
		{"forty days", 40 * 24 * time.Hour, FocusAge{Text: "40d", Stale: true}},
	} {
		if got := FocusAgeOf(now.Add(-c.age), now); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	// No time known (no focus): no age, however late now is.
	if got := FocusAgeOf(time.Time{}, now); got != (FocusAge{}) {
		t.Errorf("a zero time: %+v, want none", got)
	}
}

// TestThePillsAndRowsCarryTheFocusAge: a switcher pill and a sidebar row (both
// what the dock and the palette draw from) carry their focus title's age as
// the server decided it — data-switcher-focus-age past a day, and
// data-switcher-focus-stale past four — and a fresh focus carries neither. The
// three fixtures' ages are 2, 5 and 0 days, distinct from each other and from
// both thresholds, so an attribute copied from the wrong entry reads wrong.
func TestThePillsAndRowsCarryTheFocusAge(t *testing.T) {
	now := time.Now()
	list := []AgentCardView{{ID: 1, Name: "ag-wren", Status: "running", Focus: &FocusView{Title: "Indexing the wren archive", UpdatedAt: now.Add(-50 * time.Hour)}}}
	mirrors := []MirrorCardView{
		{Host: "box-old", Session: "w", Window: "@1", Label: "fig:1", Online: true, Focus: &FocusView{Title: "Pruning the fig orchard", UpdatedAt: now.Add(-5*24*time.Hour - 3*time.Hour)}},
		{Host: "box-new", Session: "w", Window: "@2", Label: "kiwi:2", Online: true, Focus: &FocusView{Title: "Peeling the kiwi crate", UpdatedAt: now.Add(-2 * time.Hour)}},
	}
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{}), true))
	all := page[strings.Index(page, `data-switcher-list="all"`):strings.Index(page, `id="switcher-views"`)]
	nav := renderString(t, NavAgentsList(list, mirrors, "", now))
	for name, html := range map[string]string{"the switcher's All row": all, "the sidebar": nav} {
		for _, c := range []struct{ code, want string }{
			{"ag-wren", `data-switcher-focus-age="2d"`},
			{"fig:1", `data-switcher-focus-age="5d" data-switcher-focus-stale`},
		} {
			if el := entryOf(html, c.code); !strings.Contains(el, c.want) {
				t.Errorf("%s: %s lacks %s:\n%s", name, c.code, c.want, el)
			}
		}
		if el := entryOf(html, "ag-wren"); strings.Contains(el, "data-switcher-focus-stale") {
			t.Errorf("%s: a two-day-old focus is marked stale:\n%s", name, el)
		}
		// CONTROL: a fresh focus carries no age and no stale mark.
		if el := entryOf(html, "kiwi:2"); el == "" || strings.Contains(el, "data-switcher-focus-age") || strings.Contains(el, "data-switcher-focus-stale") {
			t.Errorf("%s: a two-hour-old focus carries an age:\n%s", name, el)
		}
	}
}

// entryOf is the opening tag of the entry in html whose data-switcher-code is
// code ("" for none).
func entryOf(html, code string) string {
	i := strings.Index(html, `data-switcher-code="`+code+`"`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(html[:i], "<")
	end := strings.Index(html[i:], ">")
	return html[start : i+end+1]
}

// TestTheSidebarAgesItsFocusByThePagesClock: NavAgentsList's injected now is
// the clock its focus ages are read by — the same one its rows' own times use
// — not the wall clock. Fixture: a page clock ten days ahead of the wall, and a
// focus written 50 hours before it (so, by the wall clock, in the future: no
// age). Red at 80a44d4a: switcherItems read time.Now().
func TestTheSidebarAgesItsFocusByThePagesClock(t *testing.T) {
	now := time.Now().Add(10 * 24 * time.Hour)
	list := []AgentCardView{{ID: 1, Name: "ag-wren", Status: "running", Focus: &FocusView{Title: "Indexing the wren archive", UpdatedAt: now.Add(-50 * time.Hour)}}}
	nav := renderString(t, NavAgentsList(list, nil, "", now))
	if el := entryOf(nav, "ag-wren"); !strings.Contains(el, `data-switcher-focus-age="2d"`) {
		t.Errorf("the sidebar row is not aged by the page's clock:\n%s", el)
	}
}

// TestTheFocusAgeIsLabelled: the age's words, for its title and aria-label,
// say what it measures — a waiting time is drawn beside it in the same shape.
func TestTheFocusAgeIsLabelled(t *testing.T) {
	for _, c := range []struct {
		age  FocusAge
		want string
	}{
		{FocusAge{}, ""},
		{FocusAge{Text: "2d"}, "focus title set 2d ago"},
		{FocusAge{Text: "5d", Stale: true}, "focus title set 5d ago (stale)"},
	} {
		if got := FocusAgeLabel(c.age); got != c.want {
			t.Errorf("FocusAgeLabel(%+v) = %q, want %q", c.age, got, c.want)
		}
	}
}
