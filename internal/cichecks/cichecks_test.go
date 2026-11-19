package cichecks

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

// TestTheSharedRuleOverEveryShape: green, one pending, one failed, neutral,
// none, and a newer failed attempt over an older success — each as a commit's
// whole CI (Summarize) and as one required check (Of). Pinned as literals.
func TestTheSharedRuleOverEveryShape(t *testing.T) {
	run := func(id int64, name, status, conclusion string) Run {
		r := Run{ID: id, Name: name, Status: status, Conclusion: conclusion}
		if status == "completed" {
			r.CompletedAt = t0.Add(time.Duration(id) * time.Minute)
		}
		return r
	}
	for _, c := range []struct {
		name                         string
		in                           Checks
		check                        string // the one Of reads
		of                           string
		done, green                  bool
		greens, pend, failed, neutrl int
		failedNames                  string
	}{
		{"green", Checks{Statuses: map[string]Status{"lint": {State: "success", UpdatedAt: t0}}, Runs: []Run{run(11, "unit", "completed", "success")}},
			"unit", Green, true, true, 2, 0, 0, 0, ""},
		{"one pending", Checks{Statuses: map[string]Status{"lint": {State: "success"}}, Runs: []Run{run(12, "unit", "in_progress", "")}},
			"unit", Pending, false, false, 1, 1, 0, 0, ""},
		{"a pending status", Checks{Statuses: map[string]Status{"deploy-preview": {State: "pending"}}},
			"deploy-preview", Pending, false, false, 0, 1, 0, 0, ""},
		{"one failed", Checks{Statuses: map[string]Status{"lint": {State: "failure", UpdatedAt: t0}}, Runs: []Run{run(13, "unit", "completed", "success")}},
			"lint", Failed, true, false, 1, 0, 1, 0, "lint"},
		{"neutral", Checks{Runs: []Run{run(14, "unit", "completed", "success"), run(15, "docs", "completed", "neutral"), run(16, "e2e", "completed", "skipped")}},
			"docs", Neutral, true, true, 1, 0, 0, 2, ""},
		{"none", Checks{}, "unit", Missing, false, false, 0, 0, 0, 0, ""},
		{"a newer failed attempt over an older success", Checks{Runs: []Run{run(17, "unit", "completed", "success"), run(29, "unit", "completed", "failure")}},
			"unit", Failed, true, false, 0, 0, 1, 0, "unit"},
		{"an older failed attempt under a newer success", Checks{Runs: []Run{run(29, "unit", "completed", "success"), run(17, "unit", "completed", "failure")}},
			"unit", Green, true, true, 1, 0, 0, 0, ""},
		{"a status wins over a run of the same name", Checks{Statuses: map[string]Status{"unit": {State: "error"}}, Runs: []Run{run(31, "unit", "completed", "success")}},
			"unit", Failed, true, false, 0, 0, 1, 0, "unit"},
	} {
		s := Summarize(c.in)
		if got := Of(c.in, c.check).State; got != c.of {
			t.Errorf("%s: Of(%s) = %s, want %s", c.name, c.check, got, c.of)
		}
		if s.Done() != c.done || s.AllGreen() != c.green || s.Green != c.greens || s.Pending != c.pend || s.Failed != c.failed || s.Neutral != c.neutrl {
			t.Errorf("%s: done %v green %v (%d green, %d pending, %d failed, %d neutral); want done %v green %v (%d, %d, %d, %d)",
				c.name, s.Done(), s.AllGreen(), s.Green, s.Pending, s.Failed, s.Neutral, c.done, c.green, c.greens, c.pend, c.failed, c.neutrl)
		}
		if got := strings.Join(s.FailedNames(), ","); got != c.failedNames {
			t.Errorf("%s: failed names %q, want %q", c.name, got, c.failedNames)
		}
	}
}

// TestEveryCheckSkippedIsNotGreen: finished, but nothing passed.
func TestEveryCheckSkippedIsNotGreen(t *testing.T) {
	s := Summarize(Checks{Runs: []Run{{ID: 41, Name: "e2e", Status: "completed", Conclusion: "skipped"}}})
	if !s.Done() || s.AllGreen() {
		t.Fatalf("all skipped: done %v green %v; want done, not green", s.Done(), s.AllGreen())
	}
}

// TestLastFinishedIsTheLatestCompletion: when the last check finished.
func TestLastFinishedIsTheLatestCompletion(t *testing.T) {
	s := Summarize(Checks{Statuses: map[string]Status{"lint": {State: "success", UpdatedAt: t0.Add(7 * time.Minute)}},
		Runs: []Run{{ID: 3, Name: "unit", Status: "completed", Conclusion: "failure", CompletedAt: t0.Add(19 * time.Minute)}}})
	if !s.LastFinished.Equal(t0.Add(19 * time.Minute)) {
		t.Fatalf("last finished %v", s.LastFinished)
	}
}
