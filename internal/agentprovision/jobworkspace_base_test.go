package agentprovision

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
	"github.com/ZacxDev/muster/internal/agents"
)

// A JOB'S base_sha, through the real worktree script and real git. The base is
// the fixture's Stray: on the origin only (branch "stray"), never fetched by
// the checkout, and not an ancestor of the pull request's head — so it is in
// the checkout after the job ONLY if the job fetched it.

func prepareWithBase(a *Adapter, r worktreetest.Repo, ag agents.Agent, base string) (string, error) {
	a.anonGitBase = r.AnonBase
	return a.PrepareJobWorktree(context.Background(), ag, overlayJobID, worktreetest.RepoName, worktreetest.PR, r.Commit, "", false, base)
}

// hasObject reports whether the checkout holds commit sha.
func hasObject(dir, sha string) bool {
	return exec.Command("git", "-C", dir, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

// assertBaseFetched: the base is at refs/muster-jobs/41-base, and from the
// job's worktree `git diff <base>...HEAD` runs with no remote at all; the ref
// is gone with the worktree.
func assertBaseFetched(t *testing.T, a *Adapter, ag agents.Agent, d *localExecDriver, r worktreetest.Repo) {
	t.Helper()
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "refs/muster-jobs/41-base"); got != r.Stray {
		t.Fatalf("refs/muster-jobs/41-base = %s, want the base %s", got, r.Stray)
	}
	wt := filepath.Join(d.root, ".muster-jobs", "41")
	if diff := worktreetest.Git(t, wt, "-c", "remote.origin.url=/nonexistent", "diff", "--name-only", r.Stray+"...HEAD"); !strings.Contains(diff, "CLAUDE.md") {
		t.Fatalf("git diff <base>...HEAD in the worktree = %q, want the pull request's changes", diff)
	}
	if err := a.RemoveJobWorktree(context.Background(), ag, overlayJobID); err != nil {
		t.Fatal(err)
	}
	if refs := worktreetest.Git(t, r.Checkout, "for-each-ref", "refs/muster-jobs/"); refs != "" {
		t.Fatalf("the job's refs outlived it: %s", refs)
	}
}

// TestAJobsBaseIsFetchedAnonymouslyForAPublicPool: with no deploy key the base
// comes from the anonymous URL, like the head.
func TestAJobsBaseIsFetchedAnonymouslyForAPublicPool(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	if hasObject(r.Checkout, r.Stray) {
		t.Fatal("instrument: the checkout already holds the base, so a fetch could not be told from none")
	}
	if _, err := prepareWithBase(a, r, workspaceAgent, r.Stray); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if args := lastArgs(t, d); args[9] != r.Stray {
		t.Fatalf("base argument = %q, want %s", args[9], r.Stray)
	}
	assertBaseFetched(t, a, workspaceAgent, d, r)
}

// TestAJobsBaseIsFetchedWithThePoolsDeployKey: on a deploy-key pool the base
// goes through ssh with the key, like the head: the fake logs two upload-packs
// after the write probe.
func TestAJobsBaseIsFetchedWithThePoolsDeployKey(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	log := worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), false)
	ag := poolAgent(privatePool)
	if _, err := prepareWithBase(a, r, ag, r.Stray); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	got := worktreetest.SSHLog(t, log)
	if n := strings.Count(got, "git-upload-pack '/acme/widgets.git'"); n != 2 {
		t.Fatalf("ssh served %d upload-pack(s), want the head's and the base's:\n%s", n, got)
	}
	assertBaseFetched(t, a, ag, d, r)
}

// TestABaseThatCannotBeFetchedFailsTheJob: a commit the repository does not
// have fails the job naming the base, and no worktree is made. An abbreviated
// base never reaches the instance.
func TestABaseThatCannotBeFetchedFailsTheJob(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	missing := strings.Repeat("e", 40)
	_, err := prepareWithBase(a, r, workspaceAgent, missing)
	if err == nil || !strings.Contains(err.Error(), BaseFetchMessage) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("prepare = %v, want an error carrying %q naming %s", err, BaseFetchMessage, missing)
	}
	if _, err := os.Lstat(filepath.Join(d.root, ".muster-jobs", "41")); !os.IsNotExist(err) {
		t.Fatalf("a worktree was created (lstat: %v)", err)
	}
	before := d.count()
	if _, err := prepareWithBase(a, r, workspaceAgent, r.Stray[:12]); err == nil {
		t.Fatal("an abbreviated base was accepted")
	}
	if d.count() != before {
		t.Fatalf("the refusal ran %d exec(s)", d.count()-before)
	}
}
