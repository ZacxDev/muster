package k8s

// This file is IN-PACKAGE on purpose. It pins what the external
// ownership_test.go structurally cannot reach: the unexported notManagedError's
// own fields and its survival through errors.As out of every refusal site, and
// labelComplaint's exact output. The external file pins what a CALLER sees —
// the exported sentinels — and that is the right split: a guard on an
// unexported field says nothing about the contract, and a guard on the
// contract cannot see whether the type made it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/provision"
)

// newForeignDeployment is a Deployment somebody else made, with a replica count
// distinct from anything the driver renders so a rewrite would be visible.
func newForeignDeployment(name string) *appsv1.Deployment {
	replicas := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: internalForeignMeta(name),
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

const internalNS = "agents"

func internalDriver(t *testing.T) (*Driver, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	empty := ""
	d, err := New(Config{Client: cs, Namespace: internalNS, WorkspaceStorageClass: &empty})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, cs
}

func internalSpec(name string) provision.Spec {
	return provision.Spec{
		Ref:     provision.Ref{Name: name},
		Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"},
		Ports:   []provision.Port{{Name: provision.DefaultPortName, Port: 8421}},
	}
}

func internalForeignMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: internalNS, Labels: map[string]string{
		"app.kubernetes.io/name":       "grafana",
		"app.kubernetes.io/managed-by": "Helm",
	}}
}

