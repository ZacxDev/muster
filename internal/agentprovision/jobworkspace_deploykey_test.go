package agentprovision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// The private-repository pool (agentspec.PoolDeployKeys) and the job's
// repo_config_overlay switch, through the real worktree script and real git.
// GitHub's SSH endpoint is played by worktreetest.FakeSSH: a `ssh` first on
// PATH that logs its argv and serves the fixture's origin to git-upload-pack,
// and refuses git-receive-pack as GitHub refuses a read-only deploy key.

const privatePool = "private-review"

// poolAgent is workspaceAgent as a member of pool.
func poolAgent(pool string) agents.Agent {
	a := workspaceAgent
	a.WorkerPool = pool
	return a
}

// newDeployKeyAdapter is newWorkspaceAdapter with privatePool's deploy key
// configured for keyRepo.
func newDeployKeyAdapter(t *testing.T, keyRepo string) (*Adapter, *localExecDriver) {
	t.Helper()
	d := &localExecDriver{Noop: provision.MustNewNoop(), root: t.TempDir()}
	spec := fixtureSpecConfig()
	spec.ClaudeCode = &agentspec.ClaudeCodeConfig{PoolDeployKeys: map[string]agentspec.PoolDeployKey{
		privatePool: {Repo: keyRepo, Secret: "private-review-deploy-key"},
	}}
	a, err := New(Config{Driver: d, Store: &recordingStore{agent: fixtureAgent()}, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return a, d
}

func prepareAs(a *Adapter, r worktreetest.Repo, ag agents.Agent, skipRepoConfig bool) (string, error) {
	a.anonGitBase = r.AnonBase
	return a.PrepareJobWorktree(context.Background(), ag, overlayJobID, worktreetest.RepoName, worktreetest.PR, r.Commit, "", skipRepoConfig)
}

// lastArgs is the most recent exec's positional arguments after the script
// ($0 onwards).
func lastArgs(t *testing.T, d *localExecDriver) []string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.cmds) == 0 {
		t.Fatal("no exec reached the instance")
	}
	return d.cmds[len(d.cmds)-1][3:]
}

// TestAPrivatePoolsJobFetchesOverSSHWithItsDeployKey: a job on a worker of a
// pool with a deploy key fetches the pull request from
// ssh://git@ssh.github.com:443/<repo>.git with an ssh command naming ONLY that
// key and GitHub's pinned host keys; the worktree is the commit, overlaid as
// usual. The fake's log shows the fetch went through ssh (upload-pack of the
// repository) after the write probe (receive-pack, refused).
func TestAPrivatePoolsJobFetchesOverSSHWithItsDeployKey(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	log := worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), false)

	if _, err := prepareAs(a, r, poolAgent(privatePool), false); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	args := lastArgs(t, d)
	if got, want := args[4], "ssh://git@ssh.github.com:443/acme/widgets.git"; got != want {
		t.Fatalf("fetch URL = %q, want %q", got, want)
	}
	for _, want := range []string{"-F /dev/null", "-o IdentityAgent=none", "-o IdentitiesOnly=yes",
		"-o IdentityFile=" + agentspec.DeployKeyPath, "-o UserKnownHostsFile=" + agentspec.KnownHostsPath,
		"-o GlobalKnownHostsFile=/dev/null", "-o StrictHostKeyChecking=yes", "-o BatchMode=yes"} {
		if !strings.Contains(args[7], want) {
			t.Errorf("ssh command %q lacks %q", args[7], want)
		}
	}
	wt := filepath.Join(d.root, ".muster-jobs", "41")
	if got := worktreetest.Git(t, wt, "rev-parse", "HEAD"); got != r.Commit {
		t.Fatalf("worktree HEAD = %s, want the pull request's head %s", got, r.Commit)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "CLAUDE.md")); string(b) != "# default instructions\n" {
		t.Fatalf("CLAUDE.md = %q, want the default branch's (the overlay is unchanged)", b)
	}
	got := worktreetest.SSHLog(t, log)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 ||
		!strings.HasSuffix(lines[0], "git@ssh.github.com git-receive-pack '/acme/widgets.git'") ||
		!strings.HasSuffix(lines[1], "git@ssh.github.com git-upload-pack '/acme/widgets.git'") {
		t.Fatalf("ssh ran %d time(s), want the write probe then the fetch:\n%s", len(lines), got)
	}
	for _, l := range lines {
		if !strings.Contains(l, "-p 443") || !strings.Contains(l, "IdentityFile="+agentspec.DeployKeyPath) {
			t.Errorf("an ssh invocation without port 443 or the deploy key: %q", l)
		}
	}
}

// TestAnUnconfiguredPoolFetchesAnonymously: on the same deployment, a worker
// of a pool with NO deploy key — and an agent in no pool — fetches from the
// anonymous URL with no ssh command, and ssh never runs. The control is the
// test above: the same fake on PATH does log a configured pool's fetch.
func TestAnUnconfiguredPoolFetchesAnonymously(t *testing.T) {
	for _, pool := range []string{"public-review", ""} {
		t.Run("pool "+pool, func(t *testing.T) {
			a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
			r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
			log := worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), true)
			if _, err := prepareAs(a, r, poolAgent(pool), false); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			args := lastArgs(t, d)
			if args[4] != r.AnonBase+worktreetest.RepoName || args[7] != "" {
				t.Fatalf("fetch URL %q, ssh command %q; want the anonymous %q and none", args[4], args[7], r.AnonBase+worktreetest.RepoName)
			}
			if got := worktreetest.SSHLog(t, log); got != "" {
				t.Fatalf("ssh ran for an anonymous fetch:\n%s", got)
			}
			if got := worktreetest.Git(t, filepath.Join(d.root, ".muster-jobs", "41"), "rev-parse", "HEAD"); got != r.Commit {
				t.Fatalf("worktree HEAD = %s, want %s", got, r.Commit)
			}
		})
	}
}

