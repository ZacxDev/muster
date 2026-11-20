package agentprovision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
)

// TestSweepRemovesEveryLeftJobWorktreeAndRef: worktrees two earlier jobs left
// (their server stopped mid-turn: registered with git, with their refs) are
// removed, git forgets them and their refs/muster-jobs refs are deleted;
// anything in .muster-jobs that is not named by a job id is left, and the
// agent's checkout is not moved.
func TestSweepRemovesEveryLeftJobWorktreeAndRef(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	a.anonGitBase = r.AnonBase
	for _, id := range []int64{41, 42} {
		if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, id, worktreetest.RepoName, worktreetest.PR, r.Commit, "", ""); err != nil {
			t.Fatalf("prepare %d: %v", id, err)
		}
	}
	jobsDir := filepath.Join(d.root, ".muster-jobs")
	other := filepath.Join(jobsDir, "not-a-job")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if refs := worktreetest.Git(t, r.Checkout, "for-each-ref", "refs/muster-jobs/"); strings.Count(refs, "\n")+1 != 2 {
		t.Fatalf("the fixture's job refs = %q, want 2", refs)
	}

	gone, err := a.SweepJobWorktrees(context.Background(), workspaceAgent)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, id := range []string{"41", "42"} {
		if _, err := os.Lstat(filepath.Join(jobsDir, id)); !os.IsNotExist(err) {
			t.Errorf("job worktree %s is still there (lstat: %v)", id, err)
		}
	}
	if _, err := os.Lstat(other); err != nil {
		t.Fatalf("a directory not named by a job id was removed: %v", err)
	}
	if list := worktreetest.Git(t, r.Checkout, "worktree", "list", "--porcelain"); strings.Contains(list, ".muster-jobs") {
		t.Fatalf("git still lists a job worktree:\n%s", list)
	}
	if refs := worktreetest.Git(t, r.Checkout, "for-each-ref", "refs/muster-jobs/"); refs != "" {
		t.Fatalf("job refs left: %q", refs)
	}
	// The local driver rewrites the workspace path in the arguments, so the
	// script names the temporary directory.
	want := []string{"removed " + jobsDir + "/41", "removed " + jobsDir + "/42",
		"deleted refs/muster-jobs/41", "deleted refs/muster-jobs/42"}
	if strings.Join(gone, "|") != strings.Join(want, "|") {
		t.Fatalf("sweep reported %q, want %q", gone, want)
	}
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "HEAD"); got != r.Default {
		t.Fatalf("the checkout's HEAD = %s, want %s", got, r.Default)
	}
	// Nothing left: not an error, nothing reported.
	if gone, err := a.SweepJobWorktrees(context.Background(), workspaceAgent); err != nil || len(gone) != 0 {
		t.Fatalf("a second sweep = %q, %v; want nothing", gone, err)
	}
}
