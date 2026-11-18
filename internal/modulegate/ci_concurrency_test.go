package modulegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A NEWER PUSH SUPERSEDES AN OLDER RUN ON THE SAME PR ONLY; A `main` PUSH AND
// A TAG RUN ARE NEVER CANCELLED OR DROPPED.
//
// HISTORY. The group was first `ci-${{ github.ref }}` with
// `cancel-in-progress: true` for every event, under which a later push to
// `main` cancelled the run of the `main` commit before it. It then became "PR
// runs only; every other run in its own `run_id` group". From 2026-11-16 a
// newer push to `main` cancelled the older `main` run, relying on the release
// tool releasing the newest green `main` descendant; measured 2026-11-18, 38
// of the 60 newest `main` runs were cancelled and `main` went 3 h+ with no
// verdict and no green descendant (ci.yml carries the numbers). So `main` is
// back in a `run_id` group of its own.
//
// 🔴 THE TAG HALF IS THE LOAD-BEARING ONE. A vX.Y.Z tag run publishes the
// release images and the GitHub release; it must keep a group of its own
// (`github.run_id`) and `cancel-in-progress: false`. A GROUP, NOT ONLY THE
// FLAG: a group holds one running and ONE pending run, and a newer pending run
// replaces the older pending one whatever the flag says — so a tag sharing any
// group could still be dropped while queued. The group expression routes ONLY
// a pull_request event to a shared group.
//
// 🔴 EXACT STRING, NOT A SUBSTRING. The obvious substring check — "the group
// mentions run_id" — accepts `github.ref || github.run_id`, which is always the
// ref, i.e. tags in a shared group. The block is three lines; a deliberate
// change to it is a deliberate change to this test.
//
// 🔴 WHAT THIS CANNOT SEE: it reads the YAML text, so it proves the expression
// is spelled as intended, never how GitHub evaluates it. The PR-cancel half is
// observed live on any PR; the `main` and tag halves only after a merge.
// It also reads ONLY the workflow-level block: a job-level `concurrency:` key is
// not inspected, deliberately — a job may legitimately serialise itself.
// ---------------------------------------------------------------------------

const wantCIConcurrency = `concurrency:
  group: ci-${{ github.event_name == 'pull_request' && github.ref || github.run_id }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}`

// TestCIConcurrencySupersedesPullRequestRunsOnly pins the workflow-level
// concurrency block.
func TestCIConcurrencySupersedesPullRequestRunsOnly(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := topLevelBlock(string(b), "concurrency")
	// Positive control: the extractor must find the block, or the comparison
	// below fails for a reason that says nothing about the block's content.
	if got == "" {
		t.Fatalf("positive control FAILED: no top-level `concurrency:` block in %s", path)
	}
	if got != wantCIConcurrency {
		t.Errorf("top-level concurrency block in %s is\n%s\n\nwant\n%s\n\n"+
			"only PR runs may share a per-ref, cancelling group; every other event "+
			"(a `main` push and a vX.Y.Z tag above all) must get its own `github.run_id` group and never be cancelled.",
			path, got, wantCIConcurrency)
	}
}

// topLevelBlock returns the first column-0 `key:` line plus the indented lines
// under it, trailing blank lines trimmed; "" if there is none.
func topLevelBlock(src, key string) string {
	var block []string
	for _, line := range strings.Split(src, "\n") {
		if block == nil {
			if strings.HasPrefix(line, key+":") {
				block = append(block, line)
			}
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		block = append(block, line)
	}
	return strings.TrimRight(strings.Join(block, "\n"), " \n")
}