// TestADeployKeyThatCanPushIsRefused: when GitHub accepts the dry-run push
// (the fake serves receive-pack), the job is refused before its fetch, and no
// worktree is made. With the read-only fake the same job succeeds (the first
// test), so it is the probe that refuses.
func TestADeployKeyThatCanPushIsRefused(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	log := worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), true)
	_, err := prepareAs(a, r, poolAgent(privatePool), false)
	if err == nil || !strings.Contains(err.Error(), DeployKeyWritableMessage) {
		t.Fatalf("prepare = %v, want an error carrying %q", err, DeployKeyWritableMessage)
	}
	if _, err := os.Lstat(filepath.Join(d.root, ".muster-jobs", "41")); !os.IsNotExist(err) {
		t.Fatalf("a worktree was created (lstat: %v)", err)
	}
	if got := worktreetest.SSHLog(t, log); strings.Contains(got, "git-upload-pack") {
		t.Fatalf("the pull request was fetched with a key that can push:\n%s", got)
	}
	if refs := worktreetest.Git(t, r.Origin, "for-each-ref", "refs/heads/muster-read-only-probe"); refs != "" {
		t.Fatalf("the dry-run probe wrote to the origin: %s", refs)
	}
}

// TestAJobOnAnotherRepositoryThanThePoolsDeployKeyIsRefused: the pool's key is
// for another repository, so the job is refused before any exec — not fetched
// anonymously instead.
func TestAJobOnAnotherRepositoryThanThePoolsDeployKeyIsRefused(t *testing.T) {
	a, d := newDeployKeyAdapter(t, "acme/other")
	sha := strings.Repeat("d", 40)
	_, err := a.PrepareJobWorktree(context.Background(), poolAgent(privatePool), overlayJobID, worktreetest.RepoName, 7, sha, "", false)
	if err == nil || !strings.Contains(err.Error(), DeployKeyRepoMismatchMessage) || !strings.Contains(err.Error(), "acme/other") {
		t.Fatalf("prepare = %v, want an error carrying %q naming the key's repository", err, DeployKeyRepoMismatchMessage)
	}
	if d.count() != 0 {
		t.Fatalf("a refused job ran %d exec(s)", d.count())
	}
	// Control: the same job with the key configured for its repository reaches
	// the instance.
	b, db := newDeployKeyAdapter(t, "ACME/Widgets")
	_, _ = b.PrepareJobWorktree(context.Background(), poolAgent(privatePool), overlayJobID, worktreetest.RepoName, 7, sha, "", false)
	if db.count() != 1 {
		t.Fatalf("control: the matching repository (case-insensitively) ran %d exec(s), want 1", db.count())
	}
}

// TestRepoConfigOverlayFalseRestoresNothing: with skipRepoConfig every
// agent-configuration path is ABSENT from the worktree — the commit's copies
// removed and none of the default branch's restored — while a file outside the
// overlay is the commit's. The control, in the same fixture, is the default:
// the default branch's copies are there.
func TestRepoConfigOverlayFalseRestoresNothing(t *testing.T) {
	a, d := newWorkspaceAdapter(t)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	wt := filepath.Join(d.root, ".muster-jobs", "41")

	if _, err := prepareAs(a, r, workspaceAgent, false); err != nil {
		t.Fatalf("control prepare: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(wt, ".claude", "settings.json")); err != nil || len(b) == 0 {
		t.Fatalf("control: the default overlay left no .claude/settings.json (%v)", err)
	}
	if err := a.RemoveJobWorktree(context.Background(), workspaceAgent, overlayJobID); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareAs(a, r, workspaceAgent, true); err != nil {
		t.Fatalf("prepare without the overlay: %v", err)
	}
	if args := lastArgs(t, d); args[8] != "0" {
		t.Fatalf("overlay argument = %q, want 0", args[8])
	}
	for _, p := range append(overlayPaths, ".claude", "sub/.claude", "deep/.claude") {
		if _, err := os.Lstat(filepath.Join(wt, p)); !os.IsNotExist(err) {
			t.Errorf("%s is in a worktree with repo_config_overlay false (lstat: %v)", p, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "f")); string(b) != "two\n" {
		t.Fatalf("f = %q, want the commit's", b)
	}
	if got := worktreetest.Git(t, wt, "rev-parse", "HEAD"); got != r.Commit {
		t.Fatalf("worktree HEAD = %s, want %s", got, r.Commit)
	}
	// A config ref names a configuration to overlay: refused with the switch
	// off, before any exec.
	before := d.count()
	if _, err := a.PrepareJobWorktree(context.Background(), workspaceAgent, overlayJobID, worktreetest.RepoName, 7, r.Commit, r.Default, true); err == nil {
		t.Fatal("a config ref with the overlay off was accepted")
	}
	if d.count() != before {
		t.Fatalf("the refusal ran %d exec(s)", d.count()-before)
	}
}
