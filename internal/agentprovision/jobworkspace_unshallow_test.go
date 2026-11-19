package agentprovision

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
	"github.com/ZacxDev/muster/internal/podagent"
)

// deepenOrigin gives the fixture's origin `n` (> 5) more commits on main (each
// adding a file of its own, pairwise distinct), pushes pull request `pr` as
// one commit on top of main~5 — so its merge-base with main's tip lies below a
// depth-1 boundary, with n-5 commits of history under it — and re-makes the
// checkout as a DEPTH-1 clone of origin. It returns the pull request's head.
//
// What a shallow clone's fetch of that pull request re-downloads is the
// history BELOW the merge-base (the server cannot count commits past the
// client's boundary as common): placing the merge-base near the root instead
// would make it nearly nothing, which is why it is main~5, not main~(n-1).
func deepenOrigin(t *testing.T, r worktreetest.Repo, n, pr int) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	worktreetest.Git(t, "", "clone", "-q", "file://"+r.Origin, src)
	for i := 0; i < n; i++ {
		name := "h" + strconv.Itoa(i)
		if err := os.WriteFile(filepath.Join(src, name), []byte(strings.Repeat(name+"\n", 200)), 0o644); err != nil {
			t.Fatal(err)
		}
		worktreetest.Git(t, src, "add", "--", name)
		worktreetest.Git(t, src, "commit", "-q", "-m", name)
	}
	worktreetest.Git(t, src, "push", "-q", "origin", "main")
	worktreetest.Git(t, src, "checkout", "-q", "-b", "pr"+strconv.Itoa(pr), "HEAD~5")
	if err := os.WriteFile(filepath.Join(src, "pr-file"), []byte("pr "+strconv.Itoa(pr)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktreetest.Git(t, src, "add", "--", "pr-file")
	worktreetest.Git(t, src, "commit", "-q", "-m", "pr")
	head := worktreetest.Git(t, src, "rev-parse", "HEAD")
	worktreetest.Git(t, src, "push", "-q", "origin", "HEAD:refs/pull/"+strconv.Itoa(pr)+"/head")
	if err := os.RemoveAll(r.Checkout); err != nil {
		t.Fatal(err)
	}
	worktreetest.Git(t, "", "clone", "-q", "--depth", "1", "file://"+r.Origin, r.Checkout)
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("fixture: the depth-1 checkout is not shallow (%s)", got)
	}
	return head
}

// objectCount is every object the repository at dir holds, loose and packed.
func objectCount(t *testing.T, dir string) int {
	t.Helper()
	total := 0
	for _, l := range strings.Split(worktreetest.Git(t, dir, "count-objects", "-v"), "\n") {
		k, v, _ := strings.Cut(l, ": ")
		if k == "count" || k == "in-pack" {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatal(err)
			}
			total += n
		}
	}
	return total
}

// TestAShallowCheckoutIsUnshallowedOnceBeforeTheFetch: a depth-1 checkout is
// made complete by a job's worktree script, and a SECOND job, on a pull
// request whose merge-base lies far below the old boundary, then adds only
// that pull request's own objects (its commit, tree and blob: at most 3).
//
// THE POSITIVE CONTROL: the same second fetch into a fresh depth-1 clone (no
// unshallow) adds the history below the merge-base — at least the 25 commits
// under main~5 — so the fixture does carry the hazard.
func TestAShallowCheckoutIsUnshallowedOnceBeforeTheFetch(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	a.anonGitBase = r.AnonBase
	pr9 := deepenOrigin(t, r, 30, 9)

	if _, err := prepareJob(a, r, r.Commit); err != nil {
		t.Fatalf("prepare (pull request 7): %v", err)
	}
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "--is-shallow-repository"); got != "false" {
		t.Fatalf("after the first job the checkout is still shallow (%s)", got)
	}
	if err := a.RemoveJobWorktree(context.Background(), workspaceAgent, overlayJobID); err != nil {
		t.Fatal(err)
	}
	before := objectCount(t, r.Checkout)
	if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, 42, worktreetest.RepoName, 9, pr9, "", ""); err != nil {
		t.Fatalf("prepare (pull request 9): %v", err)
	}
	added := objectCount(t, r.Checkout) - before
	t.Logf("second job's fetch into the unshallowed checkout added %d object(s)", added)
	if added > 3 {
		t.Fatalf("the second job's fetch added %d objects to the unshallowed checkout, want at most 3", added)
	}

	// Control: a fresh depth-1 clone fetching the same pull request.
	ctl := filepath.Join(t.TempDir(), "ctl")
	worktreetest.Git(t, "", "clone", "-q", "--depth", "1", "file://"+r.Origin, ctl)
	before = objectCount(t, ctl)
	worktreetest.Git(t, ctl, "fetch", "-q", "--no-tags", r.AnonBase+worktreetest.RepoName, "+refs/pull/9/head:refs/muster-jobs/ctl")
	added = objectCount(t, ctl) - before
	t.Logf("control: the same fetch into a depth-1 clone added %d object(s)", added)
	if added < 25 {
		t.Fatalf("control: the shallow clone's fetch added only %d objects; the fixture does not carry the re-download", added)
	}
}

