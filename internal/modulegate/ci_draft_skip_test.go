package modulegate

import (
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A DRAFT PULL REQUEST SKIPS EXACTLY THE INTEGRATION JOBS, AND NOTHING THAT
// READS THOSE SKIPS RUNS ON A PULL REQUEST.
//
// 🔴 A LEDGER, NOT A LIST OF THINGS THAT MUST BE PRESENT: it fails when the set
// of jobs skipped on a draft GROWS (a job that gives a draft its fast feedback,
// such as the Go suite, quietly stops running there) and when it SHRINKS (the
// saving is lost without anyone deciding to lose it).
//
// 🔴 THE SEAM IS `publish-gate`. It needs several of these jobs, and
// tag_gate.py gate refuses a skip that no main run vouched for, so on a draft
// it would go red on every run. It therefore runs only on a push — pinned here
// as an exact string, together with the ledger, because each half is wrong
// without the other.
//
// 🔴 WHAT THIS CANNOT SEE: it reads the workflow, not how GitHub evaluates it.
// The draft run (these jobs skipped, the run green) and the ready run (all of
// them ran) were observed live on the PR that added this test.
// ---------------------------------------------------------------------------

// The job-level `if:` of a draft-skipped job contains this, and no other job's does.
const draftSkip = "!(" + draftExpr + ")"

var wantDraftSkipped = []string{
	"ccd-image",
	"chart-kind",
	"docshots",
	"e2e-leg",
	"image-pr",
	"release-dry-run",
}

const (
	wantSuiteDraftSkipIf = "${{ !cancelled() && needs.main-verdict.outputs.retest != 'false' && " + draftSkip + " }}"
	wantImagePRIf        = "${{ github.event_name == 'pull_request' && " + draftSkip + " }}"
	wantPublishGateIf    = "${{ !cancelled() && github.event_name == 'push' }}"
)

// TestCIDraftRunsSkipExactlyTheIntegrationJobs pins the draft-skip ledger and
// publish-gate's push-only condition.
func TestCIDraftRunsSkipExactlyTheIntegrationJobs(t *testing.T) {
	path, wf := readCIWorkflow(t)

	var got []string
	for id, j := range wf.Jobs {
		if strings.Contains(j.If, draftSkip) {
			got = append(got, id)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(wantDraftSkipped, ",") {
		t.Errorf("%s: the jobs skipped on a draft pull request are\n  [%s]\nwant\n  [%s]\n"+
			"(a job added here stops giving drafts feedback; one removed loses the saving)",
			path, strings.Join(got, ", "), strings.Join(wantDraftSkipped, ", "))
	}

	// Exact conditions: the draft skip is added to each job's usual condition,
	// never in place of it (a tag whose main run vouched for it still skips).
	for _, id := range wantDraftSkipped {
		j, ok := wf.Jobs[id]
		if !ok {
			t.Errorf("%s: no %q job", path, id)
			continue
		}
		want := wantSuiteDraftSkipIf
		if id == "image-pr" {
			want = wantImagePRIf
		}
		if j.If != want {
			t.Errorf("%s: %s's if is\n  %s\nwant\n  %s", path, id, j.If, want)
		}
		if strings.Contains(j.Name, requiredE2ECheck) {
			t.Errorf("%s: draft-skipped job %q carries the required check name %q: a skip counts as "+
				"passing, so a draft run would satisfy it", path, id, requiredE2ECheck)
		}
	}

	// The required check's aggregate must still RUN on a draft (under its
	// draft name), so it can never be in the ledger.
	if strings.Contains(wf.Jobs["e2e"].If, draftSkip) {
		t.Errorf("%s: the e2e aggregate is skipped on a draft; it must run there under its draft name", path)
	}

	pg, ok := wf.Jobs["publish-gate"]
	if !ok {
		t.Fatalf("%s: no `publish-gate` job", path)
	}
	if pg.If != wantPublishGateIf {
		t.Errorf("%s: publish-gate's if is\n  %s\nwant\n  %s\n(on a pull request it would read a "+
			"draft's skipped jobs as refusals and go red)", path, pg.If, wantPublishGateIf)
	}
}
