package k8s_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/k8s"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// This file is about ONE property: a name is not an identity. Every case here
// seeds an object that muster did not create, under a name an instance asks
// for, and asserts the driver neither reports it, nor writes to it, nor deletes
// it.
//
// 🔴 IT IS ONLY FULLY REACHABLE UNDER THE SHARED-NAMESPACE LAYOUT, which is why
// the harness had to be parameterised first. With a namespace per instance the
// collisions are rarer; in a namespace the operator also uses, every workload
// in it is a candidate.

// foreignLabels are what somebody ELSE's objects carry: a plausible set that
// does not include muster's pair. `app.kubernetes.io/name` is present on
// purpose with a different value, so a predicate that checked only for the
// KEY's existence would still pass this fixture.
func foreignLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "grafana",
		"app.kubernetes.io/managed-by": "Helm",
	}
}

// seedForeign puts a Deployment, a Service, a ConfigMap, a Secret and a
// ServiceAccount under `name` in `ns`, none of them muster's.
func seedForeign(t *testing.T, cs *fake.Clientset, ns, name string) {
	t.Helper()
	ctx := context.Background()
	meta := func(n string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: n, Namespace: ns, Labels: foreignLabels()}
	}
	replicas := int32(2)
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{
		ObjectMeta: meta(name),
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: meta(name),
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": name}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: meta(name + "-files"),
		BinaryData: map[string][]byte{"dashboard.json": []byte(`{"panels":[]}`)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed configmap: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: meta(name + "-env"),
		Data:       map[string][]byte{"ADMIN_PASSWORD": []byte("someone-elses-value-70413")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: meta(name),
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed serviceaccount: %v", err)
	}
}

// assertForeignSurvives reads back every object seedForeign created.
func assertForeignSurvives(t *testing.T, cs *fake.Clientset, ns, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("a Deployment muster did not create was removed: %v", err)
	}
	if _, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("a Service muster did not create was removed: %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name+"-files", metav1.GetOptions{}); err != nil {
		t.Errorf("a ConfigMap muster did not create was removed: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{}); err != nil {
		t.Errorf("a Secret muster did not create was removed: %v", err)
	}
	if _, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("a ServiceAccount muster did not create was removed: %v", err)
	}
}

// TestDestroyLeavesWorkloadsItDidNotCreateAlone.
//
// 🔴 THE MEASURED DEFECT: Destroy deleted by NAME. Destroy(Ref{Name:"grafana"})
// in a shared namespace removed a Service, a ConfigMap and a ServiceAccount
// belonging to somebody else, and returned nil — a successful teardown of an
// instance that never existed.
func TestDestroyLeavesWorkloadsItDidNotCreateAlone(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("grafana")
		seedForeign(t, cs, ns, "grafana")

		err := d.Destroy(ctx, provision.Ref{Name: "grafana"})
		if err == nil {
			t.Fatal("Destroy reported success for a workload muster did not create")
		}
		if !strings.Contains(err.Error(), "not managed by muster") {
			t.Errorf("the refusal must say why it refused, got %q", err)
		}
		assertForeignSurvives(t, cs, ns, "grafana")

		// 🔴 THE CONTROL THAT MAKES THE REFUSAL ABOUT OWNERSHIP RATHER THAN
		// ABOUT DESTROY BEING BROKEN: an instance muster DID create, in the same
		// namespace, is destroyed normally.
		spec := provisiontest.MinimalSpec("ours")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("control Create: %v", err)
		}
		if err := d.Destroy(ctx, spec.Ref); err != nil {
			t.Fatalf("control: Destroy of muster's own instance must succeed, got %v", err)
		}
		if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "ours", metav1.GetOptions{}); err == nil {
			t.Error("control: muster's own Deployment survived its Destroy")
		}
		// And the foreign objects are STILL there after a successful destroy
		// next door.
		assertForeignSurvives(t, cs, ns, "grafana")
	})
}

