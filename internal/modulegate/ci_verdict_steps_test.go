package modulegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE GO SUITES IN CI ARE PIPED INTO tests/verdict.py UNDER `pipefail`.
//
// 🔴 WHAT `set -o pipefail` CARRIES, AND WHY IT IS PINNED: verdict.py refuses a
// skip, zero verdicts, and any `fail`/`build-fail` event, but it cannot see
// `go test`'s exit status. A run that exits non-zero over a stream with no
// failure event in it is refused by pipefail ALONE — and Actions runs a `run:`
// with no `shell:` under `bash -e`, which has none. Delete that one line and
// the step still looks right and still goes red on every ordinary failure.
//
// 🔴 EXACT LINES, NOT SUBSTRINGS, in the style of ci_concurrency_test.go: the
// `set -o pipefail` must be the line directly above the pipe, in the same
// `run: |` block, and the pipe must end in verdict.py. A deliberate change to
// either step is a deliberate change to this test.
//
// 🔴 WHAT THIS CANNOT SEE: it reads the YAML text. It does not prove how the
// runner's shell behaves, and a `shell:` key added to the step is not
// inspected. The behaviour was measured by running the extracted step text
// under `bash -e` with a `go` that exits 7 on a clean stream: exit 7 with the
// line, exit 0 without it.
// ---------------------------------------------------------------------------

var wantVerdictPipes = map[string]string{
	// The shard's packages / `-run` and its `--require` names come from the
	// plan step (tests/goshard.py); the required names are pinned there, in
	// the same job, by TestCIRequiresTheIsolationWalkInTheShardPlan below.
	// Multi-process internal/api: the shard's processes write one -json file
	// each, ONE verdict reads them all (each process's exit status is checked
	// by the step separately, wantTestExitChecks), and the `tee` keeps that
	// SAME union for the `test-times-` artifact; TestCIUploadsTheVerdictStreamAsTestTimes
	// pins the upload.
	"test":     `cat "$RUNNER_TEMP"/go-test.*.json | tee "$RUNNER_TEMP/go-test.json" | python3 -u tests/verdict.py --echo summary $(cat "$RUNNER_TEMP"/verdict-args.*)`,
	"ccd-tmux": "go test -tags tmuxit -race -run '^TestTmux' -json ./cmd/ccd/ | python3 -u tests/verdict.py --echo all",
	"mirror-tmux": "go test -tags tmuxit -race -run '^TestTmux' -json ./internal/mirror/ ./internal/mirrorhost/ ./cmd/muster-mirror/ | python3 -u tests/verdict.py --echo all" +
		" --require TestTmuxADaemonStartedBeforeItsServerStillGetsItsQuestions --require TestTmuxASymlinkedTMUX_TMPDIRMatches" +
		" --require TestTmuxATenantSpawnLandsInItsServerAndReachesItsOwnMuster --require TestTmuxATenantSpawnNeverStartsItsServer" +
		" --require TestTmuxAMisSetTenantUnitNeitherSpawnsNorSweepsTheDefaultsWorktrees --require TestTmuxATenantOpenCodeSpawnBindsInItsServer",
	"mirror-macos": "go test -race -json ./internal/mirrorhost/... ./cmd/muster-mirror/... | python3 -u tests/verdict.py --echo summary --require TestDarwinSourceReadsThisHostsOwnProcesses",
}

// 🔴 THE `test` JOB IS THE EXCEPTION: its left side is `cat` over the
// processes' -json files, so pipefail there carries only a missing file. Its
// `go test` exit statuses are carried by the two lines after the pipe, which
// check one recorded status per process and fail on any non-zero one; they are
// pinned exactly below (wantTestExitChecks), because deleting them leaves a
// process that died over a clean stream green.
var wantTestExitChecks = []string{
	`[ "$(wc -l < "$RUNNER_TEMP/go-test.rcs")" = "$MUSTER_TEST_PROCS" ] || { echo "missing exit statuses" >&2; exit 1; }`,
	`while read -r rc; do [ "$rc" = 0 ] || exit "$rc"; done < "$RUNNER_TEMP/go-test.rcs"`,
}

