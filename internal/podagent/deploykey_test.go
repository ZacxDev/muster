package podagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
)

// TestADeployKeyCheckoutClonesOverSSHWithTheKey: with a deploy key and its
// known_hosts, the checkout is cloned from ssh://git@ssh.github.com:443/<repo>.git
// through an ssh that names only that key and those host keys (the fake ssh's
// log), and the anonymous https route — still pointed at a DIFFERENT
// repository's bytes here — is not used. The control is the same repository
// without a key: it clones through https, and ssh never runs.
func TestADeployKeyCheckoutClonesOverSSHWithTheKey(t *testing.T) {
	gh := localGitHub(t)
	publish(t, gh, "o/r", map[string]string{"README": "over ssh\n"})
	// The https route serves other bytes, so a clone that went there is seen.
	anon := filepath.Join(t.TempDir(), "anon")
	publish(t, anon, "o/r", map[string]string{"README": "anonymous\n"})
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[url \"file://"+anon+"/\"]\n\tinsteadOf = https://github.com/\n[init]\n\tdefaultBranch = trunk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	log := worktreetest.FakeSSH(t, os.Getenv("PATH"), gh, false)

	clone := func(key, hosts string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "work")
		c, err := NewCheckout(CheckoutConfig{Repo: "o/r", Dir: dir, DeployKey: key, KnownHosts: hosts,
			Voice: Voice{Who: "fixd", Agent: "the fixture agent", AgentOf: "its fixture agent"}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Attempt(ctx); err != nil {
			t.Fatalf("clone (key %q): %v", key, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "README"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if got := clone("/etc/muster/deploy-key/ssh-privatekey", "/etc/muster/deploy-key/known_hosts"); got != "over ssh\n" {
		t.Fatalf("a deploy-key checkout holds %q, want the ssh route's bytes", got)
	}
	l := worktreetest.SSHLog(t, log)
	for _, want := range []string{"-F /dev/null", "-o IdentitiesOnly=yes", "-o IdentityFile=/etc/muster/deploy-key/ssh-privatekey",
		"-o UserKnownHostsFile=/etc/muster/deploy-key/known_hosts", "-o StrictHostKeyChecking=yes", "-p 443",
		"git@ssh.github.com git-upload-pack '/o/r.git'"} {
		if !strings.Contains(l, want) {
			t.Errorf("the clone's ssh argv lacks %q:\n%s", want, l)
		}
	}
	if err := os.Truncate(log, 0); err != nil {
		t.Fatal(err)
	}
	if got := clone("", ""); got != "anonymous\n" {
		t.Fatalf("control: a checkout without a key holds %q, want the https route's bytes", got)
	}
	if l := worktreetest.SSHLog(t, log); l != "" {
		t.Fatalf("control: ssh ran for an anonymous clone:\n%s", l)
	}
}

// TestADeployKeyNeedsItsKnownHostsAndASafePath: a key without its pinned host
// keys (or the reverse) is refused, and so is a path the ssh command would have
// to quote.
func TestADeployKeyNeedsItsKnownHostsAndASafePath(t *testing.T) {
	for _, c := range []struct{ key, hosts string }{
		{"/k", ""},
		{"", "/h"},
		{"/k y", "/h"},
		{"relative", "/h"},
		{"/k", "/h;x"},
		{"/k/../etc", "/h"},
	} {
		if _, err := NewCheckout(CheckoutConfig{Repo: "o/r", Dir: "/w", DeployKey: c.key, KnownHosts: c.hosts}); err == nil {
			t.Errorf("key %q, known_hosts %q: accepted", c.key, c.hosts)
		}
	}
	if _, err := NewCheckout(CheckoutConfig{Repo: "o/r", Dir: "/w", DeployKey: "/k", KnownHosts: "/h"}); err != nil {
		t.Fatalf("control: %v", err)
	}
}