// TestDestroyLeavesCoNamedObjectsOfOtherKindsAlone is the case that REACHES
// the per-object ownership check. When the Deployment is muster's, the teardown
// proceeds — and every other object it deletes is named after the instance, so
// a ConfigMap, Secret, Service or claim somebody else put under those names is
// in the blast radius. This spec asks for none of them, so every co-named
// object here belongs to somebody else.
func TestDestroyLeavesCoNamedObjectsOfOtherKindsAlone(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("ours")

		spec := provisiontest.MinimalSpec("ours")
		spec.Ports = nil // no Service, no files, no secrets: muster renders none of them
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		meta := func(n string) metav1.ObjectMeta {
			return metav1.ObjectMeta{Name: n, Namespace: ns, Labels: foreignLabels()}
		}
		if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: meta("ours-files"),
			BinaryData: map[string][]byte{"theirs.json": []byte("{}")},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed configmap: %v", err)
		}
		if _, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: meta("ours-env"),
			Data:       map[string][]byte{"THEIR_TOKEN": []byte("someone-elses-value-70413")},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed secret: %v", err)
		}
		if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{
			ObjectMeta: meta("ours"),
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed service: %v", err)
		}
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: meta("ours-workspace"),
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pvc: %v", err)
		}

		if err := d.Destroy(ctx, spec.Ref); err != nil {
			t.Fatalf("Destroy of muster's own instance: %v", err)
		}
		// Control: muster's own objects went.
		if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "ours", metav1.GetOptions{}); err == nil {
			t.Error("control: muster's Deployment survived its own Destroy")
		}
		if _, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "ours", metav1.GetOptions{}); err == nil {
			t.Error("control: muster's ServiceAccount survived its own Destroy")
		}
		// And nobody else's did.
		if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "ours-files", metav1.GetOptions{}); err != nil {
			t.Errorf("a ConfigMap muster never created was deleted by name: %v", err)
		}
		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "ours-env", metav1.GetOptions{}); err != nil {
			t.Errorf("a Secret muster never created was deleted by name: %v", err)
		}
		if _, err := cs.CoreV1().Services(ns).Get(ctx, "ours", metav1.GetOptions{}); err != nil {
			t.Errorf("a Service muster never created was deleted by name: %v", err)
		}
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "ours-workspace", metav1.GetOptions{}); err != nil {
			t.Errorf("a PersistentVolumeClaim muster never created was deleted by name — and a claim is "+
				"somebody's data: %v", err)
		}
	})
}

// TestApplyRefusesToWriteOverACoNamedObjectOfAnotherKind reaches the ownership
// check on the WRITE path. There is no Deployment in the way here, so Create
// runs the whole of apply, and the first object it writes — the ServiceAccount
// — is one somebody else already owns.
func TestApplyRefusesToWriteOverACoNamedObjectOfAnotherKind(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("collide")

		if _, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "collide", Namespace: ns, Labels: foreignLabels()},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed serviceaccount: %v", err)
		}

		err := d.Create(ctx, provisiontest.MinimalSpec("collide"))
		if err == nil {
			t.Fatal("Create overwrote a ServiceAccount muster did not create")
		}
		if !strings.Contains(err.Error(), "not managed by muster") {
			t.Errorf("the refusal must say why, got %q", err)
		}
		sa, gerr := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "collide", metav1.GetOptions{})
		if gerr != nil {
			t.Fatalf("get serviceaccount: %v", gerr)
		}
		if sa.Labels["app.kubernetes.io/managed-by"] != "Helm" {
			t.Errorf("the foreign ServiceAccount was overwritten; labels are now %v", sa.Labels)
		}
		// A ServiceAccount is an identity: taking it over is taking over
		// whatever it can do. The Deployment must not exist either.
		if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "collide", metav1.GetOptions{}); err == nil {
			t.Error("the refused Create still produced a Deployment")
		}
	})
}

