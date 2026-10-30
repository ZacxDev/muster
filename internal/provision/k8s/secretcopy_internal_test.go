package k8s

import (
	"bytes"
	"context"
	"errors"
	"log"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/provision"
)

// The tests for copying a key holder's Secret from muster's own namespace into
// the worker's (secretcopy.go).

// copySourceNS is the namespace the operator keeps the Secret in — no default
// and no real deployment uses it.
const copySourceNS = "ops-home"

// keyBytes is a recognisable private-key stand-in, so a test can assert it
// reaches no log line or error.
var keyBytes = []byte("fixture-deploy-key-7f3a91c2\n")

// copySpec is an internal spec with a key holder mounting key ssh-privatekey of
// Secret `secret`.
func copySpec(name, secret string) provision.Spec {
	s := internalSpec(name)
	s.Security.RunAsUser = 1000
	s.KeyHolder = &provision.KeyHolder{
		Name: "deploy-key", RunAsUser: 1001, Command: []string{"sh", "-c", "sleep 1"},
		SecretMounts: []provision.SecretMount{{Secret: secret, Key: "ssh-privatekey", Path: "/etc/muster/deploy-key/ssh-privatekey"}},
		SharedDir:    "/var/lib/muster-mirror",
	}
	return s
}

// copyDriver is a per-instance driver (the layout a worker runs in) copying
// from copySourceNS, logging into the returned buffer.
func copyDriver(t *testing.T) (*Driver, *fake.Clientset, *bytes.Buffer) {
	t.Helper()
	cs := fake.NewClientset()
	empty := ""
	var logs bytes.Buffer
	d, err := New(Config{Client: cs, NamespacePerInstance: true, NamespacePrefix: "muster-agent-",
		WorkspaceStorageClass: &empty, SecretSourceNamespace: copySourceNS, Logger: log.New(&logs, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return d, cs, &logs
}

func seedSource(t *testing.T, cs *fake.Clientset, name string, typ corev1.SecretType, data map[string][]byte) {
	t.Helper()
	if _, err := cs.CoreV1().Secrets(copySourceNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: copySourceNS,
			Labels:      map[string]string{"team": "ops"},
			Annotations: map[string]string{"note": "operator-made"}},
		Type: typ, Data: data,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestTheKeyHoldersSecretIsCopiedWithItsDataAndType: the worker's namespace gets
// a Secret under the source's name whose data map and type are IDENTICAL to the
// source's (two keys, so a copy of one key only fails), carrying muster's
// ownership labels and NONE of the source's labels or annotations. The pod
// mounts it by that name.
func TestTheKeyHoldersSecretIsCopiedWithItsDataAndType(t *testing.T) {
	d, cs, _ := copyDriver(t)
	ctx := context.Background()
	data := map[string][]byte{"ssh-privatekey": keyBytes, "ssh-publickey": []byte("ssh-ed25519 AAAA fixture")}
	seedSource(t, cs, "app-deploy-key", corev1.SecretTypeSSHAuth, data)
	if err := d.Create(ctx, copySpec("keyed", "app-deploy-key")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := cs.CoreV1().Secrets("muster-agent-keyed").Get(ctx, "app-deploy-key", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no copy in the worker's namespace: %v", err)
	}
	if !reflect.DeepEqual(got.Data, data) {
		t.Errorf("copied data differs from the source's (keys %v)", keysOf(got.Data))
	}
	if got.Type != corev1.SecretTypeSSHAuth {
		t.Errorf("type = %q, want %q", got.Type, corev1.SecretTypeSSHAuth)
	}
	if !owned(got.Labels) || got.Labels["team"] != "" || len(got.Annotations) != 0 {
		t.Errorf("labels %v annotations %v: want muster's ownership labels and nothing of the source's", got.Labels, got.Annotations)
	}
	dep, err := cs.AppsV1().Deployments("muster-agent-keyed").Get(ctx, "keyed", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		found = found || (v.Secret != nil && v.Secret.SecretName == "app-deploy-key")
	}
	if !found {
		t.Error("the pod mounts no volume of the copied Secret's name")
	}
	// The source is untouched.
	src, _ := cs.CoreV1().Secrets(copySourceNS).Get(ctx, "app-deploy-key", metav1.GetOptions{})
	if src.Labels["team"] != "ops" || owned(src.Labels) {
		t.Errorf("the source's labels changed: %v", src.Labels)
	}
}

func keysOf(m map[string][]byte) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAMissingSourceFailsProvisioningByName: no source Secret (and, separately,
// a source without the mounted key) fails Create with an error naming the Secret
// and its namespace, and NO Deployment is written — the pod that would wait in
// ContainerCreating forever does not exist.
func TestAMissingSourceFailsProvisioningByName(t *testing.T) {
	for _, c := range []struct {
		name string
		seed func(*testing.T, *fake.Clientset)
		want []string
	}{
		{"absent", func(*testing.T, *fake.Clientset) {}, []string{`"app-deploy-key"`, `"ops-home"`, "does not exist"}},
		{"no such key", func(t *testing.T, cs *fake.Clientset) {
			seedSource(t, cs, "app-deploy-key", corev1.SecretTypeOpaque, map[string][]byte{"other": []byte("x")})
		}, []string{`"app-deploy-key"`, `"ssh-privatekey"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, cs, _ := copyDriver(t)
			c.seed(t, cs)
			err := d.Create(context.Background(), copySpec("keyed", "app-deploy-key"))
			if err == nil {
				t.Fatal("Create succeeded without the source Secret")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %s", err, w)
				}
			}
			if errors.Is(err, provision.ErrBlind) {
				t.Errorf("a missing Secret reported as a transient ErrBlind: %v", err)
			}
			if _, e := cs.AppsV1().Deployments("muster-agent-keyed").Get(context.Background(), "keyed", metav1.GetOptions{}); !apierrors.IsNotFound(e) {
				t.Errorf("a Deployment was written (%v)", e)
			}
		})
	}
}

// TestAChangedSourcePropagatesOnUpdate: after the operator replaces the key, a
// re-apply (Update: Start / reapply) writes the new data over the copy; a key
// removed from the source is removed from the copy too.
func TestAChangedSourcePropagatesOnUpdate(t *testing.T) {
	d, cs, _ := copyDriver(t)
	ctx := context.Background()
	seedSource(t, cs, "app-deploy-key", corev1.SecretTypeSSHAuth, map[string][]byte{"ssh-privatekey": keyBytes, "old": []byte("o")})
	spec := copySpec("keyed", "app-deploy-key")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	src, _ := cs.CoreV1().Secrets(copySourceNS).Get(ctx, "app-deploy-key", metav1.GetOptions{})
	rotated := map[string][]byte{"ssh-privatekey": []byte("rotated-key-2b8e")}
	src.Data = rotated
	if _, err := cs.CoreV1().Secrets(copySourceNS).Update(ctx, src, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Update(ctx, spec); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := cs.CoreV1().Secrets("muster-agent-keyed").Get(ctx, "app-deploy-key", metav1.GetOptions{})
	if !reflect.DeepEqual(got.Data, rotated) {
		t.Errorf("after the source changed the copy holds keys %v, not the rotated data", keysOf(got.Data))
	}
}

// TestASpecWithoutAKeyHolderCopiesNothing: a spec with no key holder (every pool
// without a deploy key) makes no call on the source namespace and writes no
// Secret beyond its own; the control (a keyed spec on the same driver) does read
// it.
func TestASpecWithoutAKeyHolderCopiesNothing(t *testing.T) {
	d, cs, _ := copyDriver(t)
	ctx := context.Background()
	seedSource(t, cs, "app-deploy-key", corev1.SecretTypeSSHAuth, map[string][]byte{"ssh-privatekey": keyBytes})
	sourceCalls := func() int {
		n := 0
		for _, a := range cs.Actions() {
			if a.GetNamespace() == copySourceNS && a.GetVerb() != "create" {
				n++
			}
		}
		return n
	}
	if err := d.Create(ctx, internalSpec("plain")); err != nil {
		t.Fatal(err)
	}
	if n := sourceCalls(); n != 0 {
		t.Errorf("a spec without a key holder made %d call(s) on the source namespace", n)
	}
	if _, err := cs.CoreV1().Secrets("muster-agent-plain").Get(ctx, "app-deploy-key", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a copy exists for a spec without a key holder (%v)", err)
	}
	if err := d.Create(ctx, copySpec("keyed", "app-deploy-key")); err != nil {
		t.Fatal(err)
	}
	if sourceCalls() == 0 {
		t.Fatal("control: the keyed spec made no call on the source namespace either")
	}
}

// TestADriverWithoutASourceNamespaceCopiesNothing: unset, the Secret is the
// operator's to place in the worker's namespace (the earlier behaviour): no read
// of any other namespace, no Secret written under its name.
func TestADriverWithoutASourceNamespaceCopiesNothing(t *testing.T) {
	cs := fake.NewClientset()
	empty := ""
	d, err := New(Config{Client: cs, NamespacePerInstance: true, NamespacePrefix: "muster-agent-", WorkspaceStorageClass: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Create(context.Background(), copySpec("keyed", "app-deploy-key")); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "secrets" && a.GetNamespace() != "muster-agent-keyed" {
			t.Errorf("secret call %s in namespace %q", a.GetVerb(), a.GetNamespace())
		}
	}
	if _, err := cs.CoreV1().Secrets("muster-agent-keyed").Get(context.Background(), "app-deploy-key", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a Secret was written under the operator's name (%v)", err)
	}
}

// TestASharedNamespaceThatIsTheSourceCopiesOntoNothing: when the instance's
// namespace IS the source's, the Secret is already where the pod mounts it:
// nothing is written over it, its labels stay the operator's.
func TestASharedNamespaceThatIsTheSourceCopiesOntoNothing(t *testing.T) {
	cs := fake.NewClientset()
	empty := ""
	d, err := New(Config{Client: cs, Namespace: copySourceNS, WorkspaceStorageClass: &empty, SecretSourceNamespace: copySourceNS})
	if err != nil {
		t.Fatal(err)
	}
	seedSource(t, cs, "app-deploy-key", corev1.SecretTypeSSHAuth, map[string][]byte{"ssh-privatekey": keyBytes})
	if err := d.Create(context.Background(), copySpec("keyed", "app-deploy-key")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := cs.CoreV1().Secrets(copySourceNS).Get(context.Background(), "app-deploy-key", metav1.GetOptions{})
	if owned(got.Labels) || got.Labels["team"] != "ops" {
		t.Errorf("the operator's Secret was rewritten as muster's: %v", got.Labels)
	}
}

// TestTheKeyNeverReachesALogOrAnError: across a successful copy, a refused one
// (a stranger's co-named Secret) and a missing key, no log line and no error
// carries the key's bytes. The positive control: the copy does hold them.
func TestTheKeyNeverReachesALogOrAnError(t *testing.T) {
	d, cs, logs := copyDriver(t)
	ctx := context.Background()
	seedSource(t, cs, "app-deploy-key", corev1.SecretTypeSSHAuth, map[string][]byte{"ssh-privatekey": keyBytes})
	var errs []string
	if err := d.Create(ctx, copySpec("keyed", "app-deploy-key")); err != nil {
		errs = append(errs, err.Error())
	}
	cp, _ := cs.CoreV1().Secrets("muster-agent-keyed").Get(ctx, "app-deploy-key", metav1.GetOptions{})
	if !bytes.Equal(cp.Data["ssh-privatekey"], keyBytes) {
		t.Fatal("control: the copy does not hold the key, so its absence below proves nothing")
	}
	// A stranger's Secret under the name in another worker's namespace.
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "muster-agent-other",
		Labels: instanceLabels("other")}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Secrets("muster-agent-other").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "app-deploy-key", Namespace: "muster-agent-other", Labels: map[string]string{"by": "hand"}},
		Data: map[string][]byte{"ssh-privatekey": keyBytes}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err := d.Create(ctx, copySpec("other", "app-deploy-key"))
	if !errors.Is(err, provision.ErrNotManaged) {
		t.Fatalf("a hand-made co-named Secret: err = %v, want ErrNotManaged", err)
	}
	errs = append(errs, err.Error())
	if err := d.Destroy(ctx, provision.Ref{Name: "keyed"}); err != nil {
		errs = append(errs, err.Error())
	}
	needle := strings.TrimSpace(string(keyBytes))
	if strings.Contains(logs.String(), needle) {
		t.Errorf("the key's bytes reached the log: %q", logs.String())
	}
	for _, e := range errs {
		if strings.Contains(e, needle) {
			t.Errorf("the key's bytes reached an error: %q", e)
		}
	}
}
