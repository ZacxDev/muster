package ccusage

import (
	"context"
	"net/http"
	"testing"
)

// ---------------------------------------------------------------------------
// THE PROBE CLEARS ONLY THE LIMIT THE MARK NAMES. The probe runs on a small
// model and its overall status speaks for that model; a mark set by the
// session (5h) or weekly (7d) limit is cleared only when THAT window, in the
// probe's headers, is present and under its limit. Any other mark text — a
// per-model limit, a message that names no window — is never cleared by the
// probe, so an account is not put back only to re-fail every interval.
// ---------------------------------------------------------------------------

func TestTheProbeClearsOnlyTheLimitTheMarkNames(t *testing.T) {
	probe := func(ws map[string]Window) *scripted {
		return &scripted{probe: []Result{{Status: http.StatusOK, HasReading: true,
			Reading: Reading{Status: "allowed", Windows: ws}}}}
	}
	for _, tc := range []struct {
		name    string
		detail  string
		fx      *scripted
		cleared bool
	}{
		{"weekly mark, 7d under its limit: cleared", "You've hit your weekly limit",
			probe(map[string]Window{SevenDay: {Utilization: f(40), Status: "allowed"}}), true},
		{"weekly mark, 7d at its limit: kept", "You've hit your weekly limit",
			probe(map[string]Window{SevenDay: {Utilization: f(100)}, FiveHour: {Utilization: f(10), Status: "allowed"}}), false},
		{"per-model mark, overall allowed: kept", "You've hit your Opus limit",
			probe(map[string]Window{SevenDay: {Utilization: f(40), Status: "allowed"}, FiveHour: {Utilization: f(10), Status: "allowed"}}), false},
		{"session mark, 5h under its limit: cleared", "You've hit your session limit",
			probe(map[string]Window{FiveHour: {Utilization: f(30), Status: "allowed"}}), true},
		{"session mark, headers carry no 5h window: kept", "You've hit your session limit",
			probe(map[string]Window{SevenDay: {Utilization: f(40), Status: "allowed"}}), false},
		{"a mark naming no window, overall allowed: kept", "Claude usage limit reached.",
			probe(map[string]Window{SevenDay: {Utilization: f(40), Status: "allowed"}, FiveHour: {Utilization: f(10), Status: "allowed"}}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newLiveRig(t, tc.fx)
			m := rig.marks.m["alpha"]
			m.RateLimitedDetail = tc.detail
			rig.marks.m["alpha"] = m
			if err := rig.poller.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := rig.alpha()
			if tc.cleared {
				if !got.RateLimitedAt.IsZero() || got.RateLimitedDetail != "" || len(rig.clearLines()) != 1 {
					t.Fatalf("alpha's mark = %+v, clear lines %q; want it cleared once", got, rig.clearLines())
				}
				return
			}
			if !got.RateLimitedAt.Equal(rig.markedAt) || got.RateLimitedDetail != tc.detail || len(rig.clearLines()) != 0 {
				t.Fatalf("alpha's mark = %+v, clear lines %q; want it kept (marked %s, %q)", got, rig.clearLines(), rig.markedAt, tc.detail)
			}
		})
	}
}

// TestMarkedLimitWindowReadsTheCLIsPhrasings: the phrasings the CLI writes
// (internal/cctranscript's pinned samples) map to their one window; anything
// else maps to none.
func TestMarkedLimitWindowReadsTheCLIsPhrasings(t *testing.T) {
	for _, c := range []struct{ detail, name, abbr string }{
		{"You've hit your session limit · resets 10:30pm (America/New_York)", FiveHour, "5h"},
		{"You’ve hit your session limit", FiveHour, "5h"},
		{"5-hour limit reached ∙ resets 3pm", FiveHour, "5h"},
		{"You've hit your weekly limit · resets Oct 15, 10pm (Asia/Tokyo)", SevenDay, "7d"},
		{"You've hit your Opus limit · resets Oct 15, 10pm (UTC)", "", ""},
		{"You've hit your weekly Opus limit", "", ""},
		{"You've hit your limit · resets 5pm", "", ""},
		{"Claude usage limit reached. Your limit will reset at 3pm (America/Los_Angeles).", "", ""},
		{"You've hit your session limit; You've hit your weekly limit", "", ""},
		{"", "", ""},
	} {
		if n, a := MarkedLimitWindow(c.detail); n != c.name || a != c.abbr {
			t.Errorf("MarkedLimitWindow(%q) = %q, %q; want %q, %q", c.detail, n, a, c.name, c.abbr)
		}
	}
}