// TestApplyRefusesAForeignWorkspaceClaim is the same check on the PVC, where
// the consequence is different in kind: an existing claim is create-only, so
// without the check a claim somebody else made under this name would simply be
// MOUNTED into the agent's pod, handing it their data.
func TestApplyRefusesAForeignWorkspaceClaim(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("claimer")

		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "claimer-workspace", Namespace: ns, Labels: foreignLabels()},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pvc: %v", err)
		}

		spec := provisiontest.MinimalSpec("claimer")
		spec.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
		err := d.Create(ctx, spec)
		if err == nil {
			t.Fatal("Create mounted a PersistentVolumeClaim muster did not create")
		}
		if !strings.Contains(err.Error(), "not managed by muster") {
			t.Errorf("the refusal must say why, got %q", err)
		}
		if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "claimer", metav1.GetOptions{}); err == nil {
			t.Error("the refused Create still produced a Deployment that would mount the foreign claim")
		}

		// Control: muster's OWN claim is reused without complaint, so the
		// refusal is about ownership and not about the claim already existing.
		if err := d.Create(ctx, provisiontest.MinimalSpec("clean")); err != nil {
			t.Fatalf("control Create: %v", err)
		}
		clean := provisiontest.MinimalSpec("clean")
		clean.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
		if err := d.Update(ctx, clean); err != nil {
			t.Fatalf("control: adding a workspace to muster's own instance: %v", err)
		}
		if err := d.Update(ctx, clean); err != nil {
			t.Fatalf("control: a second apply over muster's OWN existing claim must succeed, got %v", err)
		}
	})
}

// TestReadPathsRefuseAWorkloadTheyDidNotCreate covers every by-name read, not
// just Get: each one fetches a Deployment by name, and a foreign workload
// answering to that name is reported as an instance — with a phase, a pod and a
// restart count belonging to a stranger. Scale is the one that also WRITES.
func TestReadPathsRefuseAWorkloadTheyDidNotCreate(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("grafana")
		seedForeign(t, cs, ns, "grafana")
		ref := provision.Ref{Name: "grafana"}

		t.Run("Get", func(t *testing.T) {
			inst, err := d.Get(ctx, ref)
			if !errors.Is(err, provision.ErrNotFound) {
				t.Fatalf("Get of a foreign workload must be ErrNotFound, got instance %+v err %v", inst, err)
			}
			if !strings.Contains(err.Error(), "not managed by muster") {
				t.Errorf("the refusal must say why, got %q", err)
			}
		})

		t.Run("Scale", func(t *testing.T) {
			err := d.Scale(ctx, ref, 0)
			if !errors.Is(err, provision.ErrNotFound) {
				t.Fatalf("Scale of a foreign workload must be refused, got %v", err)
			}
			// 🔴 THE STATE, NOT JUST THE ERROR. Scaling somebody else's
			// Deployment to zero is an outage nobody would attribute to muster.
			dep, gerr := cs.AppsV1().Deployments(ns).Get(ctx, "grafana", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get seeded deployment: %v", gerr)
			}
			if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
				t.Errorf("the foreign Deployment's replicas are %v, want the 2 it was seeded with",
					dep.Spec.Replicas)
			}
		})

		t.Run("Endpoint", func(t *testing.T) {
			if _, err := d.Endpoint(ctx, ref); !errors.Is(err, provision.ErrNotFound) {
				t.Fatalf("Endpoint of a foreign workload must be refused, got %v", err)
			}
		})

		t.Run("TailLogs", func(t *testing.T) {
			if _, err := d.TailLogs(ctx, ref, 10); !errors.Is(err, provision.ErrNotFound) {
				t.Fatalf("TailLogs of a foreign workload must be refused — its output is not muster's "+
					"to serve; got %v", err)
			}
		})

		t.Run("List", func(t *testing.T) {
			spec := provisiontest.MinimalSpec("ours")
			if err := d.Create(ctx, spec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			list, err := d.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			// The server-side selector and the client-side predicate must agree
			// about the same object set.
			if len(list) != 1 || list[0].Ref.Name != "ours" {
				t.Fatalf("List reported %+v; want exactly the one instance muster created", list)
			}
			// Control for the whole subtest group: the driver DOES answer for
			// its own instance on every path that just refused.
			if _, err := d.Get(ctx, spec.Ref); err != nil {
				t.Errorf("control: Get of muster's own instance: %v", err)
			}
			if err := d.Scale(ctx, spec.Ref, 2); err != nil {
				t.Errorf("control: Scale of muster's own instance: %v", err)
			}
			if _, err := d.Endpoint(ctx, spec.Ref); err != nil {
				t.Errorf("control: Endpoint of muster's own instance: %v", err)
			}
			if _, err := d.TailLogs(ctx, spec.Ref, 10); err != nil {
				t.Errorf("control: TailLogs of muster's own instance: %v", err)
			}
		})
	})
}