// TestCIGoSuitesPipeIntoTheVerdictUnderPipefail pins every verdict step.
func TestCIGoSuitesPipeIntoTheVerdictUnderPipefail(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for job, want := range wantVerdictPipes {
		lines := jobLines(string(b), job)
		// Positive control: the extractor must find the job, or "no pipe found"
		// below would say nothing about the step.
		if len(lines) == 0 {
			t.Errorf("positive control FAILED: no `%s:` job in %s", job, path)
			continue
		}
		var at []int
		for i, l := range lines {
			if strings.Contains(l, "tests/verdict.py --echo") && !strings.HasPrefix(l, "#") {
				at = append(at, i)
			}
		}
		if len(at) != 1 {
			t.Errorf("job %q: %d line(s) run tests/verdict.py --echo, want exactly 1", job, len(at))
			continue
		}
		i := at[0]
		if lines[i] != want {
			t.Errorf("job %q: the verdict line is\n  %s\nwant\n  %s", job, lines[i], want)
		}
		if job == "test" {
			for k, w := range wantTestExitChecks {
				if i+1+k >= len(lines) || lines[i+1+k] != w {
					t.Errorf("job \"test\": line %d after the verdict pipe is not\n  %s\n(without it a go test process that exited non-zero over a clean stream is green)", k+1, w)
				}
			}
		}
		if i < 2 || lines[i-1] != "set -o pipefail" || lines[i-2] != "run: |" {
			t.Errorf("job %q: the verdict pipe must be directly under `run: |` and "+
				"`set -o pipefail`; without pipefail a `go test` that exits non-zero over "+
				"a clean stream is reported green.", job)
		}
	}
}

// TestCIUploadsTheVerdictStreamAsTestTimes pins the per-test timing artifact:
// the `test` job uploads the file its verdict pipe tees (so the timings are of
// the run that set the job's status, not a re-run), under a `test-times-`
// name per shard, even when the suite went red (a red run's timings are the
// ones a flake count needs). An external weekly timing report reads
// `test-times-*`; nothing in this repo downloads it.
func TestCIUploadsTheVerdictStreamAsTestTimes(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := jobLines(string(b), "test")
	if len(lines) == 0 {
		t.Fatalf("positive control FAILED: no `test:` job in %s", path)
	}
	name := -1
	for i, l := range lines {
		if l == "name: test-times-${{ strategy.job-index }}-a${{ github.run_attempt }}" {
			name = i
		}
	}
	if name < 2 {
		t.Fatalf("the `test` job uploads no `test-times-${{ strategy.job-index }}-a${{ github.run_attempt }}` artifact")
	}
	// The step: `- name:`/`if:`/`uses:`/`with:` above the artifact name, its
	// `path:` and the rest below, up to the next step.
	var step []string
	start := name
	for start > 0 && !strings.HasPrefix(lines[start], "- ") {
		start--
	}
	for _, l := range lines[start:] {
		if len(step) > 0 && strings.HasPrefix(l, "- ") {
			break
		}
		step = append(step, l)
	}
	for _, want := range []string{
		"if: ${{ !cancelled() }}",
		"uses: actions/upload-artifact@v4",
		`path: ${{ runner.temp }}/go-test.json`,
	} {
		found := false
		for _, l := range step {
			if l == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the test-times upload step lacks %q; step is:\n  %s", want, strings.Join(step, "\n  "))
		}
	}
	// The uploaded file is the one the verdict pipe tees.
	tee := 0
	for _, l := range lines {
		if strings.Contains(l, `| tee "$RUNNER_TEMP/go-test.json" | python3 -u tests/verdict.py`) && !strings.HasPrefix(l, "#") {
			tee++
		}
	}
	if tee != 1 {
		t.Errorf("%d verdict pipe(s) tee $RUNNER_TEMP/go-test.json, want exactly 1", tee)
	}
}

// TestCIRequiresTheIsolationWalkInTheShardPlan pins the accounts isolation
// walk's three tests as `--require` lines of the `test` job's goshard.py plan
// step — the one place those names now enter CI (goshard.py refuses in every
// internal/api shard if one is missing from the package, and passes it to
// verdict.py in the shard that runs it).
func TestCIRequiresTheIsolationWalkInTheShardPlan(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := jobLines(string(b), "test")
	plan := -1
	for i, l := range lines {
		if l == "python3 tests/goshard.py" {
			plan = i
		}
	}
	if plan < 0 {
		t.Fatalf("positive control FAILED: no `python3 tests/goshard.py` line in the `test` job of %s", path)
	}
	// The plan step's folded `run: >-` block: the lines after the command, up
	// to the first that is not part of it.
	var block []string
	for _, l := range lines[plan+1:] {
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "- ") {
			break
		}
		block = append(block, l)
	}
	for _, name := range []string{
		"TestEveryRouteIsClassifiedForIsolation",
		"TestNoRouteLetsOneUserReachAnothersData",
		"TestAnotherUsersWritesNeverReachYourEventStream",
	} {
		found := false
		for _, l := range block {
			if l == "--require "+name {
				found = true
			}
		}
		if !found {
			t.Errorf("the `test` job's goshard.py plan step does not --require %s; "+
				"the isolation walk could stop running and CI stay green", name)
		}
	}
}

