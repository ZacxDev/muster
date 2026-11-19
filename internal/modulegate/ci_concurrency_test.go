package modulegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THREE KINDS OF GROUP: a newer PR commit cancels its PR's older run; `main`
// keeps ONE run going and ONE (the newest push) waiting; a vX.Y.Z tag run is
// never cancelled or replaced.
//
// HISTORY. The group was first `ci-${{ github.ref }}` with
// `cancel-in-progress: true` for every event. It then became "PR runs only;
// every other run in its own `run_id` group". 493314225 made a newer push to
// `main` cancel the running `main` run, and `main` went hours with no verdict.
// b1c2ae4c2 put every `main` push back in a group of its own, and a burst of
// merges then queued one full suite per push for hours. Now (operator's
// choice) `main` pushes share group `main` with cancel-in-progress OFF: the
// running run always finishes, and GitHub replaces the one pending run with the
// newest push. ci.yml carries the numbers.
//
// 🔴 THE TAG HALF IS THE LOAD-BEARING ONE. A vX.Y.Z tag run publishes the
// release images and the GitHub release; it must keep a group of its own
// (`github.run_id`) and `cancel-in-progress: false`. A GROUP, NOT ONLY THE
// FLAG: a group holds one running and ONE pending run, and a newer pending run
// replaces the older pending one whatever the flag says — so a tag sharing any
// group could still be dropped while queued.
//
// 🔴 EXACT STRING, NOT A SUBSTRING. The obvious substring check — "the group
// mentions run_id" — accepts `github.ref || github.run_id`, which is always the
// ref, i.e. tags in a shared group. The block is three lines; a deliberate
// change to it is a deliberate change to this test.
//
// 🔴 AND EVALUATED PER EVENT, because the exact string cannot see a precedence
// slip: TestCIConcurrencyGroupsPerEvent evaluates the pinned expressions (with
// GitHub's `&&`/`||`, which return an operand, and bind `&&` tighter) for a PR,
// a push to main, a RE-RUN of a main run (run_attempt 2: its own group, so it
// can neither replace the newest push's waiting run nor be replaced) and a tag
// push, and asserts the group and the flag for each.
//
// 🔴 WHAT THIS CANNOT SEE: it reads the YAML text, so it proves the expression
// is spelled as intended, never how GitHub evaluates it. The PR-cancel half is
// observed live on any PR; the `main` and tag halves only after a merge.
// It also reads ONLY the workflow-level block: a job-level `concurrency:` key is
// not inspected, deliberately — a job may legitimately serialise itself.
// ---------------------------------------------------------------------------

const wantCIConcurrency = `concurrency:
  group: ci-${{ github.event_name == 'pull_request' && github.ref || github.event_name == 'push' && github.ref == 'refs/heads/main' && github.run_attempt == '1' && 'main' || github.run_id }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}`

// TestCIConcurrencyGroups pins the workflow-level concurrency block.
func TestCIConcurrencyGroups(t *testing.T) {
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
			"PR runs share a per-ref cancelling group, main pushes share group `main` without cancelling, "+
			"and every other event (a vX.Y.Z tag above all) gets its own `github.run_id` group.",
			path, got, wantCIConcurrency)
	}
}

// TestCIConcurrencyGroupsPerEvent evaluates the pinned group and
// cancel-in-progress expressions for each event the workflow runs on.
func TestCIConcurrencyGroupsPerEvent(t *testing.T) {
	var group, cancel string
	for _, l := range strings.Split(wantCIConcurrency, "\n") {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "group: ci-${{ "); ok {
			group = strings.TrimSuffix(v, " }}")
		}
		if v, ok := strings.CutPrefix(l, "cancel-in-progress: ${{ "); ok {
			cancel = strings.TrimSuffix(v, " }}")
		}
	}
	for _, c := range []struct {
		name, event, ref string
		wantGroup        string
		wantCancel       bool
	}{
		{"pull request", "pull_request", "refs/pull/7/merge", "ci-refs/pull/7/merge", true},
		{"push to main", "push", "refs/heads/main", "ci-main", false},
		{"re-run of a main run", "push", "refs/heads/main", "ci-4242", false},
		{"tag push", "push", "refs/tags/v1.2.3", "ci-4242", false},
	} {
		attempt := "1"
		if c.name == "re-run of a main run" {
			attempt = "2"
		}
		ctx := map[string]string{"github.event_name": c.event, "github.ref": c.ref, "github.run_id": "4242",
			"github.run_attempt": attempt}
		if g := "ci-" + evalExpr(t, group, ctx); g != c.wantGroup {
			t.Errorf("%s: group %q, want %q", c.name, g, c.wantGroup)
		}
		if got := evalExpr(t, cancel, ctx) == "true"; got != c.wantCancel {
			t.Errorf("%s: cancel-in-progress %v, want %v", c.name, got, c.wantCancel)
		}
	}
}

// evalExpr evaluates the expression subset the concurrency block uses: `||` of
// `&&` chains of `a == 'lit'`, a context name, or a 'literal'. Like GitHub's,
// `&&`/`||` return an operand (falsy: "" and false), and `&&` binds tighter.
// A comparison yields "true" or "false"; "false" is falsy.
func evalExpr(t *testing.T, expr string, ctx map[string]string) string {
	t.Helper()
	truthy := func(v string) bool { return v != "" && v != "false" }
	atom := func(a string) string {
		a = strings.TrimSpace(a)
		if l, r, ok := strings.Cut(a, " == "); ok {
			if strings.TrimSpace(l) != "" && ctx[strings.TrimSpace(l)] == strings.Trim(strings.TrimSpace(r), "'") {
				return "true"
			}
			return "false"
		}
		if strings.HasPrefix(a, "'") {
			return strings.Trim(a, "'")
		}
		v, ok := ctx[a]
		if !ok {
			t.Fatalf("evalExpr: unknown operand %q", a)
		}
		return v
	}
	var out string
	for _, alt := range strings.Split(expr, " || ") {
		v := ""
		for _, term := range strings.Split(alt, " && ") {
			v = atom(term)
			if !truthy(v) {
				break
			}
		}
		out = v
		if truthy(v) {
			return v
		}
	}
	return out
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
