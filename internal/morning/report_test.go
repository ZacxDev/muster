package morning

import (
	"testing"
	"time"
)

var (
	nightSince = time.Date(2000, 1, 1, 14, 0, 0, 0, time.UTC)
	nightUntil = time.Date(2000, 1, 2, 14, 0, 0, 0, time.UTC)
)

// Quiet is "nothing open, nothing done, nothing newly stuck": each term alone
// makes a day loud, and a session stuck since before the night does not.
func TestQuietIsNothingOpenDoneOrNewlyStuck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		r     Report
		quiet bool
	}{
		{"nothing at all", Report{}, true},
		{"one open question", Report{Questions: []Question{{AskID: 3, Text: "Ship the kiosk?"}}}, false},
		{"one thing done", Report{Done: []Done{{Kind: DoneTask, Label: "Paint the kiosk"}}}, false},
		{"stuck since inside the night", Report{Stuck: []Stuck{{Kind: StuckIdle, Name: "kiosk", Since: nightSince.Add(time.Hour)}}}, false},
		{"stuck since exactly the night's start", Report{Stuck: []Stuck{{Kind: StuckError, Name: "kiosk", Since: nightSince}}}, false},
		{"stuck since before the night", Report{Stuck: []Stuck{{Kind: StuckIdle, Name: "kiosk", Since: nightSince.Add(-time.Minute)}}}, true},
		{"usage alone", Report{Usage: Usage{Chief: ChiefUsage{Tracked: true, Turns: 7, Busy: time.Hour}}}, true},
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
		q, d, s int
		want    string
	}{
		{1, 0, 0, "1 question for you"},
		{2, 3, 4, "2 questions for you · 3 done · 4 stuck"},
		{0, 5, 0, "5 done"},
		{0, 0, 6, "6 stuck"},
		{0, 0, 0, "Nothing new"},
	} {
		if got := PushBody(tc.q, tc.d, tc.s); got != tc.want {
			t.Errorf("PushBody(%d, %d, %d) = %q; want %q", tc.q, tc.d, tc.s, got, tc.want)
		}
	}
}
