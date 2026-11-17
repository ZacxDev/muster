package fleet

import (
	"strconv"
	"testing"
	"time"
)

// The rules, as tables: every case is literal, and the fixture fleet below
// gives every group a count no other group has, so a session sent to the wrong
// group (a swapped case in GroupOf, a mis-keyed group in Build) changes two
// counts at once and fails.

var t0 = time.Date(2000, 1, 2, 3, 0, 0, 0, time.UTC)

func TestStateOfPrecedence(t *testing.T) {
	stalled := &Episode{ID: 1, State: "stalled", Since: t0}
	needs := &Episode{ID: 2, State: "needs_you", Since: t0}
	waitingEp := &Episode{ID: 3, State: "needs_you", Waiting: true, Since: t0}
	for _, tc := range []struct {
		name    string
		waiting bool
		ep      *Episode
		want    string
	}{
		{"no stop", false, nil, "working"},
		{"a wait on screen before its episode opens", true, nil, "waiting"},
		{"a wait on screen over a stalled episode", true, stalled, "waiting"},
		{"an episode opened waiting", false, waitingEp, "waiting"},
		{"needs you", false, needs, "needs_you"},
		{"stalled", false, stalled, "stalled"},
		{"an episode state this package does not know", false, &Episode{State: "other"}, "working"},
	} {
		if got := StateOf(tc.waiting, tc.ep); got != tc.want {
			t.Errorf("%s: StateOf = %q; want %q", tc.name, got, tc.want)
		}
	}
}

func TestGroupOfEveryCase(t *testing.T) {
	open := []PR{{Repo: "example-org/widget", Number: 4, State: "open"}}
	merged := []PR{{Repo: "example-org/widget", Number: 5, State: "merged"}, {Repo: "example-org/widget", Number: 3, State: "open"}}
	closed := []PR{{Repo: "example-org/widget", Number: 6, State: "closed"}}
	unknown := []PR{{Repo: "example-org/widget", Number: 7, State: "unknown"}}
	olderOpen := []PR{{Repo: "example-org/widget", Number: 8, State: "merged"}}
	for _, tc := range []struct {
		name     string
		state    string
		prs      []PR
		inFlight bool
		want     string
	}{
		{"waiting", "waiting", nil, false, "needs_you"},
		{"needs you, a merged pull request too", "needs_you", merged, false, "needs_you"},
		{"working, its pull request merged", "working", merged, false, "working"},
		{"stalled, no pull request", "stalled", nil, false, "stalled"},
		{"stalled, latest open", "stalled", open, false, "waiting_ci"},
		{"stalled, latest merged (an older one open)", "stalled", merged, false, "done"},
		{"stalled, latest closed unmerged", "stalled", closed, false, "done"},
		{"stalled, latest merged but a message queued", "stalled", olderOpen, true, "stalled"},
		{"stalled, its state unknown", "stalled", unknown, false, "stalled"},
		{"no state", "", merged, false, ""},
	} {
		if got := GroupOf(tc.state, tc.prs, tc.inFlight); got != tc.want {
			t.Errorf("%s: GroupOf = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// fixtureFleet: Needs you 6 (2 waiting + 1 needs_you + 3 chief asks),
// Waiting on CI 2, Working 7, Done 4, Stalled 5 — pairwise distinct, and
// none equal to 1 or 0.
func fixtureFleet() ([]Session, []Ask) {
	var out []Session
	n := 0
	add := func(k int, tmpl Session) {
		for i := 0; i < k; i++ {
			n++
			s := tmpl
			s.ID = "mirror:s" + strconv.Itoa(n)
			if tmpl.Episode != nil {
				e := *tmpl.Episode
				e.ID = int64(n)
				e.Since = e.Since.Add(time.Duration(n) * time.Minute)
				s.Episode = &e
			}
			out = append(out, s)
		}
	}
	add(2, Session{State: "waiting"})
	add(1, Session{State: "needs_you", Episode: &Episode{State: "needs_you", Since: t0}})
	add(2, Session{State: "stalled", Episode: &Episode{State: "stalled", Since: t0}, PRs: []PR{{State: "open"}}})
	add(7, Session{State: "working"})
	// Done: two whose pull request closed after the test's baseline, two before.
	add(2, Session{State: "stalled", Episode: &Episode{State: "stalled", Since: t0}, PRs: []PR{{State: "merged", ClosedAt: t0.Add(time.Hour)}}})
	add(2, Session{State: "stalled", Episode: &Episode{State: "stalled", Since: t0}, PRs: []PR{{State: "closed", ClosedAt: t0}}})
	add(5, Session{State: "stalled", Episode: &Episode{State: "stalled", Since: t0}})
	asks := []Ask{{ID: 1, CreatedAt: t0.Add(2 * time.Hour)}, {ID: 2, CreatedAt: t0}, {ID: 3, CreatedAt: t0.Add(3 * time.Hour)}}
	return out, asks
}

func TestBuildCountsEachGroupFromAFleetWithDistinctCounts(t *testing.T) {
	sessions, asks := fixtureFleet()
	d := Build(sessions, asks, time.Time{})
	want := map[string]int{"needs_you": 6, "waiting_ci": 2, "working": 7, "done": 4, "stalled": 5}
	var keys []string
	for _, g := range d.Groups {
		keys = append(keys, g.Key)
		if g.Count() != want[g.Key] {
			t.Errorf("group %s: count %d; want %d", g.Key, g.Count(), want[g.Key])
		}
		if g.Label != GroupLabel[g.Key] || g.Label == "" {
			t.Errorf("group %s: label %q", g.Key, g.Label)
		}
	}
	if got, wantKeys := keys, []string{"needs_you", "waiting_ci", "working", "done", "stalled"}; len(got) != len(wantKeys) {
		t.Fatalf("groups %v; want %v", got, wantKeys)
	} else {
		for i := range got {
			if got[i] != wantKeys[i] {
				t.Fatalf("groups %v; want %v", got, wantKeys)
			}
		}
	}
	if d.Total != 21 {
		t.Errorf("total %d; want 21 sessions", d.Total)
	}
	if d.Groups[0].Asks[0].ID != 2 || d.Groups[0].Asks[2].ID != 3 {
		t.Errorf("asks not oldest first: %+v", d.Groups[0].Asks)
	}
	if d.Since != (Since{}) {
		t.Errorf("no baseline: since = %+v; want nothing", d.Since)
	}
}

// Since you last looked: against a baseline between the fixture's times, the
// stops opened after it, the Done sessions whose pull request closed after
// it, and the asks raised after it — each a distinct count.
func TestBuildCountsWhatMovedSinceTheBaseline(t *testing.T) {
	sessions, asks := fixtureFleet()
	// Episodes open at t0 + n minutes, n = 1..21 by position: after t0+14m
	// are the stops of sessions 15..21 that have one (the Done 4 are 13..16:
	// 15, 16; the Stalled 5 are 17..21) — 7.
	base := t0.Add(14 * time.Minute)
	d := Build(sessions, asks, base)
	if want := (Since{Baseline: base, NewStops: 7, Finished: 2, NewQuestions: 2}); d.Since != want {
		t.Errorf("since = %+v; want %+v", d.Since, want)
	}
}