// TestCreateAndUpdateRefuseToAdoptAForeignWorkload. Create's refusal keeps the
// divergence sentinel — the caller's branch is unchanged, "refused, nothing
// written" — while Update, which creates what is ABSENT, must not create over
// what is FOREIGN.
func TestCreateAndUpdateRefuseToAdoptAForeignWorkload(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		ns := m.ns("grafana")
		spec := provisiontest.MinimalSpec("grafana")

		t.Run("Create", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			seedForeign(t, cs, ns, "grafana")
			err := d.Create(ctx, spec)
			if !errors.Is(err, provision.ErrDivergentSpec) {
				t.Fatalf("Create over a foreign workload must be refused with ErrDivergentSpec, got %v", err)
			}
			if !strings.Contains(err.Error(), "not managed by muster") {
				t.Errorf("the refusal must name the reason rather than reporting a missing fingerprint, got %q", err)
			}
			assertForeignSurvives(t, cs, ns, "grafana")
			assertForeignUntouched(t, cs, ns, "grafana")
		})

		t.Run("Update", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			seedForeign(t, cs, ns, "grafana")
			err := d.Update(ctx, spec)
			if err == nil {
				t.Fatal("Update took over a workload muster did not create")
			}
			if !strings.Contains(err.Error(), "not managed by muster") {
				t.Errorf("the refusal must say why, got %q", err)
			}
			assertForeignSurvives(t, cs, ns, "grafana")
			assertForeignUntouched(t, cs, ns, "grafana")
		})

		// 🔴 THE CASE THAT REACHES UPDATE'S OWN PRE-FLIGHT CHECK. With a whole
		// foreign release in the way, the first object apply writes already
		// collides and refuses — so that fixture cannot tell a pre-flight check
		// from a per-object one. Here ONLY the Deployment is foreign: apply
		// would create muster's ServiceAccount and Service first and refuse at
		// the very last object, leaving muster-labelled leftovers scattered
		// over somebody else's release.
		t.Run("Update with only the Deployment in the way", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			replicas := int32(2)
			if _, err := cs.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "grafana", Namespace: ns, Labels: foreignLabels()},
				Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed deployment: %v", err)
			}
			err := d.Update(ctx, spec)
			if err == nil {
				t.Fatal("Update took over a workload muster did not create")
			}
			if !strings.Contains(err.Error(), "not managed by muster") {
				t.Errorf("the refusal must say why, got %q", err)
			}
			if _, gerr := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "grafana", metav1.GetOptions{}); gerr == nil {
				t.Error("the refused Update created a ServiceAccount next to somebody else's Deployment")
			}
			if _, gerr := cs.CoreV1().Services(ns).Get(ctx, "grafana", metav1.GetOptions{}); gerr == nil {
				t.Error("the refused Update created a Service whose selector points at muster's pods")
			}
			dep, gerr := cs.AppsV1().Deployments(ns).Get(ctx, "grafana", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get seeded deployment: %v", gerr)
			}
			if dep.Labels["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("the foreign Deployment was overwritten; labels are now %v", dep.Labels)
			}
		})
	})
}

