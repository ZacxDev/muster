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

// The private-repository pool (agentspec.PoolDeployKeys), through the real
// scripts and real git: the key holder's (podagent.MirrorScript, exec'd into
// the "deploy-key" container) and the worktree script (exec'd into the
// agent's). GitHub's SSH endpoint is played by worktreetest.FakeSSH: a `ssh`
// first on PATH that logs its argv and serves the fixture's origin to
// git-upload-pack, and refuses git-receive-pack as GitHub refuses a read-only
// deploy key.

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

func prepareAs(a *Adapter, r worktreetest.Repo, ag agents.Agent) (string, error) {
	a.anonGitBase = r.AnonBase
	return a.PrepareJobWorktree(context.Background(), ag, overlayJobID, worktreetest.RepoName, worktreetest.PR, r.Commit, "", "")
}

// lastArgs is the most recent exec's positional arguments after the script
// ($0 onwards).
func lastArgs(t *testing.T, d *localExecDriver) []string {
	t.Helper()
	return execArgs(t, d, -1)
}

// execArgs is exec i's positional arguments after the script ($0 onwards); a
// negative i counts from the last.
func execArgs(t *testing.T, d *localExecDriver, i int) []string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if i < 0 {
		i += len(d.cmds)
	}
	if i < 0 || i >= len(d.cmds) {
		t.Fatalf("exec %d of %d", i, len(d.cmds))
	}
	return d.cmds[i][3:]
}

// execContainers is the container of each exec so far ("" = the agent's own).
func execContainers(d *localExecDriver) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.containers...)
}

