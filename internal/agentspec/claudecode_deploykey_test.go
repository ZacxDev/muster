package agentspec

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/provision"
)

// deployKeyConfig is ccConfig with pool "private-review"'s deploy key for
// acme/private-app.
func deployKeyConfig() Config {
	cfg := ccConfig()
	cfg.ClaudeCode.PoolDeployKeys = map[string]PoolDeployKey{
		"private-review": {Repo: "acme/private-app", Secret: "private-app-deploy-key"},
	}
	return cfg
}

func deployKeyAgent(pool, repo string) agents.Agent {
	a := ccAgent()
	a.WorkerPool, a.Repo = pool, repo
	return a
}

// TestAPrivatePoolWorkerMountsItsDeployKeyInItsKeyHolder: a worker of a pool
// with a deploy key gets a KEY HOLDER — container "deploy-key", uid 1001 (not
// the session's 1000) — that alone mounts EXACTLY the Secret's ssh-privatekey
// at the literal path, has GitHub's pinned host keys as a plain file of its
// own, runs the mirror script on the pool's repository, and shares
// /var/lib/muster-mirror. The agent's own spec gets only the mirror's path;
// the key is in no variable, no instance file and no muster-written Secret.
func TestAPrivatePoolWorkerMountsItsDeployKeyInItsKeyHolder(t *testing.T) {
	spec := mustBuild(t, deployKeyAgent("private-review", "acme/private-app"), deployKeyConfig(), Options{})
	k := spec.KeyHolder
	if k == nil {
		t.Fatal("no key holder")
	}
	want := []provision.SecretMount{{Secret: "private-app-deploy-key", Key: "ssh-privatekey", Path: "/etc/muster/deploy-key/ssh-privatekey"}}
	if !reflect.DeepEqual(k.SecretMounts, want) {
		t.Fatalf("secret mounts = %+v, want %+v", k.SecretMounts, want)
	}
	if k.Name != "deploy-key" || k.RunAsUser != 1001 || k.SharedDir != "/var/lib/muster-mirror" {
		t.Fatalf("holder = %q uid %d shared %q, want deploy-key 1001 /var/lib/muster-mirror", k.Name, k.RunAsUser, k.SharedDir)
	}
	if k.RunAsUser == spec.Security.RunAsUser {
		t.Fatalf("the holder runs as the session's uid %d", k.RunAsUser)
	}
	cmd, err := podagent.MirrorHolderCommand("acme/private-app", "/var/lib/muster-mirror/repo.git",
		"/etc/muster/deploy-key/ssh-privatekey", "/etc/muster/deploy-key/known_hosts")
	if err != nil || !reflect.DeepEqual(k.Command, cmd) {
		t.Fatalf("holder command = %q (%v), want the mirror script on acme/private-app", k.Command, err)
	}
	if v, _ := envValue(spec, "CCD_REPO_MIRROR"); v != "/var/lib/muster-mirror/repo.git" {
		t.Errorf("CCD_REPO_MIRROR = %q", v)
	}
	i := slices.IndexFunc(k.Files, func(f provision.File) bool { return f.Path == "/etc/muster/deploy-key/known_hosts" })
	if i < 0 || string(k.Files[i].Content) != podagent.GitHubKnownHosts || k.Files[i].Secret {
		t.Fatalf("known_hosts file missing, not GitHub's pinned keys, or marked secret: %+v", k.Files)
	}
	if !strings.HasPrefix(string(k.Files[i].Content), "[ssh.github.com]:443 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n") {
		t.Fatalf("known_hosts does not start with GitHub's ed25519 key for ssh.github.com:443: %q", k.Files[i].Content)
	}
	if slices.ContainsFunc(spec.Files, func(f provision.File) bool { return strings.HasPrefix(f.Path, "/etc/muster/deploy-key/") }) {
		t.Fatalf("the agent's own files include the holder's: %+v", spec.Files)
	}
	for _, e := range append(append(append([]provision.EnvVar(nil), spec.Env...), spec.Secrets...), k.Env...) {
		if strings.Contains(e.Value, "private-app-deploy-key") {
			t.Errorf("variable %s names the Secret: the holder needs only the mounted path", e.Name)
		}
	}
	if err := provision.CheckSpec(provision.Capabilities{Secrets: true, Files: true, Persistence: true, ResourceLimits: true,
		NetworkIsolation: true, NetworkEgressByName: true, KeyHolder: true}, spec); err != nil {
		t.Fatalf("the spec does not pass CheckSpec: %v", err)
	}
	if err := provision.CheckSpec(provision.Capabilities{Secrets: true, Files: true, Persistence: true, ResourceLimits: true,
		NetworkIsolation: true, NetworkEgressByName: true}, spec); err == nil {
		t.Fatal("a driver that cannot run a key holder accepted the spec")
	}
}

// TestAWorkerOutsideADeployKeyPoolIsUnchanged: an unpooled agent, a worker of
// an unlisted pool, and a chief whose row names the pool get no key holder, no
// file and no variable — the spec they had before deploy keys existed — and
// keep the repository's agent configuration (StripsRepoConfig false; the
// pool's worker is the control).
func TestAWorkerOutsideADeployKeyPoolIsUnchanged(t *testing.T) {
	chief := deployKeyAgent("private-review", "acme/private-app")
	chief.Name = agents.ChiefName
	if !StripsRepoConfig(deployKeyAgent("private-review", "acme/private-app"), deployKeyConfig()) {
		t.Fatal("control: the deploy-key pool's worker is not stripped")
	}
	for name, a := range map[string]agents.Agent{
		"no pool":       deployKeyAgent("", "acme/private-app"),
		"unlisted pool": deployKeyAgent("public-review", "acme/private-app"),
		"chief":         chief,
	} {
		t.Run(name, func(t *testing.T) {
			with := mustBuild(t, a, deployKeyConfig(), Options{})
			without := mustBuild(t, a, ccConfig(), Options{})
			if !reflect.DeepEqual(with, without) {
				t.Fatalf("the deploy-key configuration changed a spec it does not apply to:\nwith    %+v\nwithout %+v", with, without)
			}
			if with.KeyHolder != nil {
				t.Fatalf("key holder = %+v", with.KeyHolder)
			}
			if StripsRepoConfig(a, deployKeyConfig()) {
				t.Fatal("its jobs would be stripped of the repository's configuration")
			}
		})
	}
}

// TestAPrivatePoolWorkerOnAnotherRepositoryIsRefused: the pool's key is for one
// repository; a worker of it on another is refused, naming both. The same
// repository in another case is the same GitHub repository and is accepted.
func TestAPrivatePoolWorkerOnAnotherRepositoryIsRefused(t *testing.T) {
	_, err := Build(deployKeyAgent("private-review", "acme/widgets"), deployKeyConfig(), Options{})
	if err == nil || !strings.Contains(err.Error(), `whose deploy key is for repository "acme/private-app", but it works on "acme/widgets"`) {
		t.Fatalf("build = %v, want a refusal naming both repositories", err)
	}
	if _, err := Build(deployKeyAgent("private-review", "Acme/Private-App"), deployKeyConfig(), Options{}); err != nil {
		t.Fatalf("control: the key's repository spelt in another case is refused: %v", err)
	}
}
