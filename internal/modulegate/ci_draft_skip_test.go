package modulegate

import (
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A DRAFT PULL REQUEST SKIPS EXACTLY THE INTEGRATION JOBS (plus publish-gate,
// which reads their skips).
//
// 🔴 A LEDGER, NOT A LIST OF THINGS THAT MUST BE PRESENT: it fails when the set
// of jobs skipped on a draft GROWS (a job that gives a draft its fast feedback,
// such as the Go suite, quietly stops running there) and when it SHRINKS (the
// saving is lost without anyone deciding to lose it). The set is every job
// whose job-level `if:` mentions `pull_request.draft` in ANY spelling, so a
// differently-worded draft skip is still counted; each member's condition is
// then pinned as an exact string.
//
// 🔴 THE SEAM IS `publish-gate`. It needs several of these jobs, and
// tag_gate.py gate refuses a skip that no main run vouched for, so on a draft
// it would go red on every run. It is therefore skipped on a draft too — and
// still runs on a ready PR, where it is what refuses a test job that skipped
// by mistake.
//
// 🔴 WHAT THIS CANNOT SEE: it reads the workflow, not how GitHub evaluates it.
// The live draft and ready runs are recorded on PR #778, not here.
// ---------------------------------------------------------------------------

// The draft condition as each draft-skipped job spells it.
const draftSkip = "!(" + draftExpr + ")"

var wantDraftSkipped = []string{
	"ccd-image",
	"chart-kind",
	"docshots",
	"e2e-leg",
	"image-pr",
	"publish-gate",
	"release-dry-run",
}

var wantDraftSkipIf = map[string]string{
	"ccd-image":       "${{ !cancelled() && needs.main-verdict.outputs.retest != 'false' && " + draftSkip + " }}",
	"chart-kind":      "${{ !cancelled() && needs.main-verdict.outputs.retest != 'false' && " + draftSkip + " }}",
	"docshots":        "${{ !cancelled() && needs.main-verdict.outputs.retest != 'false' && " + draftSkip + " }}",
	"e2e-leg":         "${{ !cancelled() && needs.main-verdict.outputs.retest != 'false' && " + draftSkip + " }}",
	"release-dry-run": "${{ !cancelled() && needs.main-verdict.outputs.retest != 'false' && " + draftSkip + " }}",
	"image-pr":        "${{ github.event_name == 'pull_request' && " + draftSkip + " }}",
	"publish-gate":    "${{ !cancelled() && " + draftSkip + " }}",
}

// TestCIDraftRunsSkipExactlyTheIntegrationJobs pins the draft-skip ledger,
// each member's exact condition, and publish-gate's place in it.
func TestCIDraftRunsSkipExactlyTheIntegrationJobs(t *testing.T) {
	path, wf := readCIWorkflow(t)

	var got []string
	for id, j := range wf.Jobs {
		if strings.Contains(j.If, "pull_request.draft") {
			got = append(got, id)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(wantDraftSkipped, ",") {
		t.Errorf("%s: the jobs whose if mentions pull_request.draft are\n  [%s]\nwant\n  [%s]\n"+
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
		if want := wantDraftSkipIf[id]; j.If != want {
			t.Errorf("%s: %s's if is\n  %s\nwant\n  %s", path, id, j.If, want)
		}
	}
}