// assertForeignUntouched checks that the seeded objects still carry the
// foreign labels — i.e. that nothing overwrote them with muster's.
func assertForeignUntouched(t *testing.T, cs *fake.Clientset, ns, name string) {
	t.Helper()
	ctx := context.Background()
	sa, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get serviceaccount: %v", err)
	}
	if sa.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		t.Errorf("the foreign ServiceAccount was overwritten; its labels are now %v", sa.Labels)
	}
	svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		t.Errorf("the foreign Service was overwritten; its labels are now %v", svc.Labels)
	}
	if svc.Spec.Selector["app"] != name {
		t.Errorf("the foreign Service's selector was rewritten to %v; its traffic now goes to muster's pods",
			svc.Spec.Selector)
	}
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name+"-files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if _, ok := cm.BinaryData["dashboard.json"]; !ok {
		t.Errorf("the foreign ConfigMap's content was replaced; keys are now %v", keysOf(cm.BinaryData))
	}
	sec, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(sec.Data["ADMIN_PASSWORD"]) != "someone-elses-value-70413" {
		t.Errorf("the foreign Secret's content was replaced; keys are now %v", keysOf(sec.Data))
	}
}

// TestCreateDoesNotAdoptANamespaceItDidNotCreate.
//
// 🔴 MEASURED: a pre-existing namespace was adopted on AlreadyExists with no
// check, and Destroy deletes the namespace WHOLE — so adopting somebody's
// namespace schedules the deletion of everything in it.
//
// ⚠ PER-INSTANCE LAYOUT ONLY, because it is the only one that creates or
// deletes a namespace at all. The shared-namespace half of the same property —
// that the driver creates NO namespace there — is asserted in
// TestRendersTheMinimalObjectSet.
func TestCreateDoesNotAdoptANamespaceItDidNotCreate(t *testing.T) {
	perInstance := nsModes[0]
	ctx := context.Background()
	d, cs := newDriver(t, perInstance, nil)

	const ns = "muster-monitoring"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: foreignLabels()},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "alert-rules", Namespace: ns, Labels: foreignLabels()},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed configmap: %v", err)
	}

	err := d.Create(ctx, provisiontest.MinimalSpec("monitoring"))
	if err == nil {
		t.Fatal("Create adopted a namespace muster did not create")
	}
	if !strings.Contains(err.Error(), "refusing to adopt") {
		t.Errorf("the refusal must say what it refused, got %q", err)
	}
	// The refusal happens BEFORE anything is written into that namespace.
	if _, gerr := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "monitoring", metav1.GetOptions{}); gerr == nil {
		t.Error("the refused Create wrote a ServiceAccount into the namespace anyway")
	}
	if _, gerr := cs.CoreV1().ConfigMaps(ns).Get(ctx, "alert-rules", metav1.GetOptions{}); gerr != nil {
		t.Errorf("the namespace's existing content was disturbed: %v", gerr)
	}

	// 🔴 CONTROL. A namespace muster creates itself is reused on the next
	// Create without complaint, so the refusal above is about ownership and not
	// about AlreadyExists.
	spec := provisiontest.MinimalSpec("ours")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("control Create: %v", err)
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("control: a second Create over muster's own namespace must be idempotent, got %v", err)
	}
}

