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

// TestAPrivatePoolWorkerMountsItsDeployKey: a worker of a pool with a deploy
// key gets EXACTLY the Secret's ssh-privatekey mounted at the literal path,
// GitHub's pinned host keys as a plain file, and the two paths for ccd; the key
// is in no variable and no muster-written Secret.
func TestAPrivatePoolWorkerMountsItsDeployKey(t *testing.T) {
	spec := mustBuild(t, deployKeyAgent("private-review", "acme/private-app"), deployKeyConfig(), Options{})
	want := []provision.SecretMount{{Secret: "private-app-deploy-key", Key: "ssh-privatekey", Path: "/etc/muster/deploy-key/ssh-privatekey"}}
	if !reflect.DeepEqual(spec.SecretMounts, want) {
		t.Fatalf("secret mounts = %+v, want %+v", spec.SecretMounts, want)
	}
	if v, _ := envValue(spec, "CCD_REPO_DEPLOY_KEY"); v != "/etc/muster/deploy-key/ssh-privatekey" {
		t.Errorf("CCD_REPO_DEPLOY_KEY = %q", v)
	}
	if v, _ := envValue(spec, "CCD_REPO_KNOWN_HOSTS"); v != "/etc/muster/deploy-key/known_hosts" {
		t.Errorf("CCD_REPO_KNOWN_HOSTS = %q", v)
	}
	i := slices.IndexFunc(spec.Files, func(f provision.File) bool { return f.Path == "/etc/muster/deploy-key/known_hosts" })
	if i < 0 || string(spec.Files[i].Content) != podagent.GitHubKnownHosts || spec.Files[i].Secret {
		t.Fatalf("known_hosts file missing, not GitHub's pinned keys, or marked secret: %+v", spec.Files)
	}
	if !strings.HasPrefix(string(spec.Files[i].Content), "[ssh.github.com]:443 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n") {
		t.Fatalf("known_hosts does not start with GitHub's ed25519 key for ssh.github.com:443: %q", spec.Files[i].Content)
	}
	for _, e := range append(append([]provision.EnvVar(nil), spec.Env...), spec.Secrets...) {
		if strings.Contains(e.Value, "private-app-deploy-key") {
			t.Errorf("variable %s names the Secret: the pod needs only the mounted path", e.Name)
		}
	}
	if err := provision.CheckSpec(provision.MustNewNoop().Capabilities(), spec); err != nil {
		t.Fatalf("the spec does not pass CheckSpec: %v", err)
	}
}

// TestAWorkerOutsideADeployKeyPoolIsUnchanged: an unpooled agent, a worker of
// an unlisted pool, and a chief whose row names the pool get no mount, no file
// and no variable — the spec they had before deploy keys existed.
func TestAWorkerOutsideADeployKeyPoolIsUnchanged(t *testing.T) {
	chief := deployKeyAgent("private-review", "acme/private-app")
	chief.Name = agents.ChiefName
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
			if len(with.SecretMounts) != 0 {
				t.Fatalf("secret mounts = %+v", with.SecretMounts)
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
