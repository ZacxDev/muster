package podagent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
)

// TestAMirrorCheckoutClonesFromTheMirror: with a mirror, the checkout is
// cloned from that local path — the anonymous https route, pointed at a
// DIFFERENT repository's bytes here, is not used, and no ssh runs (this
// process holds no key). The control is the same repository without a mirror:
// it clones through https.
func TestAMirrorCheckoutClonesFromTheMirror(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "o/r", map[string]string{"README": "anonymous\n"})
	mirrors := t.TempDir()
	publish(t, mirrors, "m/repo", map[string]string{"README": "from the mirror\n"})
	mirror := filepath.Join(mirrors, "m", "repo.git")
	log := worktreetest.FakeSSH(t, os.Getenv("PATH"), gh, false)

	clone := func(mirror string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "work")
		c, err := NewCheckout(CheckoutConfig{Repo: "o/r", Dir: dir, Mirror: mirror,
			Voice: Voice{Who: "fixd", Agent: "the fixture agent", AgentOf: "its fixture agent"}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Attempt(ctx); err != nil {
			t.Fatalf("clone (mirror %q): %v", mirror, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "README"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if got := clone(mirror); got != "from the mirror\n" {
		t.Fatalf("a mirror checkout holds %q, want the mirror's bytes", got)
	}
	if got := clone(""); got != "anonymous\n" {
		t.Fatalf("control: a checkout without a mirror holds %q, want the https route's bytes", got)
	}
	if l := worktreetest.SSHLog(t, log); l != "" {
		t.Fatalf("ssh ran in the agent's checkout:\n%s", l)
	}
}

// TestAMirrorPathMustBeCleanAndAbsolute: a relative or unclean mirror path is
// refused; the control is accepted.
func TestAMirrorPathMustBeCleanAndAbsolute(t *testing.T) {
	for _, m := range []string{"relative/repo.git", "/var/lib/../lib/m/repo.git", "/var/lib/m/repo.git/"} {
		if _, err := NewCheckout(CheckoutConfig{Repo: "o/r", Dir: "/w", Mirror: m}); err == nil {
			t.Errorf("mirror %q: accepted", m)
		}
	}
	if _, err := NewCheckout(CheckoutConfig{Repo: "o/r", Dir: "/w", Mirror: "/var/lib/muster-mirror/repo.git"}); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// runMirrorScript runs MirrorScript as the key holder would: $1 the SSH URL,
// $2 the mirror, $3 the ssh command for key and hosts, $4 a pull request ("" =
// only the mirror), $5 a base.
func runMirrorScript(t *testing.T, mirror, key, hosts, pr, base string) (string, error) {
	t.Helper()
	ssh, err := DeployKeySSHCommand(key, hosts)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := exec.Command("sh", "-c", MirrorScript, "muster-mirror", DeployKeyURL("o/r"), mirror, ssh, pr, base)
	c.Stdout, c.Stderr = &out, &out
	err = c.Run()
	return out.String(), err
}

// TestTheHolderMakesTheMirrorOnceAndCarriesTheKeyOnlyByPath: the holder's
// script, with no pull request, clones a bare mirror over ssh (one
// upload-pack), configured to serve any object by id; run again it does not
// clone again (ssh does not run). And the key reaches no process's argv or
// environment — ssh, which inherits everything the script exports, is handed
// its PATH (the fake logs its argv and environment): an INVARIANT guard, since
// muster never holds the key's bytes; its control is that the path IS there.
func TestTheHolderMakesTheMirrorOnceAndCarriesTheKeyOnlyByPath(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "o/r", map[string]string{"README": "private\n"})
	log := worktreetest.FakeSSH(t, os.Getenv("PATH"), gh, false)
	envLog := filepath.Join(t.TempDir(), "ssh.env")
	t.Setenv(worktreetest.FakeSSHEnvLogEnv, envLog)
	keyDir := t.TempDir()
	const keyBytes = "-----BEGIN OPENSSH PRIVATE KEY-----\nfixture-deploy-key-bytes-5c1e\n-----END OPENSSH PRIVATE KEY-----\n"
	key, hosts := filepath.Join(keyDir, "ssh-privatekey"), filepath.Join(keyDir, "known_hosts")
	if err := os.WriteFile(key, []byte(keyBytes), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hosts, []byte(GitHubKnownHosts), 0o444); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(t.TempDir(), "shared", "repo.git")

	if out, err := runMirrorScript(t, mirror, key, hosts, "", ""); err != nil {
		t.Fatalf("mirror: %v: %s", err, out)
	}
	if got := gitT(t, mirror, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Fatalf("the mirror is not bare: %q", got)
	}
	if got := gitT(t, mirror, "config", "uploadpack.allowAnySHA1InWant"); got != "true" {
		t.Fatalf("uploadpack.allowAnySHA1InWant = %q", got)
	}
	l := worktreetest.SSHLog(t, log)
	if strings.Count(l, "git-upload-pack") != 1 || !strings.Contains(l, "IdentityFile="+key) {
		t.Fatalf("the mirror's clone: ssh log %q, want one upload-pack naming the key's path", l)
	}
	env, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"argv": l, "environment": string(env)} {
		if strings.Contains(got, "fixture-deploy-key-bytes-5c1e") || strings.Contains(got, "PRIVATE KEY") {
			t.Errorf("ssh's %s carries the key's bytes", name)
		}
	}
	if !strings.Contains(string(env), "IdentityFile="+key) {
		t.Fatalf("control: ssh's environment does not name the key's path (GIT_SSH_COMMAND): the check above saw nothing")
	}

	if err := os.Truncate(log, 0); err != nil {
		t.Fatal(err)
	}
	if out, err := runMirrorScript(t, mirror, key, hosts, "", ""); err != nil {
		t.Fatalf("again: %v: %s", err, out)
	}
	if l := worktreetest.SSHLog(t, log); l != "" {
		t.Fatalf("an existing mirror was cloned again:\n%s", l)
	}
	// No temporary clone is left beside the mirror (its lock file may be).
	entries, _ := os.ReadDir(filepath.Dir(mirror))
	for _, e := range entries {
		if e.Name() != "repo.git" && e.Name() != "repo.git.lock" {
			t.Errorf("the shared directory holds %q beside the mirror", e.Name())
		}
	}
}

// TestTheHolderCommandIsTheScriptInALoop: the holder's container runs
// MirrorScript itself (no copy that could drift), on the repository's SSH URL,
// the mirror and the ssh command for the mounted key.
func TestTheHolderCommandIsTheScriptInALoop(t *testing.T) {
	cmd, err := MirrorHolderCommand("o/r", "/var/lib/muster-mirror/repo.git", "/etc/k", "/etc/h")
	if err != nil {
		t.Fatal(err)
	}
	ssh, _ := DeployKeySSHCommand("/etc/k", "/etc/h")
	if len(cmd) != 8 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[4] != MirrorScript ||
		cmd[5] != "ssh://git@ssh.github.com:443/o/r.git" || cmd[6] != "/var/lib/muster-mirror/repo.git" || cmd[7] != ssh {
		t.Fatalf("holder command = %q", cmd)
	}
	if _, err := MirrorHolderCommand("o/r", "/m", "/k y", "/h"); err == nil {
		t.Fatal("a key path the ssh command would have to quote was accepted")
	}
}