// TestDestroyDoesNotDeleteANamespaceItDidNotCreate is the other half of the
// adoption defect: even reached with no Deployment in the way, the namespace
// delete is ownership-checked.
func TestDestroyDoesNotDeleteANamespaceItDidNotCreate(t *testing.T) {
	perInstance := nsModes[0]
	ctx := context.Background()
	d, cs := newDriver(t, perInstance, nil)

	const ns = "muster-lonely"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: foreignLabels()},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}

	if err := d.Destroy(ctx, provision.Ref{Name: "lonely"}); err != nil {
		t.Fatalf("Destroy of an instance muster never created must be nil, got %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
		t.Fatalf("a namespace muster did not create was deleted by Destroy: %v", err)
	}

	// 🔴 CONTROL. The namespace muster DID create is deleted, so the case above
	// is measuring the ownership check rather than a Destroy that deletes no
	// namespaces at all.
	spec := provisiontest.MinimalSpec("ours")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("control Create: %v", err)
	}
	if err := d.Destroy(ctx, spec.Ref); err != nil {
		t.Fatalf("control Destroy: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, "muster-ours", metav1.GetOptions{}); err == nil {
		t.Error("control: muster's own namespace survived its Destroy")
	}
}

// TestDestroyRemovesNamespacedPolicyObjects.
//
// 🔴 ONLY OBSERVABLE UNDER THE SHARED-NAMESPACE LAYOUT IN PRODUCTION, which is
// why it went unnoticed: the teardown enumerated ClusterRoles and
// ClusterRoleBindings only, and the namespaced Role and RoleBinding were
// removed — where they were removed at all — by the namespace deletion. In a
// shared namespace nothing deletes them, and a RoleBinding whose subject is
// `ServiceAccount <ns>/<name>` outlives the instance: the next instance to take
// that name gets a ServiceAccount with the same name in the same namespace, and
// the dangling binding grants it access nobody asked for.
func TestDestroyRemovesNamespacedPolicyObjects(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("recycled")
		spec := provisiontest.MinimalSpec("recycled")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := provision.Grant(ctx, d, spec.Ref, grantablePolicy); err != nil {
			t.Fatalf("Grant: %v", err)
		}
		name := k8s.PolicyObjectName("recycled", grantablePolicy.Name)

		// Control: they exist, and the binding names the ServiceAccount a
		// namesake would inherit.
		rb, err := cs.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("control: the RoleBinding must exist before the destroy: %v", err)
		}
		if len(rb.Subjects) != 1 || rb.Subjects[0].Kind != rbacv1.ServiceAccountKind ||
			rb.Subjects[0].Name != "recycled" || rb.Subjects[0].Namespace != ns {
			t.Fatalf("control: the binding's subject is %+v, want the instance's own ServiceAccount", rb.Subjects)
		}
		if _, err := cs.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Fatalf("control: the Role must exist before the destroy: %v", err)
		}

		if err := d.Destroy(ctx, spec.Ref); err != nil {
			t.Fatalf("Destroy: %v", err)
		}

		if _, err := cs.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			t.Error("the RoleBinding survived the destroy; recreating an instance with this name " +
				"recreates its exact subject, so the grant comes back with it")
		}
		if _, err := cs.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			t.Error("the Role survived the destroy")
		}

		// And the namesake really is recreated verbatim — which is what makes
		// the dangling binding dangerous rather than merely untidy.
		if err := d.Create(ctx, provisiontest.MinimalSpec("recycled")); err != nil {
			t.Fatalf("recreate: %v", err)
		}
		if _, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "recycled", metav1.GetOptions{}); err != nil {
			t.Fatalf("the recreated instance has no ServiceAccount, so this case's premise is wrong: %v", err)
		}
	})
}

