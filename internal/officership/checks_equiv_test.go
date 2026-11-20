package officership

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// checksGreenBefore is the shipping gate's CI rule as it was before it read
// through internal/cichecks (main's gates.go, verbatim but for its name and
// receiver): the reference the refactor must equal.
func checksGreenBefore(ctx context.Context, gh GitHub, ship *officers.Ship, sha string) error {
	st, err := gh.Statuses(ctx, sha)
	if err != nil {
		return refuse(GateChecks, "could not read the commit statuses of %s: %v", short(sha), err)
	}
	var bad []string
	for _, name := range ship.RequiredChecks {
		if state, ok := st[name]; ok {
			if state != "success" {
				bad = append(bad, name+"="+state)
			}
			continue
		}
		runs, err := gh.CheckRuns(ctx, sha, name)
		if err != nil {
			return refuse(GateChecks, "could not read check run %q on %s: %v", name, short(sha), err)
		}
		if r, ok := newest(runs, name); !ok {
			bad = append(bad, name+"=missing")
		} else if r.Status != "completed" || r.Conclusion != "success" {
			bad = append(bad, name+"="+r.Status+"/"+r.Conclusion)
		}
	}
	if len(bad) > 0 {
		return refuse(GateChecks, "on %s: %s", short(sha), strings.Join(bad, ", "))
	}
	return nil
}

// TestChecksGreenEqualsItsOldRule: the shipping gate's CI verdict, now read
// through cichecks.Of, is BEHAVIOUR-EQUIVALENT to the rule it replaced — the
// same refusal (or none), byte for byte — over every pair of shapes for two
// required checks: a commit status of each state, a status AND runs (the
// status wins), no run, one run of each status and conclusion, a newer run
// over an older one each way, and only another check's run.
func TestChecksGreenEqualsItsOldRule(t *testing.T) {
	type shape struct {
		name   string
		status string // "" = no commit status
		runs   []CheckRun
	}
	run := func(id int64, status, conclusion string) CheckRun {
		return CheckRun{ID: id, Status: status, Conclusion: conclusion, AppSlug: "ci-app"}
	}
	shapes := []shape{
		{name: "status success", status: "success"},
		{name: "status pending", status: "pending"},
		{name: "status failure", status: "failure"},
		{name: "status error", status: "error"},
		{name: "status failure over a green run", status: "failure", runs: []CheckRun{run(71, "completed", "success")}},
		{name: "status success over a red run", status: "success", runs: []CheckRun{run(72, "completed", "failure")}},
		{name: "no run"},
		{name: "run green", runs: []CheckRun{run(73, "completed", "success")}},
		{name: "run queued", runs: []CheckRun{run(74, "queued", "")}},
		{name: "run in progress", runs: []CheckRun{run(75, "in_progress", "")}},
		{name: "run failed", runs: []CheckRun{run(76, "completed", "failure")}},
		{name: "run timed out", runs: []CheckRun{run(77, "completed", "timed_out")}},
		{name: "run cancelled", runs: []CheckRun{run(78, "completed", "cancelled")}},
		{name: "run neutral", runs: []CheckRun{run(79, "completed", "neutral")}},
		{name: "run skipped", runs: []CheckRun{run(80, "completed", "skipped")}},
		{name: "newer red over older green", runs: []CheckRun{run(81, "completed", "success"), run(93, "completed", "failure")}},
		{name: "newer green over older red", runs: []CheckRun{run(94, "completed", "success"), run(82, "completed", "failure")}},
		{name: "newer pending over older green", runs: []CheckRun{run(83, "completed", "success"), run(95, "in_progress", "")}},
	}
	required := []string{"unit-tests", "lint-gate"}
	const sha = "c0ffee1234567890c0ffee1234567890c0ffee12"
	ctx := context.Background()
	ship := fxShip()
	ship.RequiredChecks = required
	compared, refusals := 0, 0
	for _, a := range shapes {
		for _, b := range shapes {
			gh := newFakeGH()
			gh.statuses = map[string]map[string]string{sha: {}}
			gh.checkRuns = map[string][]CheckRun{}
			for i, sh := range []shape{a, b} {
				name := required[i]
				if sh.status != "" {
					gh.statuses[sha][name] = sh.status
				}
				for _, r := range sh.runs {
					r.Name = name
					gh.checkRuns[sha] = append(gh.checkRuns[sha], r)
				}
			}
			// Another check's run is never read for these.
			gh.checkRuns[sha] = append(gh.checkRuns[sha], CheckRun{ID: 99, Name: "docs-preview", Status: "completed", Conclusion: "success"})
			g := &Gates{Ship: ship, GH: gh}
			got, want := g.checksGreen(ctx, sha), checksGreenBefore(ctx, gh, ship, sha)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s / %s: now %v; before %v", a.name, b.name, got, want)
			}
			compared++
			if want != nil {
				refusals++
			}
		}
	}
	// The instrument: every pair compared, and both answers occur (a table
	// that only ever refused, or never did, would compare nothing useful).
	if compared != len(shapes)*len(shapes) || refusals == 0 || refusals == compared {
		t.Fatalf("instrument: %d pairs, %d refused", compared, refusals)
	}
}