// TestEveryWriteSiteRefusalKeepsItsTypeAndItsSentinels is the LEDGER of the six
// object kinds apply and Destroy can refuse on.
//
// 🔴 IT IS A SEAM GUARD, NOT A PER-SITE ONE. Each site was individually
// reviewed and each refusal was individually correct where it was CONSTRUCTED;
// what was broken was the wrapping between the construction and the caller.
// Five of the six sites wrapped the refusal in blind(), which formats with %v,
// so at 138d269 errors.As(err, **notManagedError) was FALSE on all five and
// notManagedError's own doc comment — "the concrete type survives errors.As" —
// was a false statement about five of its six producers. A table asserting all
// six together is what makes a sixth site added later visible: it either
// appears here or the ledger is short.
func TestEveryWriteSiteRefusalKeepsItsTypeAndItsSentinels(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		site string
		kind string
		seed func(*fake.Clientset)
		act  func(*Driver) error
	}{
		{
			site: "apply/serviceaccount",
			kind: "serviceaccount",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().ServiceAccounts(internalNS).Create(ctx, //nolint:errcheck // fake cannot fail here
					&corev1.ServiceAccount{ObjectMeta: internalForeignMeta("sa")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error { return d.Create(ctx, internalSpec("sa")) },
		},
		{
			site: "apply/configmap",
			kind: "configmap",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().ConfigMaps(internalNS).Create(ctx, //nolint:errcheck
					&corev1.ConfigMap{ObjectMeta: internalForeignMeta("cm-files")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("cm")
				s.Files = []provision.File{{Path: "/etc/muster/agent.json", Content: []byte("{}")}}
				return d.Create(ctx, s)
			},
		},
		{
			site: "apply/env secret",
			kind: "secret",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().Secrets(internalNS).Create(ctx, //nolint:errcheck
					&corev1.Secret{ObjectMeta: internalForeignMeta("es-env")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("es")
				s.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "v-51199"}}
				return d.Create(ctx, s)
			},
		},
		{
			site: "apply/file secret",
			kind: "secret",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().Secrets(internalNS).Create(ctx, //nolint:errcheck
					&corev1.Secret{ObjectMeta: internalForeignMeta("fs-secret-files")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("fs")
				s.Files = []provision.File{{Path: "/etc/muster/token", Content: []byte("v"), Secret: true}}
				return d.Create(ctx, s)
			},
		},
		{
			site: "apply/pvc",
			kind: "persistentvolumeclaim",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().PersistentVolumeClaims(internalNS).Create(ctx, //nolint:errcheck
					&corev1.PersistentVolumeClaim{ObjectMeta: internalForeignMeta("pv-workspace")},
					metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("pv")
				s.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
				return d.Create(ctx, s)
			},
		},
		{
			site: "apply/service",
			kind: "service",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().Services(internalNS).Create(ctx, //nolint:errcheck
					&corev1.Service{ObjectMeta: internalForeignMeta("sv")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error { return d.Create(ctx, internalSpec("sv")) },
		},
		{
			site: "apply/deployment (via Update)",
			kind: "deployment",
			seed: func(cs *fake.Clientset) {
				cs.AppsV1().Deployments(internalNS).Create(ctx, //nolint:errcheck
					newForeignDeployment("dp"), metav1.CreateOptions{})
			},
			act: func(d *Driver) error { return d.Update(ctx, internalSpec("dp")) },
		},
		{
			site: "Destroy/deployment",
			kind: "deployment",
			seed: func(cs *fake.Clientset) {
				cs.AppsV1().Deployments(internalNS).Create(ctx, //nolint:errcheck
					newForeignDeployment("dz"), metav1.CreateOptions{})
			},
			act: func(d *Driver) error { return d.Destroy(ctx, provision.Ref{Name: "dz"}) },
		},
	}

	for _, c := range cases {
		t.Run(c.site, func(t *testing.T) {
			d, cs := internalDriver(t)
			c.seed(cs)
			err := c.act(d)
			if err == nil {
				t.Fatalf("%s: no refusal at all", c.site)
			}

			var foreign *notManagedError
			if !errors.As(err, &foreign) {
				t.Fatalf("%s: the concrete *notManagedError did NOT survive to the caller. Its own doc "+
					"comment claims it does, and Update's pre-flight branch reads it with errors.As. "+
					"Got %v", c.site, err)
			}
			if foreign.kind != c.kind {
				t.Errorf("%s: the refusal names kind %q, want %q — a refusal naming the wrong object "+
					"sends an operator to relabel something that is not in the way",
					c.site, foreign.kind, c.kind)
			}
			if foreign.fromRead {
				t.Errorf("%s: this refusal answered a WRITE or a TEARDOWN, so fromRead must be false: "+
					"an ErrNotFound-flavoured refusal here is discarded by `!errors.Is(err, "+
					"provision.ErrNotFound)`", c.site)
			}
			if !errors.Is(err, provision.ErrNotManaged) {
				t.Errorf("%s: must report provision.ErrNotManaged, got %v", c.site, err)
			}
			if errors.Is(err, provision.ErrBlind) {
				t.Errorf("%s: must NOT report provision.ErrBlind — a permanent refusal arriving as a "+
					"transient fault is the defect this ledger exists for. Got %v", c.site, err)
			}
			if errors.Is(err, provision.ErrNotFound) {
				t.Errorf("%s: must NOT report provision.ErrNotFound, got %v", c.site, err)
			}
		})
	}

	// The NAMESPACE anchor needs the other layout, so it cannot join the table
	// above — but leaving it out would make notManagedError's "survives
	// errors.As on every path" claim wider than what is pinned. Its refusal
	// travels through two more %w wrappings than any other (fail -> firstErr ->
	// "destroy %q did not complete"), which is exactly where a %v would break
	// the chain again.
	t.Run("Destroy/namespace", func(t *testing.T) {
		cs := fake.NewClientset()
		empty := ""
		d, err := New(Config{Client: cs, NamespacePerInstance: true, WorkspaceStorageClass: &empty})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "muster-lonely", Labels: map[string]string{
				"app.kubernetes.io/name":       "grafana",
				"app.kubernetes.io/managed-by": "Helm",
			}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed namespace: %v", err)
		}
		derr := d.Destroy(ctx, provision.Ref{Name: "lonely"})
		var foreign *notManagedError
		if !errors.As(derr, &foreign) {
			t.Fatalf("Destroy/namespace: the concrete type did not survive two levels of wrapping: %v", derr)
		}
		if foreign.kind != "namespace" || foreign.fromRead {
			t.Errorf("Destroy/namespace: refusal is kind=%q fromRead=%v, want namespace/false",
				foreign.kind, foreign.fromRead)
		}
		if foreign.ns != "" {
			t.Errorf("a Namespace is cluster-scoped, so its refusal must carry no namespace of its "+
				"own — an 'in namespace \"muster-lonely\"' clause about muster-lonely itself reads as "+
				"a second object; got %q", foreign.ns)
		}
		if !errors.Is(derr, provision.ErrNotManaged) || errors.Is(derr, provision.ErrNotFound) ||
			errors.Is(derr, provision.ErrBlind) {
			t.Errorf("Destroy/namespace sentinels are wrong: %v", derr)
		}
	})

	// 🔴 THE OTHER DIRECTION, so the assertions above cannot be satisfied by a
	// driver that flags every refusal the same way: a by-name READ must keep
	// ErrNotFound and must be read-flavoured.
	t.Run("read paths stay ErrNotFound", func(t *testing.T) {
		d, cs := internalDriver(t)
		cs.AppsV1().Deployments(internalNS).Create(ctx, //nolint:errcheck
			newForeignDeployment("rd"), metav1.CreateOptions{})
		_, err := d.Get(ctx, provision.Ref{Name: "rd"})
		var foreign *notManagedError
		if !errors.As(err, &foreign) {
			t.Fatalf("Get: the concrete type did not survive: %v", err)
		}
		if !foreign.fromRead {
			t.Error("Get's refusal must be read-flavoured: a status read that reported a stranger's " +
				"Deployment as an instance is the defect the ownership check exists for, and " +
				"'muster has no instance by that name' is the right answer to give")
		}
		if !errors.Is(err, provision.ErrNotFound) || !errors.Is(err, provision.ErrNotManaged) {
			t.Errorf("Get's refusal must report BOTH ErrNotFound and ErrNotManaged, got %v", err)
		}
		if errors.Is(err, provision.ErrBlind) {
			t.Errorf("Get's refusal must not report ErrBlind, got %v", err)
		}
	})
}

// TestLabelComplaintNamesTheKeyThatIsActuallyWrong.
//
// 🔴 THE MEASURED DEFECT: the refusal printed
// `app.kubernetes.io/managed-by=muster` and nothing else, while owned()
// requires BOTH keys of managedLabels. An object labelled managed-by=muster
// with app.kubernetes.io/name set to something else was therefore refused with
// "it does not carry app.kubernetes.io/managed-by=muster" — a FALSE statement
// about the object in front of the operator, whose only named remedy was
// already satisfied. Relabelling is the one documented escape from these
// refusals, so following that message looped.
func TestLabelComplaintNamesTheKeyThatIsActuallyWrong(t *testing.T) {
	// ⚠ THE EXPECTATIONS ARE WHOLE STRINGS, NOT KEYWORDS. A guard on words is
	// walkable by rewording; this one fails on a cosmetic change, which is the
	// price of a machine-readable claim about what an operator is told.
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "neither key present",
			labels: map[string]string{},
			want: `it does not carry app.kubernetes.io/managed-by, which must be "muster"; ` +
				`it does not carry app.kubernetes.io/name, which must be "muster-agent"`,
		},
		{
			name: "both keys present with somebody else's values",
			labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/name":       "grafana",
			},
			want: `its app.kubernetes.io/managed-by is "Helm", not "muster"; ` +
				`its app.kubernetes.io/name is "grafana", not "muster-agent"`,
		},
		{
			// 🔴 THE CASE THE OLD MESSAGE WAS FALSE ABOUT. managed-by is exactly
			// right; the message must not name it as the problem.
			name: "managed-by correct, name wrong",
			labels: map[string]string{
				"app.kubernetes.io/managed-by": "muster",
				"app.kubernetes.io/name":       "something-else",
			},
			want: `its app.kubernetes.io/name is "something-else", not "muster-agent"`,
		},
		{
			// The mirror image, so the function is not merely reporting whichever
			// key it happens to check second.
			name: "name correct, managed-by wrong",
			labels: map[string]string{
				"app.kubernetes.io/managed-by": "flux",
				"app.kubernetes.io/name":       "muster-agent",
			},
			want: `its app.kubernetes.io/managed-by is "flux", not "muster"`,
		},
		{
			name: "name correct, managed-by absent",
			labels: map[string]string{
				"app.kubernetes.io/name": "muster-agent",
			},
			want: `it does not carry app.kubernetes.io/managed-by, which must be "muster"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The premise: owned() really does reject this fixture. Without it a
			// case could assert a complaint about labels that are in fact fine.
			if owned(c.labels) {
				t.Fatalf("premise: owned(%v) is true, so this fixture never produces a refusal", c.labels)
			}
			if got := labelComplaint(c.labels); got != c.want {
				t.Errorf("labelComplaint(%v)\n got %q\nwant %q", c.labels, got, c.want)
			}
		})
	}

	// The disagreement detector: labelComplaint and owned() are derived from the
	// same managedLabels, so a fully-correct label set can only reach
	// labelComplaint if those two derivations have drifted apart. This asserts
	// the branch says so rather than printing an empty complaint.
	t.Run("a fully labelled object reports the disagreement", func(t *testing.T) {
		if !owned(managedLabels) {
			t.Fatal("premise: owned(managedLabels) must be true")
		}
		got := labelComplaint(managedLabels)
		if !strings.Contains(got, "disagree") {
			t.Errorf("labelComplaint over correct labels must name the disagreement rather than "+
				"printing an empty parenthesis; got %q", got)
		}
	})
}

// TestTheRefusalMessageCarriesTheComplaintToTheOperator is the behavioural half:
// labelComplaint being right is worth nothing if the message does not use it.
func TestTheRefusalMessageCarriesTheComplaintToTheOperator(t *testing.T) {
	ctx := context.Background()
	d, cs := internalDriver(t)
	cs.CoreV1().ServiceAccounts(internalNS).Create(ctx, //nolint:errcheck
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: "half", Namespace: internalNS, Labels: map[string]string{
				"app.kubernetes.io/managed-by": "muster",
				"app.kubernetes.io/name":       "something-else",
			},
		}}, metav1.CreateOptions{})

	err := d.Create(ctx, internalSpec("half"))
	if err == nil {
		t.Fatal("Create overwrote a half-labelled ServiceAccount")
	}
	msg := err.Error()
	if !strings.Contains(msg, `its app.kubernetes.io/name is "something-else", not "muster-agent"`) {
		t.Errorf("the refusal does not name the label that is actually wrong: %q", msg)
	}
	// 🔴 AND IT MUST NOT NAME THE KEY THAT IS CORRECT. This is the whole finding:
	// the old message's only named remedy was already satisfied, so an operator
	// following it looped.
	if strings.Contains(msg, "does not carry app.kubernetes.io/managed-by") {
		t.Errorf("the refusal blames app.kubernetes.io/managed-by, which this object carries "+
			"correctly — the remedy it names is already done: %q", msg)
	}
}
