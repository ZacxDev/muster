package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
)

// 🔴 A PRIVATE-REPOSITORY POOL'S DEPLOY KEY IS UNREADABLE BY THE AGENT'S SESSION.
// These tests render a REAL deploy-key worker (agentspec builds it, this driver
// renders it) and pin the RELATIONSHIP, not one container: every container that
// mounts the operator's key Secret runs as a uid that is not the session's, the
// session's container (and the init container, which copies its mounts) mounts
// no key volume and carries no key path or reference in its environment, and no
// container shares process namespaces (which would hand the session the other
// container's /proc/<pid>/root and memory).

const deployKeySecret = "private-app-deploy-key"

// deployKeyPodSpec renders a worker of pool "private-review", whose deploy key
// (Secret deployKeySecret) is for acme/private-app.
func deployKeyPodSpec(t *testing.T) corev1.PodSpec {
	t.Helper()
	spec, err := agentspec.Build(agents.Agent{
		ID: 9, Name: "private-heron", Namespace: "agents", HooksToken: "fixture-hooks-token-k8s", GatewaySecret: "fixture-turn-credential-k8s",
		Kind: agents.KindClaudeCode, CCAccount: "work", WorkerPool: "private-review", Repo: "acme/private-app",
	}, agentspec.Config{
		ImageRepo: "registry.example.test/unused", APIBaseURL: "http://muster.example.test:8105",
		ClaudeCode: &agentspec.ClaudeCodeConfig{
			Image:    "ghcr.io/example-org/claude-code-agent:abc1234",
			Accounts: map[string]string{"work": fakeCCToken},
			PoolDeployKeys: map[string]agentspec.PoolDeployKey{
				"private-review": {Repo: "acme/private-app", Secret: deployKeySecret},
			},
		},
	}, agentspec.Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	d, cs := internalDriver(t)
	if err := d.Create(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments(internalNS).Get(context.Background(), "private-heron", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return dep.Spec.Template.Spec
}

// effectiveUID is the uid container c runs as: its own runAsUser, else the
// pod's; -1 when neither sets one (the image's, unknown here).
func effectiveUID(pod corev1.PodSpec, c corev1.Container) int64 {
	if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
		return *c.SecurityContext.RunAsUser
	}
	if pod.SecurityContext != nil && pod.SecurityContext.RunAsUser != nil {
		return *pod.SecurityContext.RunAsUser
	}
	return -1
}

func allContainers(pod corev1.PodSpec) []corev1.Container {
	return append(append([]corev1.Container(nil), pod.InitContainers...), pod.Containers...)
}

func sessionContainer(t *testing.T, pod corev1.PodSpec) corev1.Container {
	t.Helper()
	for _, c := range pod.Containers {
		if c.Name == containerName {
			return c
		}
	}
	t.Fatalf("no %q container in %+v", containerName, pod.Containers)
	return corev1.Container{}
}

// TestTheDeployKeyIsMountedOnlyIntoAContainerThatIsNotTheSessions: the key
// Secret's volume exists and is mounted by at least one container (the
// positive control: the key does reach the pod), and EVERY container mounting
// it is not the session's container, runs as a uid that is neither the
// session's nor root, as non-root, without privilege escalation.
func TestTheDeployKeyIsMountedOnlyIntoAContainerThatIsNotTheSessions(t *testing.T) {
	pod := deployKeyPodSpec(t)
	session := sessionContainer(t, pod)
	sessionUID := effectiveUID(pod, session)
	if sessionUID != agentspec.ClaudeCodeUID {
		t.Fatalf("the session container runs as uid %d, want %d", sessionUID, agentspec.ClaudeCodeUID)
	}
	keyVols := map[string]bool{}
	for _, v := range pod.Volumes {
		if v.Secret != nil && v.Secret.SecretName == deployKeySecret {
			keyVols[v.Name] = true
		}
	}
	if len(keyVols) == 0 {
		t.Fatalf("no volume names the operator's Secret %q: the key never reaches the pod", deployKeySecret)
	}
	holders := 0
	for _, c := range allContainers(pod) {
		mounts := false
		for _, m := range c.VolumeMounts {
			if keyVols[m.Name] {
				mounts = true
			}
		}
		if !mounts {
			continue
		}
		holders++
		uid := effectiveUID(pod, c)
		if c.Name == containerName || c.Name == initContainerName || uid == sessionUID || uid <= 0 {
			t.Errorf("container %q (uid %d) mounts the deploy key; only a container that is not the session's, "+
				"under a uid other than the session's %d and not root, may", c.Name, uid, sessionUID)
		}
		sc := c.SecurityContext
		if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("container %q holds the key without runAsNonRoot and allowPrivilegeEscalation=false: %+v", c.Name, sc)
		}
	}
	if holders == 0 {
		t.Fatal("no container mounts the deploy key's volume")
	}
}

// TestTheSessionContainerHasNoDeployKeyVolumeOrVariable: neither the session's
// container nor the init container (which runs as the session's uid and copies
// its mounts) mounts the key's volume, and no container's environment names the
// key's path, the variables that used to hand it to ccd, or the Secret — nor
// pulls any variable from that Secret (which would put the key's BYTES in
// /proc/<pid>/environ).
func TestTheSessionContainerHasNoDeployKeyVolumeOrVariable(t *testing.T) {
	pod := deployKeyPodSpec(t)
	keyVols := map[string]bool{}
	for _, v := range pod.Volumes {
		if v.Secret != nil && v.Secret.SecretName == deployKeySecret {
			keyVols[v.Name] = true
		}
	}
	for _, c := range allContainers(pod) {
		session := c.Name == containerName || c.Name == initContainerName
		for _, m := range c.VolumeMounts {
			if session && keyVols[m.Name] {
				t.Errorf("session container %q mounts the deploy key's volume %q at %s", c.Name, m.Name, m.MountPath)
			}
		}
		for _, e := range c.Env {
			if session && (e.Name == "CCD_REPO_DEPLOY_KEY" || strings.Contains(e.Value, "ssh-privatekey") || strings.Contains(e.Value, deployKeySecret)) {
				t.Errorf("session container %q: variable %s=%q points at the deploy key", c.Name, e.Name, e.Value)
			}
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == deployKeySecret {
				t.Errorf("container %q: variable %s is read from the key's Secret: its bytes would be in the process environment", c.Name, e.Name)
			}
		}
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == deployKeySecret {
				t.Errorf("container %q exports the key's Secret as variables", c.Name)
			}
		}
		for _, a := range append(append([]string(nil), c.Command...), c.Args...) {
			if session && strings.Contains(a, "ssh-privatekey") {
				t.Errorf("session container %q's command names the key: %q", c.Name, a)
			}
		}
	}
}