// TestPolicyObjectNamesCannotCollideAcrossInstances.
//
// 🔴 THE MEASURED CONSEQUENCE of joining the two components with `-`: the
// second Grant rewrote the FIRST instance's ClusterRole, while the
// ClusterRoleBinding — create-only, because RoleRef is immutable — kept the
// first instance's ServiceAccount as its subject. One instance was silently
// escalated to the other's rules, the other got nothing, and both Grants
// returned nil.
func TestPolicyObjectNamesCannotCollideAcrossInstances(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()

		// The two pairs whose naive join is the same string.
		first := struct{ instance, policy string }{"agent-a", "ops"}
		second := struct{ instance, policy string }{"agent", "a-ops"}
		if "muster-"+first.instance+"-"+first.policy != "muster-"+second.instance+"-"+second.policy {
			t.Fatal("control: the fixture pairs do not collide under a naive join, so this case measures nothing")
		}

		nameFirst := k8s.PolicyObjectName(first.instance, first.policy)
		nameSecond := k8s.PolicyObjectName(second.instance, second.policy)
		if nameFirst == nameSecond {
			t.Fatalf("(%q,%q) and (%q,%q) produce the same object name %q: one instance's grant "+
				"overwrites the other's rules while the binding keeps the first subject",
				first.instance, first.policy, second.instance, second.policy, nameFirst)
		}

		// The behavioural half: two instances, two policies, distinct rules.
		d, cs := newDriver(t, m, nil)
		for _, inst := range []string{first.instance, second.instance} {
			if err := d.Create(ctx, provisiontest.MinimalSpec(inst)); err != nil {
				t.Fatalf("Create %s: %v", inst, err)
			}
		}
		polFirst := provision.Policy{Name: first.policy, Rules: mustJSON(k8s.Rules{
			ClusterRules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"},
			}},
		})}
		polSecond := provision.Policy{Name: second.policy, Rules: mustJSON(k8s.Rules{
			ClusterRules: []rbacv1.PolicyRule{{
				APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create"},
			}},
		})}
		if err := provision.Grant(ctx, d, provision.Ref{Name: first.instance}, polFirst); err != nil {
			t.Fatalf("Grant %s: %v", first.policy, err)
		}
		if err := provision.Grant(ctx, d, provision.Ref{Name: second.instance}, polSecond); err != nil {
			t.Fatalf("Grant %s: %v", second.policy, err)
		}

		for _, want := range []struct {
			name     string
			instance string
			resource string
		}{
			{nameFirst, first.instance, "nodes"},
			{nameSecond, second.instance, "jobs"},
		} {
			cr, err := cs.RbacV1().ClusterRoles().Get(ctx, want.name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("clusterrole %s: %v", want.name, err)
			}
			if len(cr.Rules) != 1 || len(cr.Rules[0].Resources) != 1 || cr.Rules[0].Resources[0] != want.resource {
				t.Errorf("clusterrole %s carries %+v, want the rules granted to %q",
					want.name, cr.Rules, want.instance)
			}
			crb, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, want.name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("clusterrolebinding %s: %v", want.name, err)
			}
			if len(crb.Subjects) != 1 || crb.Subjects[0].Name != want.instance {
				t.Errorf("binding %s names %+v, want the ServiceAccount of %q",
					want.name, crb.Subjects, want.instance)
			}
		}

		// Revoke finds them by the same derivation, so one revoke must not take
		// the other's objects with it.
		if err := provision.Revoke(ctx, d, provision.Ref{Name: first.instance}, first.policy); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if _, err := cs.RbacV1().ClusterRoles().Get(ctx, nameSecond, metav1.GetOptions{}); err != nil {
			t.Errorf("revoking one instance's policy removed the other's ClusterRole: %v", err)
		}
		if _, err := cs.RbacV1().ClusterRoles().Get(ctx, nameFirst, metav1.GetOptions{}); err == nil {
			t.Error("control: Revoke did not remove the policy it was asked to remove")
		}
	})
}

