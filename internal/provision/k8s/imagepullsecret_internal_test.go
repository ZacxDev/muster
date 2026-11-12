package k8s

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/provision"
)

// The tests for the image pull Secret (provision.Spec.ImagePullSecret): the pod
// lists it in imagePullSecrets, and apply copies it from muster's namespace
// through the key holder's copy rule (secretcopy.go).

// pullSecretName is the operator's Secret name in these tests.
const pullSecretName = "officer-image-pull"

// dockerConfig is a recognisable credential stand-in, so a test can assert it
// reaches no error.
var dockerConfig = []byte(`{"auths":{"ghcr.io":{"auth":"fixture-pull-cred-5d1e"}}}`)

// pullSpec is an internal spec whose image is pulled with pullSecretName.
func pullSpec(name string) provision.Spec {
	s := internalSpec(name)
	s.Runtime.Image = "ghcr.io/example-org/officer-runner@sha256:" + strings.Repeat("a", 64)
	s.ImagePullSecret = pullSecretName
	return s
}

// TestThePodListsTheImagePullSecret: with the field set the pod spec carries
// exactly one imagePullSecrets entry naming it.
func TestThePodListsTheImagePullSecret(t *testing.T) {
	d, _, _ := copyDriver(t)
	dep, err := d.renderDeployment(pullSpec("pulled"), "muster-agent-pulled")
	if err != nil {
		t.Fatal(err)
	}
	want := []corev1.LocalObjectReference{{Name: pullSecretName}}
	if got := dep.Spec.Template.Spec.ImagePullSecrets; !reflect.DeepEqual(got, want) {
		t.Fatalf("imagePullSecrets = %v, want %v", got, want)
	}
}

// TestAPodWithoutOneListsNone is an INVARIANT GUARD (main already rendered no
// imagePullSecrets; it pins that the unset field adds nothing): unset, the pod
// spec has no imagePullSecrets at all, not an empty-named entry.
func TestAPodWithoutOneListsNone(t *testing.T) {
	d, _, _ := copyDriver(t)
	dep, err := d.renderDeployment(internalSpec("plain"), "muster-agent-plain")
	if err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Spec.ImagePullSecrets; got != nil {
		t.Fatalf("imagePullSecrets = %v, want none", got)
	}
}

// TestTheImagePullSecretMovesTheFingerprintOnlyWhenSet: a spec gaining one is a
// different pod template, so it must re-render; unset keeps the fingerprint.
func TestTheImagePullSecretMovesTheFingerprintOnlyWhenSet(t *testing.T) {
	plain := pullSpec("pulled")
	plain.ImagePullSecret = ""
	if provision.Fingerprint(plain) == provision.Fingerprint(pullSpec("pulled")) {
		t.Fatal("a spec gaining an image pull Secret kept its fingerprint: an existing instance would never roll to it")
	}
	other := pullSpec("pulled")
	other.ImagePullSecret = "another-pull"
	if provision.Fingerprint(other) == provision.Fingerprint(pullSpec("pulled")) {
		t.Fatal("two different pull Secrets share a fingerprint")
	}
}

// TestTheKubernetesDriverDeclaresTheCapability: without it CheckSpec would
// refuse every officer spec carrying the field on the one driver that renders it.
func TestTheKubernetesDriverDeclaresTheCapability(t *testing.T) {
	d, _, _ := copyDriver(t)
	if !d.Capabilities().ImagePullSecret {
		t.Fatal("the kubernetes driver does not declare Capabilities.ImagePullSecret")
	}
}

// TestTheImagePullSecretIsCopiedWithOnlyItsKeyAndType: the instance's namespace
// gets a Secret under the source's name holding ONLY .dockerconfigjson with the
// source's bytes, of type kubernetes.io/dockerconfigjson, under muster's labels
// and none of the source's; the pod names it; the source is untouched.
func TestTheImagePullSecretIsCopiedWithOnlyItsKeyAndType(t *testing.T) {
	d, cs, _ := copyDriver(t)
	ctx := context.Background()
	seedSource(t, cs, pullSecretName, corev1.SecretTypeDockerConfigJson,
		map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig, "extra": []byte("not-for-the-copy")})
	if err := d.Create(ctx, pullSpec("pulled")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := cs.CoreV1().Secrets("muster-agent-pulled").Get(ctx, pullSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no copy in the instance's namespace: %v", err)
	}
	if want := map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig}; !reflect.DeepEqual(got.Data, want) {
		t.Errorf("copied keys %v, want only %s with the source's bytes", keysOf(got.Data), corev1.DockerConfigJsonKey)
	}
	if got.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("type = %q, want %q", got.Type, corev1.SecretTypeDockerConfigJson)
	}
	if !owned(got.Labels) || got.Labels["team"] != "" || len(got.Annotations) != 0 {
		t.Errorf("labels %v annotations %v: want muster's ownership labels and nothing of the source's", got.Labels, got.Annotations)
	}
	dep, err := cs.AppsV1().Deployments("muster-agent-pulled").Get(ctx, "pulled", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []corev1.LocalObjectReference{{Name: pullSecretName}}; !reflect.DeepEqual(dep.Spec.Template.Spec.ImagePullSecrets, want) {
		t.Errorf("the applied pod's imagePullSecrets = %v, want %v", dep.Spec.Template.Spec.ImagePullSecrets, want)
	}
	src, _ := cs.CoreV1().Secrets(copySourceNS).Get(ctx, pullSecretName, metav1.GetOptions{})
	if src.Labels["team"] != "ops" || owned(src.Labels) || len(src.Data) != 2 {
		t.Errorf("the source changed: labels %v keys %v", src.Labels, keysOf(src.Data))
	}
}

