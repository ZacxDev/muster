package modulegate

import (
	"strings"
	"testing"
)

// requiredTestCheck is the Go suite's required status check on main: the
// `test` job's name as shard 0 renders it. Branch protection names it
// literally, so it must not change when the shard count does.
const requiredTestCheck = "test (postgres, shard 0)"

// TestCITestJobNameCarriesNoShardCount pins the `test` job's name to one that
// renders the same whatever the matrix size. It used to read
// `shard ${{ strategy.job-index }} of ${{ strategy.job-total }}`, so moving from
// 4 shards to 3 renamed shard 0 from "…shard 0 of 4" to "…shard 0 of 3": the
// required check main's protection names stopped being reported, and every
// pull request read BLOCKED (muster#803, 2026-11-18). Branch protection has to
// be edited by hand whenever that name changes, so the name must not depend on
// strategy.job-total, nor spell a literal count.
func TestCITestJobNameCarriesNoShardCount(t *testing.T) {
	path, wf := readCIWorkflow(t)
	job := wf.Jobs["test"]
	const want = "test (postgres, shard ${{ strategy.job-index }})"
	if job.Name != want {
		t.Errorf("%s: the test job's name is\n  %s\nwant\n  %s\n(shard 0 must render as the required check %q, whatever the shard count)",
			path, job.Name, want, requiredTestCheck)
	}
	if strings.Contains(job.Name, "job-total") || strings.Contains(job.Name, " of ") {
		t.Errorf("%s: the test job's name %q carries a shard count: a reshard renames the required check %q",
			path, job.Name, requiredTestCheck)
	}
	if got := strings.ReplaceAll(job.Name, "${{ strategy.job-index }}", "0"); got != requiredTestCheck {
		t.Errorf("%s: shard 0 renders as %q, want the required check %q", path, got, requiredTestCheck)
	}
}
