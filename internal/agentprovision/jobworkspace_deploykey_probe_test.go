package agentprovision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
)

// The deploy-key pool's write probe and fetches, against what the SESSION can
// change: the origin's refs (a branch at the probe's name) and the agent
// checkout's git config (which a lane's tools can write). Neither may make a
// key that can push read as read-only, nor break a read-only key's fetch.

const sshRepoURL = "ssh://git@ssh.github.com:443/acme/widgets.git"

// TestAProbeBranchAtAnUnrelatedCommitDoesNotHideAWritableKey: the origin has
// refs/heads/muster-read-only-probe at a commit with no history in common
// with the default branch. A probe pushed without force is refused as a
// non-fast-forward before GitHub is asked anything about write access, which
// read a writable key as read-only; with a forced refspec the dry run is
// accepted and the job refused.
func TestAProbeBranchAtAnUnrelatedCommitDoesNotHideAWritableKey(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	tree := worktreetest.Git(t, r.Origin, "rev-parse", r.Default+"^{tree}")
	orphan := worktreetest.Git(t, r.Origin, "commit-tree", tree, "-m", "someone else's probe branch")
	worktreetest.Git(t, r.Origin, "update-ref", "refs/heads/muster-read-only-probe", orphan)
	worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), true)

	_, err := prepareAs(a, r, poolAgent(privatePool))
	if err == nil || !strings.Contains(err.Error(), DeployKeyWritableMessage) {
		t.Fatalf("prepare = %v, want the writable-key refusal %q", err, DeployKeyWritableMessage)
	}
	if got := worktreetest.Git(t, r.Origin, "rev-parse", "refs/heads/muster-read-only-probe"); got != orphan {
		t.Fatalf("the dry-run probe moved the origin's branch to %s", got)
	}
}

// TestTheCheckoutsConfigCannotSteerTheWriteProbe: the session plants, in the
// agent checkout's own config, a push URL or a receive-pack program for the
// fetch URL. Each made the probe fail (exit 128) and so read a writable key as
// read-only. The probe must not read that config at all: with a key that can
// push, each plant still ends in the refusal.
func TestTheCheckoutsConfigCannotSteerTheWriteProbe(t *testing.T) {
	for name, kv := range map[string][2]string{
		"pushurl":     {"remote." + sshRepoURL + ".pushurl", "ssh://git@ssh.github.com:443/nobody/nothing.git"},
		"receivepack": {"remote." + sshRepoURL + ".receivepack", "no-such-receive-pack"},
	} {
		t.Run(name, func(t *testing.T) {
			a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
			r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
			worktreetest.Git(t, r.Checkout, "config", kv[0], kv[1])
			worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), true)
			_, err := prepareAs(a, r, poolAgent(privatePool))
			if err == nil || !strings.Contains(err.Error(), DeployKeyWritableMessage) {
				t.Fatalf("with %s planted: prepare = %v, want the writable-key refusal", kv[0], err)
			}
		})
	}
}

// TestTheCheckoutsConfigCannotBreakAReadOnlyKeysFetch: an upload-pack program
// planted for the key's fetch URL in the checkout's config does not reach the
// key's fetch (it once replaced the program ssh asked GitHub to run, failing
// every job): with a read-only key the job is made, at the commit.
func TestTheCheckoutsConfigCannotBreakAReadOnlyKeysFetch(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	worktreetest.Git(t, r.Checkout, "config", "remote."+sshRepoURL+".uploadpack", "no-such-upload-pack")
	log := worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), false)
	if _, err := prepareAs(a, r, poolAgent(privatePool)); err != nil {
		t.Fatalf("prepare: %v\nssh log:\n%s", err, worktreetest.SSHLog(t, log))
	}
	if got := worktreetest.Git(t, filepath.Join(d.root, ".muster-jobs", "41"), "rev-parse", "HEAD"); got != r.Commit {
		t.Fatalf("worktree HEAD = %s, want %s", got, r.Commit)
	}
	if strings.Contains(worktreetest.SSHLog(t, log), "no-such-upload-pack") {
		t.Fatal("the planted upload-pack program reached ssh")
	}
}

// TestADeployKeyPoolsJobGetsNoRepositoryConfiguration: a job on a deploy-key
// pool's worker — sent with NO flag at all — has none of the repository's agent
// configuration in its worktree: the commit's removed and the default branch's
// not restored. The server decides it from the pool, not the job. Control: the
// same job on a pool with no deploy key gets the default branch's overlay.
func TestADeployKeyPoolsJobGetsNoRepositoryConfiguration(t *testing.T) {
	a, d := newDeployKeyAdapter(t, worktreetest.RepoName)
	r := worktreetest.New(t, filepath.Join(d.root, "widgets"), filepath.Join(d.root, "markers"))
	worktreetest.FakeSSH(t, os.Getenv("PATH"), r.AnonRoot(), false)
	wt := filepath.Join(d.root, ".muster-jobs", "41")

	if _, err := prepareAs(a, r, poolAgent("public-review")); err != nil {
		t.Fatalf("control prepare: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "CLAUDE.md")); string(b) != "# default instructions\n" {
		t.Fatalf("control: CLAUDE.md = %q, want the default branch's", b)
	}
	if err := a.RemoveJobWorktree(t.Context(), poolAgent("public-review"), overlayJobID); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareAs(a, r, poolAgent(privatePool)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, p := range append(overlayPaths, ".claude", "sub/.claude", "deep/.claude") {
		if _, err := os.Lstat(filepath.Join(wt, p)); !os.IsNotExist(err) {
			t.Errorf("%s is in a deploy-key pool job's worktree (lstat: %v)", p, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "f")); string(b) != "two\n" {
		t.Fatalf("f = %q, want the commit's", b)
	}
}