// TestAPrivatePoolsJobFetchesThroughItsKeyHolder: a job on a worker of a pool
// with a deploy key is two execs. FIRST into the "deploy-key" container, the
// mirror script on ssh://git@ssh.github.com:443/<repo>.git with an ssh command
// naming ONLY that key and GitHub's pinned host keys: it clones the mirror (an
// upload-pack), probes the key (a receive-pack, refused) and fetches the pull
// request (an upload-pack). THEN into the agent's own container, the worktree
// script, whose fetch URL is the MIRROR's path — no ssh, no key — and which
// strips the repository's configuration (overlay 0). The worktree is the
// commit.
func TestAPrivatePoolsJobFetchesThroughItsKeyHolder(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	log := worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), false)

	if _, err := prepareAs(a, r, poolAgent(privatePool)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := execContainers(d); len(got) != 2 || got[0] != agentspec.DeployKeyHolderName || got[1] != "" {
		t.Fatalf("execs ran in %q, want the key holder's container then the agent's", got)
	}
	holder := execArgs(t, d, 0)
	if holder[1] != "ssh://git@ssh.github.com:443/acme/widgets.git" || holder[2] != agentspec.MirrorPath || holder[4] != "7" {
		t.Fatalf("holder arguments = %q, want the ssh URL, the mirror and the pull request", holder)
	}
	for _, want := range []string{"-F /dev/null", "-o IdentityAgent=none", "-o IdentitiesOnly=yes",
		"-o IdentityFile=" + agentspec.DeployKeyPath, "-o UserKnownHostsFile=" + agentspec.KnownHostsPath,
		"-o GlobalKnownHostsFile=/dev/null", "-o StrictHostKeyChecking=yes", "-o BatchMode=yes"} {
		if !strings.Contains(holder[3], want) {
			t.Errorf("ssh command %q lacks %q", holder[3], want)
		}
	}
	main := lastArgs(t, d)
	if main[4] != agentspec.MirrorPath || main[7] != agentspec.MirrorPath || main[8] != "0" {
		t.Fatalf("worktree script: fetch URL %q, mirror %q, overlay %q; want the mirror twice and 0", main[4], main[7], main[8])
	}
	for _, arg := range main {
		if strings.Contains(arg, agentspec.DeployKeyPath) || strings.Contains(arg, "ssh://") {
			t.Fatalf("the agent's container was handed %q", arg)
		}
	}
	wt := filepath.Join(d.root, ".muster-jobs", "41")
	if got := worktreetest.Git(t, wt, "rev-parse", "HEAD"); got != r.Commit {
		t.Fatalf("worktree HEAD = %s, want the pull request's head %s", got, r.Commit)
	}
	got := worktreetest.SSHLog(t, log)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 ||
		!strings.HasSuffix(lines[0], "git@ssh.github.com git-upload-pack '/acme/widgets.git'") ||
		!strings.HasSuffix(lines[1], "git@ssh.github.com git-receive-pack '/acme/widgets.git'") ||
		!strings.HasSuffix(lines[2], "git@ssh.github.com git-upload-pack '/acme/widgets.git'") {
		t.Fatalf("ssh ran %d time(s), want the mirror's clone, the write probe, then the fetch:\n%s", len(lines), got)
	}
	for _, l := range lines {
		if !strings.Contains(l, "-p 443") || !strings.Contains(l, "IdentityFile="+agentspec.DeployKeyPath) {
			t.Errorf("an ssh invocation without port 443 or the deploy key: %q", l)
		}
	}
	// A second job reuses the mirror: no clone, the probe and the fetch only.
	if err := a.RemoveJobWorktree(context.Background(), poolAgent(privatePool), overlayJobID); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(log, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAs(a, r, poolAgent(privatePool)); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if got := strings.Count(worktreetest.SSHLog(t, log), "\n"); got != 2 {
		t.Fatalf("the second job ran ssh %d time(s), want 2 (probe, fetch):\n%s", got, worktreetest.SSHLog(t, log))
	}
}

// TestAnUnconfiguredPoolFetchesAnonymously: on the same deployment, a worker
// of a pool with NO deploy key — and an agent in no pool — fetches from the
// anonymous URL with no mirror, ssh never runs, and no exec reaches a key
// holder. The control is the test above: the same fake on PATH does log a
// configured pool's fetch.
func TestAnUnconfiguredPoolFetchesAnonymously(t *testing.T) {
	for _, pool := range []string{"public-review", ""} {
		t.Run("pool "+pool, func(t *testing.T) {
			a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
			r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
			log := worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), true)
			if _, err := prepareAs(a, r, poolAgent(pool)); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			args := lastArgs(t, d)
			if args[4] != r.AnonBase+worktreetest.RepoName || args[7] != "" || args[8] != "1" {
				t.Fatalf("fetch URL %q, mirror %q, overlay %q; want the anonymous %q, none, 1", args[4], args[7], args[8], r.AnonBase+worktreetest.RepoName)
			}
			if got := execContainers(d); len(got) != 1 || got[0] != "" {
				t.Fatalf("execs ran in %q, want the agent's container only", got)
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
// (the fake serves receive-pack), the job is refused by the holder before its
// fetch: the worktree script never runs, no worktree is made, the mirror holds
// no pull-request ref, and the probe wrote nothing to the origin. With the
// read-only fake the same job succeeds (the first test), so it is the probe
// that refuses.
func TestADeployKeyThatCanPushIsRefused(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), true)
	_, err := prepareAs(a, r, poolAgent(privatePool))
	if err == nil || !strings.Contains(err.Error(), DeployKeyWritableMessage) {
		t.Fatalf("prepare = %v, want an error carrying %q", err, DeployKeyWritableMessage)
	}
	if got := execContainers(d); len(got) != 1 || got[0] != agentspec.DeployKeyHolderName {
		t.Fatalf("execs ran in %q, want the key holder's only", got)
	}
	if _, err := os.Lstat(filepath.Join(d.root, ".muster-jobs", "41")); !os.IsNotExist(err) {
		t.Fatalf("a worktree was created (lstat: %v)", err)
	}
	mirror := filepath.Join(d.root, ".mirror", "repo.git")
	if refs := worktreetest.Git(t, mirror, "for-each-ref", "refs/pull/"); refs != "" {
		t.Fatalf("the pull request was fetched with a key that can push: %s", refs)
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
	_, err := a.PrepareJobWorktree(context.Background(), poolAgent(privatePool), overlayJobID, worktreetest.RepoName, 7, sha, "", "")
	if err == nil || !strings.Contains(err.Error(), DeployKeyRepoMismatchMessage) || !strings.Contains(err.Error(), "acme/other") {
		t.Fatalf("prepare = %v, want an error carrying %q naming the key's repository", err, DeployKeyRepoMismatchMessage)
	}
	if d.count() != 0 {
		t.Fatalf("a refused job ran %d exec(s)", d.count())
	}
	// Control: the same job with the key configured for its repository reaches
	// the key holder (which then fails: the fake serves no such repository).
	b, db := newDeployKeyAdapter(t, "ACME/Widgets")
	worktreetest.FakeSSH(t, os.Getenv("PATH"), t.TempDir(), false)
	_, _ = b.PrepareJobWorktree(context.Background(), poolAgent(privatePool), overlayJobID, worktreetest.RepoName, 7, sha, "", "")
	if got := execContainers(db); len(got) != 1 || got[0] != agentspec.DeployKeyHolderName {
		t.Fatalf("control: the matching repository (case-insensitively) ran execs in %q, want the key holder's", got)
	}
}

// TestAConfigRefOnADeployKeyPoolIsRefusedBeforeAnyExec: a deploy-key pool's
// jobs get no repository configuration, so a config_ref — which names one to
// overlay — is refused, before any exec, rather than silently ignored. (The
// job API refuses it at submission; this holds when the configuration moved
// since.) Control: the same config_ref on an agent outside the pool reaches the
// instance.
func TestAConfigRefOnADeployKeyPoolIsRefusedBeforeAnyExec(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	sha, cfg := strings.Repeat("d", 40), strings.Repeat("c", 40)
	_, err := a.PrepareJobWorktree(context.Background(), poolAgent(privatePool), overlayJobID, worktreetest.RepoName, 7, sha, cfg, "")
	if err == nil || !strings.Contains(err.Error(), ConfigRefStrippedMessage) {
		t.Fatalf("prepare = %v, want an error carrying %q", err, ConfigRefStrippedMessage)
	}
	if d.count() != 0 {
		t.Fatalf("the refusal ran %d exec(s)", d.count())
	}
	_, _ = a.PrepareJobWorktree(context.Background(), poolAgent("public-review"), overlayJobID, worktreetest.RepoName, 7, sha, cfg, "")
	if d.count() != 1 {
		t.Fatalf("control: a config_ref outside the pool ran %d exec(s), want 1", d.count())
	}
}