// TestABadImagePullSourceFailsTheApplyByName: a missing source, one without
// .dockerconfigjson, and one of another type each fail Create with
// ErrInvalidSpec, naming the Secret and muster's namespace and the specific
// defect, never the credential's bytes; and NOTHING that runs is written (no
// Deployment, no ServiceAccount, no copy).
func TestABadImagePullSourceFailsTheApplyByName(t *testing.T) {
	for _, c := range []struct {
		name string
		seed func(*testing.T, *fake.Clientset)
		want []string
	}{
		{"absent", func(*testing.T, *fake.Clientset) {},
			[]string{`the instance's image is pulled with Secret "officer-image-pull", which does not exist in namespace "ops-home"`}},
		{"no .dockerconfigjson", func(t *testing.T, cs *fake.Clientset) {
			// The fake apiserver does not enforce a type's required key, so a
			// dockerconfigjson Secret without one can be seeded.
			seedSource(t, cs, pullSecretName, corev1.SecretTypeDockerConfigJson, map[string][]byte{".dockercfg": dockerConfig})
		}, []string{`Secret "officer-image-pull" in namespace "ops-home" has no key ".dockerconfigjson", which the instance's image is pulled with`}},
		{"wrong type", func(t *testing.T, cs *fake.Clientset) {
			seedSource(t, cs, pullSecretName, corev1.SecretTypeOpaque, map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig})
		}, []string{`Secret "officer-image-pull" in namespace "ops-home" is of type "Opaque"; an image pull Secret must be "kubernetes.io/dockerconfigjson"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, cs, logs := copyDriver(t)
			c.seed(t, cs)
			ctx := context.Background()
			err := d.Create(ctx, pullSpec("pulled"))
			if err == nil {
				t.Fatal("Create succeeded")
			}
			if !errors.Is(err, provision.ErrInvalidSpec) {
				t.Errorf("err = %v, want ErrInvalidSpec", err)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			needle := "fixture-pull-cred-5d1e"
			if strings.Contains(err.Error(), needle) || bytes.Contains(logs.Bytes(), []byte(needle)) {
				t.Errorf("the credential's bytes reached the error or the log")
			}
			ns := "muster-agent-pulled"
			if _, e := cs.AppsV1().Deployments(ns).Get(ctx, "pulled", metav1.GetOptions{}); !apierrors.IsNotFound(e) {
				t.Errorf("a Deployment was written (%v)", e)
			}
			if _, e := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "pulled", metav1.GetOptions{}); !apierrors.IsNotFound(e) {
				t.Errorf("a ServiceAccount was written (%v)", e)
			}
			if _, e := cs.CoreV1().Secrets(ns).Get(ctx, pullSecretName, metav1.GetOptions{}); !apierrors.IsNotFound(e) {
				t.Errorf("a copy was written (%v)", e)
			}
		})
	}
}

// TestASecretBothMountedAndPulledWithIsOneCopy: a key holder mounting key
// ssh-privatekey of the same Secret the image is pulled with gets ONE copy
// holding both keys (no other), of the source's type.
func TestASecretBothMountedAndPulledWithIsOneCopy(t *testing.T) {
	d, cs, _ := copyDriver(t)
	ctx := context.Background()
	seedSource(t, cs, pullSecretName, corev1.SecretTypeDockerConfigJson, map[string][]byte{
		corev1.DockerConfigJsonKey: dockerConfig, "ssh-privatekey": keyBytes, "spare": []byte("x")})
	spec := copySpec("both", pullSecretName)
	spec.ImagePullSecret = pullSecretName
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := cs.CoreV1().Secrets("muster-agent-both").Get(ctx, pullSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig, "ssh-privatekey": keyBytes}
	if !reflect.DeepEqual(got.Data, want) || got.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("copy keys %v type %q, want both mounted and pulled keys only, type %q", keysOf(got.Data), got.Type,
			corev1.SecretTypeDockerConfigJson)
	}
}