// TestUpdateRemovesObjectsTheSpecNoLongerAsksFor.
//
// 🔴 A CREDENTIAL REMOVED FROM A SPEC HAS TO LEAVE THE CLUSTER. The render
// functions return nil for an object the spec no longer needs, and until the
// sweep in apply, nothing deleted it: the Secret kept the revoked token, the
// ConfigMap kept the file, and the Service kept selecting live pods.
func TestUpdateRemovesObjectsTheSpecNoLongerAsksFor(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("shrinking")

		full := provisiontest.MinimalSpec("shrinking")
		full.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "revoked-value-51199"}}
		full.Files = []provision.File{
			{Path: "/etc/muster/agent.json", Content: []byte(`{"tools":["tasks"]}`)},
			{Path: "/etc/muster/token", Content: []byte("file-content-30517"), Secret: true},
		}
		if err := d.Create(ctx, full); err != nil {
			t.Fatalf("Create: %v", err)
		}

		// Control: everything exists first, so the absences below are removals.
		for _, o := range []struct {
			what string
			get  func() error
		}{
			{"configmap", func() error {
				_, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "shrinking-files", metav1.GetOptions{})
				return err
			}},
			{"env secret", func() error {
				_, err := cs.CoreV1().Secrets(ns).Get(ctx, "shrinking-env", metav1.GetOptions{})
				return err
			}},
			{"file secret", func() error {
				_, err := cs.CoreV1().Secrets(ns).Get(ctx, "shrinking-secret-files", metav1.GetOptions{})
				return err
			}},
			{"service", func() error {
				_, err := cs.CoreV1().Services(ns).Get(ctx, "shrinking", metav1.GetOptions{})
				return err
			}},
		} {
			if err := o.get(); err != nil {
				t.Fatalf("control: the %s must exist before the update: %v", o.what, err)
			}
		}

		shrunk := provisiontest.MinimalSpec("shrinking")
		shrunk.Secrets = nil
		shrunk.Files = nil
		shrunk.Ports = nil
		if err := d.Update(ctx, shrunk); err != nil {
			t.Fatalf("Update: %v", err)
		}

		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "shrinking-env", metav1.GetOptions{}); err == nil {
			t.Error("the Secret holding the dropped credential survived the update; removing a credential " +
				"from a spec has to remove it from the cluster")
		}
		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "shrinking-secret-files", metav1.GetOptions{}); err == nil {
			t.Error("the Secret holding the dropped confidential file survived the update")
		}
		if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "shrinking-files", metav1.GetOptions{}); err == nil {
			t.Error("the ConfigMap holding the dropped files survived the update")
		}
		if _, err := cs.CoreV1().Services(ns).Get(ctx, "shrinking", metav1.GetOptions{}); err == nil {
			t.Error("the Service survived a spec that declares no ports; its selector still matches live pods")
		}

		// The pod no longer references any of it either — a dangling reference
		// is a pod that cannot start.
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "shrinking", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("deployment: %v", err)
		}
		if len(dep.Spec.Template.Spec.Containers[0].EnvFrom) != 0 {
			t.Errorf("the container still names a secret in envFrom: %+v",
				dep.Spec.Template.Spec.Containers[0].EnvFrom)
		}
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Secret != nil || v.ConfigMap != nil {
				t.Errorf("the pod still mounts %q after the files were dropped", v.Name)
			}
		}
	})
}

// TestUpdateLeavesTheWorkspaceClaimAlone is a DECLARED LIMIT, not regression
// coverage: the sweep above deliberately does not delete the PersistentVolume
// Claim, because it holds the instance's data and a spec edit that turns
// Persist off would otherwise destroy it on the next reconcile. Removing the
// claim is an operator's deliberate act.
func TestUpdateLeavesTheWorkspaceClaimAlone(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("stateful")

		spec := provisiontest.MinimalSpec("stateful")
		spec.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "stateful-workspace", metav1.GetOptions{}); err != nil {
			t.Fatalf("control: the claim must exist first: %v", err)
		}

		stateless := provisiontest.MinimalSpec("stateful")
		if err := d.Update(ctx, stateless); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "stateful-workspace", metav1.GetOptions{}); err != nil {
			t.Fatalf("the workspace claim was deleted by a spec change; its data goes with it: %v", err)
		}
	})
}
