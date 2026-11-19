package agentprovision

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
)

// packCount is how many packs the repository at dir holds.
func packCount(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, ".git", "objects", "pack"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".pack") {
			n++
		}
	}
	return n
}

// bloat gives the checkout at dir n packs the way a shallow worker's job
// fetches do: each round commits a file of its own to branch b (so every
// commit stays reachable) and then writes a pack holding EVERY object so far —
// all of the previous pack's objects again plus the new ones — and drops the
// loose copies. It returns the commits, oldest first.
func bloat(t *testing.T, dir string, n int) []string {
	t.Helper()
	worktreetest.Git(t, dir, "config", "gc.auto", "0")
	worktreetest.Git(t, dir, "checkout", "-q", "-b", "b")
	var commits []string
	for i := 0; i < n; i++ {
		name := "f" + strconv.Itoa(i)
		// Pairwise distinct contents, sized so the packs are not trivial.
		body := strings.Repeat(name+" ", 2000)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		worktreetest.Git(t, dir, "add", "--", name)
		worktreetest.Git(t, dir, "commit", "-q", "-m", name)
		commits = append(commits, worktreetest.Git(t, dir, "rev-parse", "HEAD"))
		sh := exec.Command("sh", "-c", `git rev-list --objects --all | git pack-objects -q .git/objects/pack/pack >/dev/null && git prune-packed`)
		sh.Dir = dir
		if out, err := sh.CombinedOutput(); err != nil {
			t.Fatalf("pack round %d: %v: %s", i, err, out)
		}
	}
	return commits
}

// TestMaintenanceConsolidatesManyPacksAndKeepsEveryReachableObject: a checkout
// with 30 overlapping packs (each holding all of the previous one's objects
// again, as a shallow worker's job fetches leave them) is brought down to at
// most 2 packs by ONE pass, and every commit is still there and the repository
// is connected (git fsck --connectivity-only).
func TestMaintenanceConsolidatesManyPacksAndKeepsEveryReachableObject(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	commits := bloat(t, r.Checkout, 30)
	if got := packCount(t, r.Checkout); got < 30 {
		t.Fatalf("the fixture has %d packs, want at least 30", got)
	}
	report, err := a.MaintainJobRepository(context.Background(), workspaceAgent)
	if err != nil {
		t.Fatalf("maintain: %v\n%s", err, report)
	}
	if got := packCount(t, r.Checkout); got > 2 {
		t.Fatalf("after one pass the checkout has %d packs, want at most 2\n%s", got, report)
	}
	for _, c := range commits {
		worktreetest.Git(t, r.Checkout, "cat-file", "-e", c+"^{commit}")
	}
	worktreetest.Git(t, r.Checkout, "fsck", "--connectivity-only", "--no-progress")
	if !strings.Contains(report, "packs ") {
		t.Fatalf("the report does not say what the pass did:\n%s", report)
	}
}

// gitConfig is the checkout's own value of key ("" when unset).
func gitConfig(dir, key string) string {
	out, _ := exec.Command("git", "-C", dir, "config", "--local", "--get", key).Output()
	return strings.TrimSpace(string(out))
}

// TestMaintenanceClearsAStaleGCLogAndTurnsAutoGCOff: a gc.log older than the
// limit is printed and removed, one younger is left; the checkout's own config
// gets gc.auto=0, gc.autoPackLimit=0 and maintenance.auto=false, over values a
// session set — and on a pass that is NOT due too, so a session cannot turn
// auto-gc back on between passes.
func TestMaintenanceClearsAStaleGCLogAndTurnsAutoGCOff(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	gcLog := filepath.Join(r.Checkout, ".git", "gc.log")
	const said = "warning: There are too many unreachable loose objects; run 'git prune' to remove them."
	if err := os.WriteFile(gcLog, []byte(said+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(gcLog, old, old); err != nil {
		t.Fatal(err)
	}
	worktreetest.Git(t, r.Checkout, "config", "gc.auto", "6700")
	worktreetest.Git(t, r.Checkout, "config", "gc.autoPackLimit", "50")
	worktreetest.Git(t, r.Checkout, "config", "maintenance.auto", "true")

	report, err := a.MaintainJobRepository(context.Background(), workspaceAgent)
	if err != nil {
		t.Fatalf("maintain: %v\n%s", err, report)
	}
	if _, err := os.Lstat(gcLog); !os.IsNotExist(err) {
		t.Fatalf("the stale gc.log is still there (lstat: %v)\n%s", err, report)
	}
	if !strings.Contains(report, said) {
		t.Fatalf("the report does not carry what the gc.log said:\n%s", report)
	}
	want := map[string]string{"gc.auto": "0", "gc.autoPackLimit": "0", "maintenance.auto": "false"}
	for k, v := range want {
		if got := gitConfig(r.Checkout, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	// A fresh gc.log is left; the next pass is not due (it just ran), and it
	// still turns auto-gc back off.
	if err := os.WriteFile(gcLog, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktreetest.Git(t, r.Checkout, "config", "gc.auto", "6700")
	report, err = a.MaintainJobRepository(context.Background(), workspaceAgent)
	if err != nil {
		t.Fatalf("second pass: %v\n%s", err, report)
	}
	if !strings.HasPrefix(report, "not due") {
		t.Fatalf("a pass right after one ran = %q, want not due", report)
	}
	if b, err := os.ReadFile(gcLog); err != nil || string(b) != "fresh\n" {
		t.Fatalf("a fresh gc.log was touched: %q, %v", b, err)
	}
	if got := gitConfig(r.Checkout, "gc.auto"); got != "0" {
		t.Fatalf("after a pass that was not due gc.auto = %q, want 0", got)
	}
}

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
