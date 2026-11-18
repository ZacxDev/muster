package morning

import (
	"testing"
	"time"
)

var (
	nightSince = time.Date(2000, 1, 1, 14, 0, 0, 0, time.UTC)
	nightUntil = time.Date(2000, 1, 2, 14, 0, 0, 0, time.UTC)
)

// Quiet is "no question asked overnight, no pull request merged, nothing newly
// stuck, every pull request checked": each term alone makes a day loud, and
// each thing that must NOT — an old question, an old stall, a task that only
// changed, usage — leaves it quiet.
func TestQuietIsNothingNewOvernight(t *testing.T) {
	merged := Done{Kind: DoneMerged, Label: "Paint the kiosk", Ref: "acme/kiosk#3"}
	for _, tc := range []struct {
		name  string
		r     Report
		quiet bool
	}{
		{"nothing at all", Report{}, true},
		{"a question asked overnight", Report{Questions: 1, NewQuestions: 1}, false},
		{"only questions asked before the night", Report{Questions: 3}, true},
		{"a pull request merged", Report{Body: Body{Done: []Done{merged}}}, false},
		{"a pull request not checked", Report{Unchecked: true}, false},
		{"a complete task that only changed overnight", Report{Body: Body{Done: []Done{{Kind: DoneTaskChanged, Label: "Tag the kiosk"}}}}, true},
		{"stuck since inside the night", Report{Body: Body{Stuck: []Stuck{{Kind: StuckIdle, Name: "kiosk", Since: nightSince.Add(time.Hour)}}}}, false},
		{"stuck since exactly the night's start", Report{Body: Body{Stuck: []Stuck{{Kind: StuckError, Name: "kiosk", Since: nightSince}}}}, false},
		{"stuck since before the night", Report{Body: Body{Stuck: []Stuck{{Kind: StuckIdle, Name: "kiosk", Since: nightSince.Add(-time.Minute)}}}}, true},
		{"stuck since after the night ended (tomorrow's)", Report{Body: Body{Stuck: []Stuck{{Kind: StuckIdle, Name: "kiosk", Since: nightUntil.Add(time.Minute)}}}}, true},
		{"stuck since exactly the night's end (tomorrow's)", Report{Body: Body{Stuck: []Stuck{{Kind: StuckIdle, Name: "kiosk", Since: nightUntil}}}}, true},
		{"usage alone", Report{Body: Body{Usage: Usage{Chief: ChiefUsage{Tracked: true, Turns: 7, Busy: time.Hour}}}}, true},
	} {
		tc.r.Since, tc.r.Until = nightSince, nightUntil
		if got := tc.r.Quiet(); got != tc.quiet {
			t.Errorf("%s: Quiet() = %v; want %v", tc.name, got, tc.quiet)
		}
	}
}

// The push body: counts only, zero terms left out, singular and plural.
func TestThePushBodyIsCountsOnly(t *testing.T) {
	for _, tc := range []struct {
		q, m, s int
		want    string
	}{
		{1, 0, 0, "1 question for you"},
		{2, 3, 4, "2 questions for you · 3 merged · 4 stuck"},
		{0, 5, 0, "5 merged"},
		{0, 0, 6, "6 stuck"},
		{0, 0, 0, "See what happened overnight"},
	} {
		if got := PushBody(tc.q, tc.m, tc.s); got != tc.want {
			t.Errorf("PushBody(%d, %d, %d) = %q; want %q", tc.q, tc.m, tc.s, got, tc.want)
		}
	}
}