// TestCIPublishJobsCheckTheirNeedsExplicitly pins the fix for an ancestor-skip
// trap (PR #655 round 1). A job `if:` with no status function is an implicit
// `success()`, which checks every ANCESTOR, not only the direct `needs`
// (actions/runner#491). `main-verdict` is skipped on every non-tag run, and
// the test jobs are skipped on a tag whose main run vouched for it, so a
// job downstream of them with an implicit success() is skipped even when
// publish-gate passed: no images on main, no release on a tag.
//
// So: every job with `needs` carries `!cancelled()` in its job-level `if:`,
// and every job downstream of publish-gate (found from the needs graph, not
// a list of names) names each of its needs as `needs.<x>.result == 'success'`.
func TestCIPublishJobsCheckTheirNeedsExplicitly(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)
	// header: the job-level lines only (before `steps:`), so a step's `if:`
	// can never stand in for a missing job-level one.
	header := func(job string) []string {
		var out []string
		for _, l := range jobLines(src, job) {
			if l == "steps:" {
				break
			}
			out = append(out, l)
		}
		return out
	}
	field := func(lines []string, key string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, key+":") {
				return strings.TrimSpace(strings.TrimPrefix(l, key+":"))
			}
		}
		return ""
	}
	// needsOf reads `needs: x`, `needs: [x, y]`, or a block list of `- x` lines.
	needsOf := func(lines []string) (out []string, has bool) {
		for i, l := range lines {
			if l != "needs:" && !strings.HasPrefix(l, "needs: ") {
				continue
			}
			has = true
			if v := strings.Trim(field([]string{l}, "needs"), "[]"); v != "" {
				for _, n := range strings.Split(v, ",") {
					out = append(out, strings.TrimSpace(n))
				}
				return out, true
			}
			for _, m := range lines[i+1:] {
				if !strings.HasPrefix(m, "- ") {
					break
				}
				out = append(out, strings.TrimSpace(strings.TrimPrefix(m, "- ")))
			}
			return out, true
		}
		return nil, false
	}
	needs := map[string][]string{}
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") || !strings.HasSuffix(line, ":") || strings.HasPrefix(line, "  #") {
			continue
		}
		job := strings.TrimSuffix(strings.TrimSpace(line), ":")
		if ns, has := needsOf(header(job)); has {
			if len(ns) == 0 {
				t.Errorf("job %q has a `needs:` this test cannot read", job)
			}
			needs[job] = ns
			if !strings.Contains(field(header(job), "if"), "!cancelled()") {
				t.Errorf("job %q has `needs` but its job-level `if:` has no `!cancelled()`: an implicit "+
					"success() skips it whenever ANY ancestor was skipped (actions/runner#491)", job)
			}
		}
	}
	// Positive control: the walk must see the jobs this test is about.
	if len(needs) < 10 {
		t.Fatalf("positive control FAILED: only %d jobs with `needs` found in %s", len(needs), path)
	}
	// Every job downstream of publish-gate, transitively.
	down := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for job, ns := range needs {
			for _, n := range ns {
				if (n == "publish-gate" || down[n]) && !down[job] {
					down[job], changed = true, true
				}
			}
		}
	}
	for _, want := range []string{"image-muster", "image-claude-code-agent", "image-stt-cohere", "release"} {
		if !down[want] {
			t.Errorf("positive control FAILED: %q is not found downstream of publish-gate", want)
		}
	}
	for job := range down {
		cond := field(header(job), "if")
		for _, n := range needs[job] {
			if !strings.Contains(cond, "needs."+n+".result == 'success'") {
				t.Errorf("job %q needs %q but its `if:` does not require `needs.%s.result == 'success'`", job, n, n)
			}
		}
	}
}

// jobLines returns the trimmed lines of the two-space-indented `job:` block
// under `jobs:`; nil if there is none.
func jobLines(src, job string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && !strings.HasPrefix(line, "  #") {
			in = line == "  "+job+":"
			continue
		}
		if in {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}
