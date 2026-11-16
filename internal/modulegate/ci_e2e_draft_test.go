package modulegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// ---------------------------------------------------------------------------
// A DRAFT PULL REQUEST RUNS NO E2E, AND ITS RUNS NEVER SATISFY THE REQUIRED
// `e2e (playwright, chromium)` CHECK.
//
// 🔴 THE THREE HALVES ARE PINNED TOGETHER, BECAUSE EACH IS HARMLESS ALONE AND
// WRONG WITHOUT THE OTHERS:
//   - the legs skip on a draft (the saving);
//   - `ready_for_review` starts a run, or a PR marked ready never gets e2e at
//     all and, once the check is required, can never merge;
//   - the aggregate `e2e` job carries the required name ONLY on a non-draft
//     run. Branch protection counts a SKIPPED check as passing, so an aggregate
//     that merely skipped on a draft (or ran under the required name and passed
//     because the legs were skipped) would let a PR merge the moment it was
//     marked ready, before any e2e had run.
//
// 🔴 EXACT STRINGS, in the style of ci_concurrency_test.go: a deliberate change
// to one of these lines is a deliberate change to this test. The draft
// condition is spelled twice in the aggregate (its name and E2E_DRAFT), so the
// RELATIONSHIP is pinned too: the name's draft branch and the variable that
// gates its steps must be the same expression, or a run could carry the
// required name down the draft path.
//
// 🔴 WHAT THIS CANNOT SEE: it reads the workflow, not how GitHub evaluates it.
// The draft run (legs skipped, `e2e (draft PR, not run)`) and the ready run
// (legs ran, `e2e (playwright, chromium)`) were observed live on the PR that
// added this test.
// ---------------------------------------------------------------------------

const (
	requiredE2ECheck = "e2e (playwright, chromium)"
	draftExpr        = "github.event_name == 'pull_request' && github.event.pull_request.draft"

	wantPRTypes    = "opened,synchronize,reopened,ready_for_review"
	wantLegIf      = "${{ !cancelled() && needs.main-verdict.outputs.retest != 'false' && !(" + draftExpr + ") }}"
	wantE2EName    = "${{ " + draftExpr + " && 'e2e (draft PR, not run)' || '" + requiredE2ECheck + "' }}"
	wantE2EDraft   = "${{ " + draftExpr + " }}"
	wantDraftIf    = "${{ env.E2E_DRAFT == 'true' }}"
	wantNotDraftIf = "${{ env.E2E_DRAFT != 'true' }}"
	wantDraftCheck = `test "${{ needs.e2e-leg.result }}" = skipped`
)

type ciStep struct {
	Name string `yaml:"name"`
	Uses string `yaml:"uses"`
	If   string `yaml:"if"`
	Run  string `yaml:"run"`
}

type ciJob struct {
	Name  string            `yaml:"name"`
	If    string            `yaml:"if"`
	Env   map[string]string `yaml:"env"`
	Steps []ciStep          `yaml:"steps"`
}

type ciWorkflow struct {
	// `on` is a YAML 1.1 boolean to some parsers; yaml.v3 keeps it a string.
	On struct {
		PullRequest struct {
			Types []string `yaml:"types"`
		} `yaml:"pull_request"`
	} `yaml:"on"`
	Jobs map[string]ciJob `yaml:"jobs"`
}

func readCIWorkflow(t *testing.T) (string, ciWorkflow) {
	t.Helper()
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	// Positive control: the parse must reach the jobs, or every "missing"
	// below says nothing about the workflow.
	if _, ok := wf.Jobs["test"]; !ok {
		t.Fatalf("positive control FAILED: no `test` job parsed from %s", path)
	}
	return path, wf
}

// TestCIRunsE2EOnlyOnceAPullRequestIsReady pins the draft skip, the ready
// trigger, and the required check's name.
func TestCIRunsE2EOnlyOnceAPullRequestIsReady(t *testing.T) {
	path, wf := readCIWorkflow(t)

	if got := strings.Join(wf.On.PullRequest.Types, ","); got != wantPRTypes {
		t.Errorf("%s: on.pull_request.types is [%s], want [%s]: without ready_for_review, marking a "+
			"draft ready starts no run, so the PR never gets e2e or the required %q check",
			path, got, wantPRTypes, requiredE2ECheck)
	}

	leg, ok := wf.Jobs["e2e-leg"]
	if !ok {
		t.Fatalf("%s: no `e2e-leg` job", path)
	}
	if leg.If != wantLegIf {
		t.Errorf("%s: e2e-leg's if is\n  %s\nwant\n  %s\n(the legs must skip on a draft pull request and nowhere else)",
			path, leg.If, wantLegIf)
	}

	agg, ok := wf.Jobs["e2e"]
	if !ok {
		t.Fatalf("%s: no `e2e` job", path)
	}
	if agg.Name != wantE2EName {
		t.Errorf("%s: the e2e job's name is\n  %s\nwant\n  %s\n(a draft run must never carry the required %q name)",
			path, agg.Name, wantE2EName, requiredE2ECheck)
	}
	if got := agg.Env["E2E_DRAFT"]; got != wantE2EDraft {
		t.Errorf("%s: the e2e job's E2E_DRAFT is %q, want %q: it must be the same draft condition as the "+
			"job's name, or a run could carry the required name down the draft path", path, got, wantE2EDraft)
	}

	// The draft path: exactly the first step, and it refuses unless the legs
	// skipped. Every other step is the real verdict and is off on a draft.
	if len(agg.Steps) < 2 {
		t.Fatalf("%s: the e2e job has %d steps, want the draft check plus the verdict steps", path, len(agg.Steps))
	}
	first := agg.Steps[0]
	if first.If != wantDraftIf || !strings.Contains(first.Run, wantDraftCheck) {
		t.Errorf("%s: the e2e job's first step must be the draft check (if %q, run containing %q); got if %q, run %q",
			path, wantDraftIf, wantDraftCheck, first.If, first.Run)
	}
	for i, s := range agg.Steps[1:] {
		if s.If != wantNotDraftIf {
			t.Errorf("%s: e2e job step %d (%s%s) has if %q, want %q: on a non-draft run every verdict step must run",
				path, i+2, s.Name, s.Uses, s.If, wantNotDraftIf)
		}
	}
	verdict := false
	for _, s := range agg.Steps[1:] {
		if strings.Contains(s.Run, `test "${{ needs.e2e-leg.result }}" = success`) {
			verdict = true
		}
	}
	if !verdict {
		t.Errorf("%s: no non-draft step of the e2e job requires e2e-leg to have succeeded", path)
	}

	// The required name is carried by the aggregate alone: a leg, or any other
	// job, spelling it would satisfy the required check without the verdict.
	for id, j := range wf.Jobs {
		if id != "e2e" && strings.Contains(j.Name, requiredE2ECheck) {
			t.Errorf("%s: job %q's name %q contains the required check name %q", path, id, j.Name, requiredE2ECheck)
		}
	}
}
