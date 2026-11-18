package agentprovision

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
	"github.com/ZacxDev/muster/internal/podagent"
)

// 🔴 THE GUARD ON THE WORKTREE SCRIPT'S FETCHES FROM THE KEY HOLDER'S MIRROR:
// each must carry --upload-pack=$7 (podagent.MirrorUploadPack). On a git newer
// than the image's 2.39 a fetch from another user's repository succeeds without
// it, so these read what git was HANDED (a recording shim first on PATH), not
// whether the fetch worked. Every expected fetch is counted, so "no matching
// call" cannot pass.

// mirrorFetches is every `git fetch` the shim logged whose argv names url.
func mirrorFetches(t *testing.T, log, url string) [][]string {
	t.Helper()
	var out [][]string
	for _, argv := range worktreetest.GitShimCalls(t, log) {
		if worktreetest.GitSubcommand(argv) == "fetch" && slices.Contains(argv, url) {
			out = append(out, argv)
		}
	}
	return out
}

func assertUploadPack(t *testing.T, fetches [][]string, want string, n int) {
	t.Helper()
	if len(fetches) != n {
		t.Fatalf("instrument: the shim saw %d fetch(es) from the mirror, want %d: %q", len(fetches), n, fetches)
	}
	for _, argv := range fetches {
		if !slices.Contains(argv, "--upload-pack="+want) {
			t.Errorf("MIRROR UPLOAD-PACK MISSING: a fetch from the mirror ran without --upload-pack=%q: %q", want, argv)
		}
	}
}

// TestEveryWorktreeScriptFetchFromAMirrorCarriesItsUploadPack runs the script
// itself with an upload-pack ($7) on the fixture's origin as the "mirror" and
// a config_ref the checkout's origin/HEAD does not reach — a combination no
// deploy-key pool sends (they never overlay), chosen so ALL THREE fetch lines
// run: the pull request's head, the base, and the default branch. (The last one
// then refuses the config_ref, after its fetch; that is not what is checked.)
func TestEveryWorktreeScriptFetchFromAMirrorCarriesItsUploadPack(t *testing.T) {
	root := t.TempDir()
	r := worktreetest.New(t, filepath.Join(root, "widgets"), filepath.Join(root, "markers"))
	up, err := podagent.MirrorUploadPack(r.Origin)
	if err != nil {
		t.Fatal(err)
	}
	log := worktreetest.GitShim(t, os.Getenv("PATH"))
	args := append([]string{"-c", jobOverlayScript, "muster-job-worktree", r.Checkout, filepath.Join(root, ".muster-jobs", "41"),
		r.Commit, r.Origin, "7", r.Stray, up, "1", r.Default}, JobOverlayRootFiles...)
	out, _ := exec.Command("sh", args...).CombinedOutput()
	if !strings.Contains(string(out), ConfigRefNotOnDefaultBranchMessage) {
		t.Fatalf("instrument: the script did not reach the default branch's fetch and its refusal: %s", out)
	}
	assertUploadPack(t, mirrorFetches(t, log, r.Origin), up, 3)
}

// TestADeployKeyJobsFetchesFromTheMirrorCarryItsUploadPack: through the
// adapter, on a deploy-key pool, the pull request's and the base's fetches from
// the mirror carry podagent.MirrorUploadPack(the mirror) — the string the
// image's cc-deploykey-smoke pins.
func TestADeployKeyJobsFetchesFromTheMirrorCarryItsUploadPack(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), false)
	log := worktreetest.GitShim(t, os.Getenv("PATH"))
	if _, err := prepareWithBase(a, r, poolAgent(privatePool), r.Stray); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The local driver places the mirror under its root.
	mirror := filepath.Join(d.root, ".mirror", "repo.git")
	up, err := podagent.MirrorUploadPack(mirror)
	if err != nil {
		t.Fatal(err)
	}
	assertUploadPack(t, mirrorFetches(t, log, mirror), up, 2)
}