// TestTheSessionMountsTheHoldersMirrorReadOnly: the directory the key holder
// writes its mirror into is mounted into the session's container (ccd clones
// from it) and the init container READ-ONLY — so the session can plant nothing
// the holder's gits read — and read-write into the holder alone.
func TestTheSessionMountsTheHoldersMirrorReadOnly(t *testing.T) {
	pod := deployKeyPodSpec(t)
	seen := 0
	for _, c := range allContainers(pod) {
		for _, m := range c.VolumeMounts {
			if m.MountPath != agentspec.MirrorDir {
				continue
			}
			seen++
			holder := c.Name == agentspec.DeployKeyHolderName
			if holder == m.ReadOnly {
				t.Errorf("container %q mounts %s read-only=%t; want read-write in the holder only", c.Name, m.MountPath, m.ReadOnly)
			}
		}
	}
	if seen < 2 {
		t.Fatalf("%d container(s) mount %s, want the holder's and the session's", seen, agentspec.MirrorDir)
	}
}

// TestADeployKeyPodSharesNoProcessNamespace: with a shared process namespace
// (or the host's), the session could read the key holder's /proc/<pid>/root,
// environ and memory. Neither is set.
func TestADeployKeyPodSharesNoProcessNamespace(t *testing.T) {
	pod := deployKeyPodSpec(t)
	if pod.ShareProcessNamespace != nil && *pod.ShareProcessNamespace {
		t.Error("shareProcessNamespace is on: the session would see the key holder's processes")
	}
	if pod.HostPID || pod.HostIPC || pod.HostNetwork {
		t.Errorf("host namespaces: pid %t ipc %t network %t", pod.HostPID, pod.HostIPC, pod.HostNetwork)
	}
}