// TestAFailedUnshallowDoesNotFailTheJob: with the origin's HEAD naming no
// branch, `git fetch --unshallow` of it fails; the job's worktree is made all
// the same, at the commit, and the checkout stays shallow. The shim proves the
// unshallow was ATTEMPTED (else this would pass with the step missing).
func TestAFailedUnshallowDoesNotFailTheJob(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	a.anonGitBase = r.AnonBase
	deepenOrigin(t, r, 5, 9)
	worktreetest.Git(t, r.Origin, "symbolic-ref", "HEAD", "refs/heads/no-such-branch")
	log := worktreetest.GitShim(t, os.Getenv("PATH"))

	dir, err := prepareJob(a, r, r.Commit)
	if err != nil {
		t.Fatalf("prepare failed when only the unshallow did: %v", err)
	}
	if got := worktreetest.Git(t, filepath.Join(d.root, ".muster-jobs", "41"), "rev-parse", "HEAD"); got != r.Commit {
		t.Fatalf("worktree %s HEAD = %s, want %s", dir, got, r.Commit)
	}
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("the checkout is not shallow any more (%s): the unshallow did not fail", got)
	}
	tried := 0
	for _, argv := range worktreetest.GitShimCalls(t, log) {
		if worktreetest.GitSubcommand(argv) == "fetch" && slices.Contains(argv, "--unshallow") {
			tried++
		}
	}
	if tried != 1 {
		t.Fatalf("instrument: %d unshallow fetch(es) were run, want 1", tried)
	}
}

// TestACompleteCheckoutIsNotUnshallowed: the fixture's ordinary (complete)
// clone runs no `fetch --unshallow` (it would fail "on a complete repository").
func TestACompleteCheckoutIsNotUnshallowed(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	log := worktreetest.GitShim(t, os.Getenv("PATH"))
	if _, err := prepareJob(a, r, r.Commit); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	for _, argv := range worktreetest.GitShimCalls(t, log) {
		if worktreetest.GitSubcommand(argv) == "fetch" {
			fetches++
			if slices.Contains(argv, "--unshallow") {
				t.Fatalf("a complete checkout was unshallowed: %q", argv)
			}
		}
	}
	if fetches == 0 {
		t.Fatal("instrument: the shim saw no fetch at all")
	}
}

// TestAShallowCheckoutsUnshallowFromAMirrorCarriesItsUploadPack: from a
// deploy-key pool's mirror, the unshallow is a fourth fetch line, and it too
// carries the mirror's --upload-pack.
func TestAShallowCheckoutsUnshallowFromAMirrorCarriesItsUploadPack(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	a.anonGitBase = r.AnonBase
	deepenOrigin(t, r, 5, 9)
	up, err := podagent.MirrorUploadPack(r.Origin)
	if err != nil {
		t.Fatal(err)
	}
	log := worktreetest.GitShim(t, os.Getenv("PATH"))
	root := d.root
	args := append([]string{"-c", jobOverlayScript, "muster-job-worktree", r.Checkout, filepath.Join(root, ".muster-jobs", "41"),
		r.Commit, r.Origin, "7", r.Stray, up, "1", r.Default, "60"}, JobOverlayRootFiles...)
	out, _ := execSh(args)
	if !strings.Contains(out, ConfigRefNotOnDefaultBranchMessage) {
		t.Fatalf("instrument: the script did not reach the default branch's fetch and its refusal: %s", out)
	}
	fetches := mirrorFetches(t, log, r.Origin)
	assertUploadPack(t, fetches, up, 4)
	if !slices.Contains(fetches[0], "--unshallow") {
		t.Fatalf("the first fetch from the mirror is not the unshallow: %q", fetches[0])
	}
}

// TestTheUnshallowBudgetIsAShareOfTheJobsDeadline: at most UnshallowMaxBudget,
// at most a third of what is left of the deadline, and none below
// unshallowMinBudget (an unshallow cut that short could not finish anyway).
func TestTheUnshallowBudgetIsAShareOfTheJobsDeadline(t *testing.T) {
	if got := unshallowBudget(context.Background()); got != 600 {
		t.Errorf("no deadline: %d s, want 600", got)
	}
	for _, c := range []struct {
		left time.Duration
		lo   int
		hi   int
	}{
		{4 * time.Hour, 600, 600},
		{30 * time.Minute, 598, 600},
		{9 * time.Minute, 178, 180},
		{89 * time.Second, 0, 0},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), c.left)
		got := unshallowBudget(ctx)
		cancel()
		if got < c.lo || got > c.hi {
			t.Errorf("%s left: %d s, want %d..%d", c.left, got, c.lo, c.hi)
		}
	}
}

// execSh runs sh with args, returning its combined output.
func execSh(args []string) (string, error) {
	out, err := exec.Command("sh", args...).CombinedOutput()
	return string(out), err
}
