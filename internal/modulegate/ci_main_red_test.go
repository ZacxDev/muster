package modulegate

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// TestCIMainRedNeedsWhatPublishGateNeeds pins ci.yml's `main-red` job: on a
// push to main it opens, updates or closes ONE `main-red` issue
// (.github/scripts/main_red.py), so a red main reaches the repository's
// watchers and the next green main closes it.
//
// What it pins, each a way the notifier goes quiet or over-reaches:
//   - the job exists, runs only on a push to main, and runs when its needs
//     FAILED (`!cancelled()`, never an implicit success());
//   - its needs are EXACTLY publish-gate's, both ways: a test job added to
//     publish-gate and not here would go red without a notification, and one
//     here and not there would call main red over a job nothing gates on;
//   - it is the ONLY job granted `issues: write`, and it is granted nothing
//     beyond `contents: read` besides;
//   - its step hands main_red.py the needs context.
//
// WHAT THIS CANNOT SEE: it reads the YAML text, not a run. main_red.py's own
// decisions are tests/main_red_test.py's.
// ---------------------------------------------------------------------------
func TestCIMainRedNeedsWhatPublishGateNeeds(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)
	job := jobLines(src, "main-red")
	if len(job) == 0 {
		t.Fatalf("no `main-red:` job in %s: nothing notifies anyone when main goes red", path)
	}
	header := func(lines []string) []string {
		var out []string
		for _, l := range lines {
			if l == "steps:" {
				break
			}
			out = append(out, l)
		}
		return out
	}
	needs := func(lines []string) []string {
		for _, l := range header(lines) {
			if v, ok := strings.CutPrefix(l, "needs: "); ok {
				var out []string
				for _, n := range strings.Split(strings.Trim(v, "[]"), ",") {
					out = append(out, strings.TrimSpace(n))
				}
				sort.Strings(out)
				return out
			}
		}
		return nil
	}

	gate := needs(jobLines(src, "publish-gate"))
	if len(gate) == 0 {
		t.Fatalf("positive control FAILED: read no needs for publish-gate in %s", path)
	}
	if got := needs(job); strings.Join(got, ",") != strings.Join(gate, ",") {
		t.Errorf("main-red needs %v, want exactly publish-gate's %v", got, gate)
	}

	wantIf := "if: ${{ !cancelled() && github.event_name == 'push' && github.ref == 'refs/heads/main' }}"
	found := false
	for _, l := range header(job) {
		if strings.HasPrefix(l, "if:") {
			found = true
			if l != wantIf {
				t.Errorf("main-red's condition is\n  %s\nwant\n  %s", l, wantIf)
			}
		}
	}
	if !found {
		t.Errorf("main-red has no job-level `if:`: it would run on pull requests and tags, and skip when a need failed")
	}

	// Its permissions block, exactly.
	var perms []string
	for i, l := range header(job) {
		if l == "permissions:" {
			for _, p := range header(job)[i+1:] {
				if !strings.Contains(p, ": ") || strings.HasPrefix(p, "steps") {
					break
				}
				if p == "runs-on: ubuntu-latest" || strings.HasPrefix(p, "if:") || strings.HasPrefix(p, "needs:") {
					break
				}
				perms = append(perms, p)
			}
		}
	}
	sort.Strings(perms)
	if strings.Join(perms, "; ") != "contents: read; issues: write" {
		t.Errorf("main-red's permissions are %q, want exactly contents: read and issues: write", perms)
	}

	// No OTHER line in the workflow grants issues: write.
	if n := strings.Count(src, "issues: write"); n != 1 {
		t.Errorf("%d `issues: write` grants in %s, want 1 (main-red's)", n, path)
	}

	var step string
	for _, l := range job {
		if strings.Contains(l, ".github/scripts/main_red.py") {
			step = l
		}
	}
	if step == "" {
		t.Errorf("main-red does not run .github/scripts/main_red.py")
	}
	if !strings.Contains(strings.Join(job, "\n"), "NEEDS: ${{ toJSON(needs) }}") {
		t.Errorf("main-red does not pass the needs context to main_red.py as NEEDS")
	}
}
