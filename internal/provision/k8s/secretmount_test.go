package k8s_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// keyHolderSpec is a minimal spec under uid 1000 with a key holder (uid 1001)
// that mounts the operator's Secret.
func keyHolderSpec(name string) provision.Spec {
	spec := provisiontest.MinimalSpec(name)
	spec.Security.RunAsUser = 1000
	spec.KeyHolder = &provision.KeyHolder{
		Name: "deploy-key", RunAsUser: 1001, Command: []string{"sh", "-c", "sleep 1"},
		Files:        []provision.File{{Path: "/etc/muster/deploy-key/known_hosts", Content: []byte("hosts\n")}},
		SecretMounts: []provision.SecretMount{{Secret: "private-app-deploy-key", Key: "ssh-privatekey", Path: "/etc/muster/deploy-key/ssh-privatekey"}},
		SharedDir:    "/var/lib/muster-mirror",
	}
	return spec
}

// TestAKeyHolderMountsTheOperatorsSecretInItsOwnContainer: a key holder's
// SecretMount becomes a secret volume naming the OPERATOR's Secret and only its
// one key, mode 0440, mounted read-only through that key's subPath on the
// HOLDER's container — never the instance's — which runs under its own uid;
// its file is mounted there too; the shared directory is an emptyDir mounted
// read-write in the holder and READ-ONLY in the instance's container. The
// driver creates no Secret of that name. A spec without a holder has none of
// it (the control), and the two specs' fingerprints differ.
func TestAKeyHolderMountsTheOperatorsSecretInItsOwnContainer(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		d, cs := newDriver(t, m, nil)
		ctx := context.Background()
		spec := keyHolderSpec("keyed")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		ns := m.ns("keyed")
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "keyed", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pod := dep.Spec.Template.Spec
		if len(pod.Containers) != 2 || pod.Containers[1].Name != "deploy-key" {
			t.Fatalf("containers = %d, want the instance's and the holder %q", len(pod.Containers), "deploy-key")
		}
		inst, holder := pod.Containers[0], pod.Containers[1]
		if sc := holder.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1001 || sc.RunAsGroup == nil || *sc.RunAsGroup != 1001 {
			t.Fatalf("holder security = %+v, want uid and gid 1001", sc)
		}
		if holder.Image != inst.Image || len(holder.Command) != 3 || holder.Command[2] != "sleep 1" {
			t.Fatalf("holder image %q command %q, want the instance's image and its own command", holder.Image, holder.Command)
		}
		secretVol, sharedVol := "", ""
		for _, v := range pod.Volumes {
			if v.Secret != nil && v.Secret.SecretName == "private-app-deploy-key" {
				if secretVol != "" {
					t.Fatal("two volumes name the operator's Secret")
				}
				secretVol = v.Name
				if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "ssh-privatekey" || v.Secret.Items[0].Mode == nil || *v.Secret.Items[0].Mode != 0o440 {
					t.Fatalf("volume %s items = %+v, want only ssh-privatekey at mode 0440", v.Name, v.Secret.Items)
				}
			}
			if v.EmptyDir != nil && v.Name != "workspace" {
				sharedVol = v.Name
			}
		}
		if secretVol == "" || sharedVol == "" {
			t.Fatalf("secret volume %q, shared volume %q: want both", secretVol, sharedVol)
		}
		// container -> shared mount path -> read-only
		mounted := map[string]map[string]bool{inst.Name: {}, holder.Name: {}}
		for _, c := range pod.Containers {
			for _, mt := range c.VolumeMounts {
				if mt.Name == secretVol {
					if c.Name != "deploy-key" || mt.MountPath != "/etc/muster/deploy-key/ssh-privatekey" || mt.SubPath != "ssh-privatekey" || !mt.ReadOnly {
						t.Fatalf("container %s mounts the secret: %+v; only the holder may, read-only through the key's subPath", c.Name, mt)
					}
				}
				if mt.Name == sharedVol {
					mounted[c.Name][mt.MountPath] = mt.ReadOnly
				}
				if mt.MountPath == "/etc/muster/deploy-key/known_hosts" && c.Name != "deploy-key" {
					t.Fatalf("container %s mounts the holder's file", c.Name)
				}
			}
		}
		if ro, ok := mounted[holder.Name]["/var/lib/muster-mirror"]; !ok || ro {
			t.Fatalf("holder's shared mount: present %t read-only %t, want read-write", ok, ro)
		}
		if ro, ok := mounted[inst.Name]["/var/lib/muster-mirror"]; !ok || !ro {
			t.Fatalf("instance's shared mount: present %t read-only %t, want read-only", ok, ro)
		}
		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "private-app-deploy-key", metav1.GetOptions{}); err == nil {
			t.Fatal("the driver created the operator's Secret")
		}

		plain := provisiontest.MinimalSpec("keyed")
		plain.Security.RunAsUser = 1000
		if provision.Fingerprint(plain) == provision.Fingerprint(spec) {
			t.Fatal("a spec with a key holder fingerprints like one without: it would never roll onto the pod")
		}
	})
}

// TestAKeyHolderIsValidated: the holder's uid must be non-zero and not the
// instance's (which must be declared), it must hold a secret, run a command
// and share a clean absolute directory outside the workspace; each secret's
// path must be absolute and not another file's, its object a DNS subdomain,
// its key a Secret key. The control is accepted, and a driver without the
// capability refuses it.
func TestAKeyHolderIsValidated(t *testing.T) {
	for name, mut := range map[string]func(*provision.Spec){
		"instance uid undeclared":  func(s *provision.Spec) { s.Security.RunAsUser = 0 },
		"holder uid zero":          func(s *provision.Spec) { s.KeyHolder.RunAsUser = 0 },
		"holder uid the session's": func(s *provision.Spec) { s.KeyHolder.RunAsUser = 1000 },
		"no command":               func(s *provision.Spec) { s.KeyHolder.Command = nil },
		"no secret":                func(s *provision.Spec) { s.KeyHolder.SecretMounts = nil },
		"relative shared dir":      func(s *provision.Spec) { s.KeyHolder.SharedDir = "var/lib/m" },
		"shared dir in workspace":  func(s *provision.Spec) { s.Workspace.Path = "/data"; s.KeyHolder.SharedDir = "/data/m" },
		"bad holder name":          func(s *provision.Spec) { s.KeyHolder.Name = "Deploy_Key" },
		"relative path":            func(s *provision.Spec) { s.KeyHolder.SecretMounts[0].Path = "etc/k" },
		"upper case obj":           func(s *provision.Spec) { s.KeyHolder.SecretMounts[0].Secret = "Deploy-Key" },
		"slash in key":             func(s *provision.Spec) { s.KeyHolder.SecretMounts[0].Key = "a/b" },
		"empty key":                func(s *provision.Spec) { s.KeyHolder.SecretMounts[0].Key = "" },
		"an instance file's path": func(s *provision.Spec) {
			s.Files = []provision.File{{Path: "/etc/muster/deploy-key/ssh-privatekey", Content: []byte("x")}}
		},
	} {
		spec := keyHolderSpec("v")
		mut(&spec)
		if err := spec.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	spec := keyHolderSpec("v")
	if err := spec.Validate(); err != nil {
		t.Fatalf("control: %v", err)
	}
	if err := provision.CheckSpec(provision.Capabilities{Files: true, Secrets: true}, spec); err == nil {
		t.Fatal("a driver that cannot run a key holder accepted one")
	}
}
