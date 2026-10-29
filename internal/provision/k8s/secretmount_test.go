package k8s_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// TestASecretMountMountsTheOperatorsSecretReadOnly: a SecretMount becomes a
// secret volume naming the OPERATOR's Secret and only its one key, mode 0440,
// mounted read-only at the path through that key's subPath on the instance's
// container — and the driver creates no Secret of that name (it is the
// operator's), nor carries the reference in any env. A spec without one has no
// such volume (the control), and the two specs' fingerprints differ.
func TestASecretMountMountsTheOperatorsSecretReadOnly(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		d, cs := newDriver(t, m, nil)
		ctx := context.Background()
		spec := provisiontest.MinimalSpec("keyed")
		spec.SecretMounts = []provision.SecretMount{{Secret: "private-app-deploy-key", Key: "ssh-privatekey", Path: "/etc/muster/deploy-key/ssh-privatekey"}}
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		ns := m.ns("keyed")
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "keyed", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pod := dep.Spec.Template.Spec
		found := 0
		for _, v := range pod.Volumes {
			if v.Secret == nil || v.Secret.SecretName != "private-app-deploy-key" {
				continue
			}
			found++
			if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "ssh-privatekey" || v.Secret.Items[0].Mode == nil || *v.Secret.Items[0].Mode != 0o440 {
				t.Fatalf("volume %s items = %+v, want only ssh-privatekey at mode 0440", v.Name, v.Secret.Items)
			}
			mounted := false
			for _, mt := range pod.Containers[0].VolumeMounts {
				if mt.Name == v.Name {
					mounted = true
					if mt.MountPath != "/etc/muster/deploy-key/ssh-privatekey" || mt.SubPath != "ssh-privatekey" || !mt.ReadOnly {
						t.Fatalf("mount = %+v, want read-only at the path through the key's subPath", mt)
					}
				}
			}
			if !mounted {
				t.Fatalf("volume %s is not mounted on the instance's container", v.Name)
			}
		}
		if found != 1 {
			t.Fatalf("%d volume(s) name the operator's Secret, want 1", found)
		}
		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "private-app-deploy-key", metav1.GetOptions{}); err == nil {
			t.Fatal("the driver created the operator's Secret")
		}
		for _, e := range pod.Containers[0].Env {
			if e.Value == "private-app-deploy-key" {
				t.Fatalf("env %s names the Secret", e.Name)
			}
		}

		plain := provisiontest.MinimalSpec("keyed")
		if provision.Fingerprint(plain) == provision.Fingerprint(spec) {
			t.Fatal("a spec with a secret mount fingerprints like one without: it would never roll onto the pod")
		}
	})
}

// TestASecretMountIsValidated: the path must be absolute and not another
// file's, the object a DNS subdomain, the key a Secret key. The control is
// accepted.
func TestASecretMountIsValidated(t *testing.T) {
	ok := provision.SecretMount{Secret: "deploy-key", Key: "ssh-privatekey", Path: "/etc/k"}
	for name, m := range map[string]provision.SecretMount{
		"relative path":  {Secret: ok.Secret, Key: ok.Key, Path: "etc/k"},
		"upper case obj": {Secret: "Deploy-Key", Key: ok.Key, Path: ok.Path},
		"slash in key":   {Secret: ok.Secret, Key: "a/b", Path: ok.Path},
		"empty key":      {Secret: ok.Secret, Path: ok.Path},
		"file's path":    {Secret: ok.Secret, Key: ok.Key, Path: "/etc/f"},
	} {
		spec := provisiontest.MinimalSpec("v")
		spec.Files = []provision.File{{Path: "/etc/f", Content: []byte("x")}}
		spec.SecretMounts = []provision.SecretMount{m}
		if err := spec.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	spec := provisiontest.MinimalSpec("v")
	spec.SecretMounts = []provision.SecretMount{ok}
	if err := spec.Validate(); err != nil {
		t.Fatalf("control: %v", err)
	}
	if err := provision.CheckSpec(provision.Capabilities{Files: true}, spec); err == nil {
		t.Fatal("a driver with no confidential store accepted a secret mount")
	}
}
