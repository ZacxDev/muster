package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// The least-privilege RBAC layout (rolebinding.go).
//
// 🔴 THE FAKE BELOW ENFORCES THE NARROW GRANT, AND THAT IS WHAT MAKES THESE
// TESTS ABOUT THE LAYOUT RATHER THAN ABOUT THE CODE. The stock fake clientset
// authorises everything, so a driver that forgot the RoleBinding — or that
// listed Deployments across every namespace — passes against it. rbacFake
// answers 403 to every request NamespaceRoleBindingClusterRBAC does not cover,
// unless the namespace holds muster's RoleBinding to the configured
// ClusterRole; and it can hold that binding back for N requests, which is the
// propagation window.

const (
	testClusterRole = "muster-agent-namespace"
	testSANamespace = "muster-system"
	testSAName      = "muster"
)

func testBindingConfig() *NamespaceRoleBindingConfig {
	return &NamespaceRoleBindingConfig{
		ClusterRole:             testClusterRole,
		ServiceAccountNamespace: testSANamespace,
		ServiceAccountName:      testSAName,
	}
}

var rbGVR = rbacv1.SchemeGroupVersion.WithResource("rolebindings")

type rbacFake struct {
	cs *fake.Clientset

	mu sync.Mutex
	// lag is how many namespaced requests in a namespace are still refused
	// after its binding appears (propagation).
	lag      int
	served   map[string]int // namespace -> requests answered since its binding appeared
	denied   []string       // every refusal, "verb resource ns"
	refusals int            // refusals inside a bound namespace (the lag)
}

func newRBACFake(t *testing.T, objs ...runtime.Object) *rbacFake {
	t.Helper()
	return enforceRBAC(fake.NewClientset(objs...))
}

// enforceRBAC puts an existing clientset — and whatever it already holds —
// behind the narrow grant from now on.
func enforceRBAC(cs *fake.Clientset) *rbacFake {
	f := &rbacFake{cs: cs, served: map[string]int{}}
	cs.PrependReactor("*", "*", f.react)
	return f
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (f *rbacFake) react(a k8stesting.Action) (bool, runtime.Object, error) {
	res, verb, ns := a.GetResource().Resource, a.GetVerb(), a.GetNamespace()
	if a.GetSubresource() != "" {
		res += "/" + a.GetSubresource()
	}
	if cluster, ok := NamespaceRoleBindingClusterRBAC[res]; ok && contains(cluster, verb) {
		if res == "rolebindings" && verb == "create" {
			// `bind` is held on ONE ClusterRole only.
			rb := a.(k8stesting.CreateAction).GetObject().(*rbacv1.RoleBinding)
			if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != testClusterRole {
				return f.deny(a, verb, res, ns)
			}
		}
		return false, nil, nil
	}
	if ns == "" {
		// A cluster-wide read of anything else: the grant this layout removes.
		return f.deny(a, verb, res, ns)
	}
	obj, err := f.cs.Tracker().Get(rbGVR, ns, NamespaceRoleBindingName)
	if err != nil {
		return f.deny(a, verb, res, ns)
	}
	rb := obj.(*rbacv1.RoleBinding)
	if rb.RoleRef.Name != testClusterRole || len(rb.Subjects) != 1 ||
		rb.Subjects[0].Name != testSAName || rb.Subjects[0].Namespace != testSANamespace {
		return f.deny(a, verb, res, ns)
	}
	f.mu.Lock()
	n := f.served[ns]
	f.served[ns] = n + 1
	lagging := n < f.lag
	if lagging {
		f.refusals++
	}
	f.mu.Unlock()
	if lagging {
		return true, nil, rbacDenied(res, ns)
	}
	return false, nil, nil
}

func rbacDenied(res, ns string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: res}, "",
		fmt.Errorf("RBAC: access denied in namespace %q", ns))
}

func (f *rbacFake) deny(_ k8stesting.Action, verb, res, ns string) (bool, runtime.Object, error) {
	f.mu.Lock()
	f.denied = append(f.denied, verb+" "+res+" "+ns)
	f.mu.Unlock()
	return true, nil, rbacDenied(res, ns)
}

func (f *rbacFake) deniedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.denied...)
}

func bindingDriver(t *testing.T, cs *fake.Clientset) *Driver {
	t.Helper()
	empty := ""
	d, err := New(Config{Client: cs, NamespacePerInstance: true, NamespacePrefix: "muster-agent-",
		WorkspaceStorageClass: &empty, NetworkPolicy: internalNetworkPolicy(),
		NamespaceRoleBinding: testBindingConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func shortPropagation(t *testing.T, timeout time.Duration) {
	t.Helper()
	oldT, oldP := rbacPropagationTimeout, rbacPropagationPoll
	rbacPropagationTimeout, rbacPropagationPoll = timeout, time.Millisecond
	t.Cleanup(func() { rbacPropagationTimeout, rbacPropagationPoll = oldT, oldP })
}

// (a) The RoleBinding is the first object written into a new namespace, and
// it binds muster's ServiceAccount to the configured ClusterRole, carrying the
// ownership labels.
func TestBindingModeWritesTheRoleBindingBeforeAnyOtherNamespacedObject(t *testing.T) {
	cs := fake.NewClientset()
	d := bindingDriver(t, cs)
	if err := d.Create(context.Background(), isolatedSpec("alpha")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	const ns = "muster-agent-alpha"
	nsCreate, rbCreate, firstOther := -1, -1, -1
	var firstOtherWhat string
	for i, a := range cs.Actions() {
		if a.GetVerb() != "create" {
			continue
		}
		switch r := a.GetResource().Resource; {
		case r == "namespaces":
			nsCreate = i
		case r == "rolebindings":
			if rbCreate < 0 {
				rbCreate = i
			}
		case firstOther < 0:
			firstOther, firstOtherWhat = i, r
		}
	}
	if nsCreate < 0 || rbCreate < 0 || firstOther < 0 {
		t.Fatalf("instrument: namespace create %d, rolebinding create %d, other create %d — want all present",
			nsCreate, rbCreate, firstOther)
	}
	if !(nsCreate < rbCreate && rbCreate < firstOther) {
		t.Fatalf("order: namespace %d, rolebinding %d, first other (%s) %d — want namespace < rolebinding < everything else",
			nsCreate, rbCreate, firstOtherWhat, firstOther)
	}
	rb, err := cs.RbacV1().RoleBindings(ns).Get(context.Background(), NamespaceRoleBindingName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantRef := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: testClusterRole}
	if rb.RoleRef != wantRef {
		t.Errorf("roleRef = %+v, want %+v", rb.RoleRef, wantRef)
	}
	wantSub := rbacv1.Subject{Kind: "ServiceAccount", Name: testSAName, Namespace: testSANamespace}
	if len(rb.Subjects) != 1 || rb.Subjects[0] != wantSub {
		t.Errorf("subjects = %+v, want [%+v]", rb.Subjects, wantSub)
	}
	if !owned(rb.Labels) || rb.Labels[labelInstance] != "alpha" {
		t.Errorf("labels = %v, want muster's ownership labels and instance alpha", rb.Labels)
	}
}

// (b) Unset, no RoleBinding is read or written: today's behaviour.
func TestWithoutTheLayoutNoRoleBindingIsTouched(t *testing.T) {
	d, cs := perInstanceDriver(t)
	ctx := context.Background()
	if err := d.Create(ctx, isolatedSpec("alpha")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := d.Get(ctx, provision.Ref{Name: "alpha"}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := d.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := d.Destroy(ctx, provision.Ref{Name: "alpha"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	n := 0
	for _, a := range cs.Actions() {
		if r := a.GetResource().Resource; r == "rolebindings" || r == "clusterroles" {
			t.Errorf("unexpected %s %s with the layout off", a.GetVerb(), r)
		}
		n++
	}
	if n == 0 {
		t.Fatal("instrument: no actions recorded")
	}
}

// The seam: an instance's whole life under the NARROW grant, against a fake
// that enforces it. Nothing may be refused except the propagation lag.
func TestTheNarrowClusterGrantCoversAnInstancesWholeLife(t *testing.T) {
	shortPropagation(t, time.Second)
	f := newRBACFake(t)
	f.lag = 2
	d := bindingDriver(t, f.cs)
	ctx := context.Background()
	ref := provision.Ref{Name: "alpha"}

	steps := []struct {
		name string
		do   func() error
	}{
		{"Create", func() error { return d.Create(ctx, isolatedSpec("alpha")) }},
		{"Create again (idempotent)", func() error { return d.Create(ctx, isolatedSpec("alpha")) }},
		{"Get", func() error { _, err := d.Get(ctx, ref); return err }},
		{"List", func() error {
			got, err := d.List(ctx)
			if err == nil && (len(got) != 1 || got[0].Ref.Name != "alpha") {
				return fmt.Errorf("List = %+v, want exactly alpha", got)
			}
			return err
		}},
		{"Scale", func() error { return d.Scale(ctx, ref, 0) }},
		{"Endpoint", func() error { _, err := d.Endpoint(ctx, ref); return err }},
		{"Update", func() error { return d.Update(ctx, isolatedSpec("alpha")) }},
		{"Destroy", func() error { return d.Destroy(ctx, ref) }},
		{"Destroy again (absent)", func() error { return d.Destroy(ctx, ref) }},
		{"Get after destroy", func() error {
			if _, err := d.Get(ctx, ref); !errors.Is(err, provision.ErrNotFound) {
				return fmt.Errorf("Get = %v, want ErrNotFound", err)
			}
			return nil
		}},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v (refused so far: %v)", s.name, err, f.deniedList())
		}
	}
	if denied := f.deniedList(); len(denied) != 0 {
		t.Errorf("requests outside the narrow grant: %v", denied)
	}
	if f.refusals != f.lag {
		t.Errorf("propagation refusals = %d, want %d (the probe must absorb the lag)", f.refusals, f.lag)
	}
}

// (c) A namespace created before the layout was turned on has no RoleBinding.
// Every path gets it one, so an upgraded muster still manages the instance once
// the cluster-wide object grant is gone.
func TestANamespaceThatPredatesTheLayoutGetsItsRoleBinding(t *testing.T) {
	shortPropagation(t, time.Second)
	for _, tc := range []struct {
		name string
		do   func(*Driver) error
	}{
		{"Get", func(d *Driver) error { _, err := d.Get(context.Background(), provision.Ref{Name: "old"}); return err }},
		{"List", func(d *Driver) error {
			got, err := d.List(context.Background())
			if err == nil && len(got) != 1 {
				return fmt.Errorf("List = %d instances, want 1", len(got))
			}
			return err
		}},
		{"Scale", func(d *Driver) error { return d.Scale(context.Background(), provision.Ref{Name: "old"}, 0) }},
		{"Update", func(d *Driver) error { return d.Update(context.Background(), isolatedSpec("old")) }},
		{"Create", func(d *Driver) error { return d.Create(context.Background(), isolatedSpec("old")) }},
		{"Destroy", func(d *Driver) error { return d.Destroy(context.Background(), provision.Ref{Name: "old"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Provisioned by a muster WITHOUT the layout (the old ClusterRole).
			legacy, plain := perInstanceDriver(t)
			if err := legacy.Create(context.Background(), isolatedSpec("old")); err != nil {
				t.Fatalf("legacy Create: %v", err)
			}
			if _, err := plain.RbacV1().RoleBindings("muster-agent-old").Get(context.Background(),
				NamespaceRoleBindingName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatalf("instrument: the legacy namespace already has a binding (%v)", err)
			}
			// The same cluster state, now behind the narrow grant.
			f := enforceRBAC(plain)
			d := bindingDriver(t, f.cs)
			if err := tc.do(d); err != nil {
				t.Fatalf("%s on a namespace that predates the layout: %v (refused: %v)", tc.name, err, f.deniedList())
			}
			if denied := f.deniedList(); len(denied) != 0 {
				t.Errorf("requests outside the narrow grant: %v", denied)
			}
			if tc.name == "Destroy" {
				return // the namespace (and the binding with it) is gone
			}
			if _, err := f.cs.RbacV1().RoleBindings("muster-agent-old").Get(context.Background(),
				NamespaceRoleBindingName, metav1.GetOptions{}); err != nil {
				t.Errorf("no RoleBinding after %s: %v", tc.name, err)
			}
		})
	}
}

// (d) The propagation window is absorbed by a bounded probe: it retries while
// 403, and gives up at the bound naming the ClusterRole — having written no
// Secret and no Deployment.
func TestTheRoleBindingPropagationWaitIsBounded(t *testing.T) {
	t.Run("absorbed", func(t *testing.T) {
		shortPropagation(t, 2*time.Second)
		f := newRBACFake(t)
		f.lag = 5
		d := bindingDriver(t, f.cs)
		if err := d.Create(context.Background(), isolatedSpec("alpha")); err != nil {
			t.Fatalf("Create across a %d-request propagation lag: %v", f.lag, err)
		}
		probes := 0
		for _, a := range f.cs.Actions() {
			if a.GetVerb() == "get" && a.GetResource().Resource == "serviceaccounts" {
				probes++
			}
		}
		// lag refusals, then one success; apply's own SA upsert does no get on
		// the create path.
		if probes != f.lag+1 {
			t.Errorf("probes = %d, want %d", probes, f.lag+1)
		}
	})
	t.Run("never takes effect", func(t *testing.T) {
		shortPropagation(t, 50*time.Millisecond)
		f := newRBACFake(t)
		f.lag = 1 << 30
		d := bindingDriver(t, f.cs)
		start := time.Now()
		err := d.Create(context.Background(), isolatedSpec("alpha"))
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("Create took %v: the wait is not bounded", elapsed)
		}
		if err == nil || !errors.Is(err, provision.ErrBlind) || !strings.Contains(err.Error(), testClusterRole) ||
			!strings.Contains(err.Error(), "did not take effect") {
			t.Fatalf("Create = %v, want ErrBlind naming the ClusterRole and the propagation failure", err)
		}
		for _, a := range f.cs.Actions() {
			if a.GetVerb() == "create" {
				switch a.GetResource().Resource {
				case "namespaces", "rolebindings":
				default:
					t.Errorf("%s was created although the binding never took effect", a.GetResource().Resource)
				}
			}
		}
	})
	t.Run("bounded by ctx", func(t *testing.T) {
		shortPropagation(t, time.Hour)
		f := newRBACFake(t)
		f.lag = 1 << 30
		d := bindingDriver(t, f.cs)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		// blind() formats with %v (see its comment), so the deadline is in the
		// text, not the chain.
		if err := d.Create(ctx, isolatedSpec("alpha")); err == nil || !errors.Is(err, provision.ErrBlind) ||
			!strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
			t.Fatalf("Create = %v, want ErrBlind carrying the ctx deadline", err)
		}
	})
}

// A namespace muster does not own is never bound into, and a namespace under
// another prefix is never listed into.
func TestMusterNeverBindsItselfIntoANamespaceItDidNotCreate(t *testing.T) {
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "muster-agent-alpha",
		Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}}}
	otherInstall := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere-beta",
		Labels: instanceLabels("beta")}}
	// An installation whose prefix NESTS under this one's ("muster-agent-staging-"
	// under "muster-agent-"), for its instance "beta": a prefix match admits it.
	nested := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "muster-agent-staging-beta",
		Labels: instanceLabels("beta")}}
	// Muster-labelled, under the prefix, but with no instance label.
	unlabelled := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "muster-agent-gamma",
		Labels: map[string]string{labelManagedBy: managedBy, labelName: nameValue}}}
	f := newRBACFake(t, foreign, otherInstall, nested, unlabelled)
	d := bindingDriver(t, f.cs)
	ctx := context.Background()

	if err := d.Create(ctx, isolatedSpec("alpha")); !errors.Is(err, provision.ErrNotManaged) {
		t.Errorf("Create into a foreign namespace = %v, want ErrNotManaged", err)
	}
	if _, err := d.Get(ctx, provision.Ref{Name: "alpha"}); !errors.Is(err, provision.ErrNotFound) {
		t.Errorf("Get in a foreign namespace = %v, want ErrNotFound", err)
	}
	if err := d.Destroy(ctx, provision.Ref{Name: "alpha"}); !errors.Is(err, provision.ErrNotManaged) {
		t.Errorf("Destroy of a foreign namespace = %v, want ErrNotManaged", err)
	}
	if got, err := d.List(ctx); err != nil || len(got) != 0 {
		t.Errorf("List = %v, %v; want empty, no error", got, err)
	}
	for _, a := range f.cs.Actions() {
		if a.GetResource().Resource == "rolebindings" && a.GetVerb() == "create" {
			t.Errorf("a RoleBinding was created in %q", a.GetNamespace())
		}
	}
}

// A co-named RoleBinding muster did not write is refused, never adopted; one
// of muster's with a stale roleRef or subject is refused too, never replaced
// (the narrow grant holds no delete on rolebindings).
func TestTheRoleBindingIsOwnershipCheckedAndCorrected(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "muster-agent-alpha", Labels: instanceLabels("alpha")}}
	t.Run("foreign", func(t *testing.T) {
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: NamespaceRoleBindingName, Namespace: ns.Name},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"}}
		cs := fake.NewClientset(ns.DeepCopy(), rb)
		d := bindingDriver(t, cs)
		err := d.Create(context.Background(), isolatedSpec("alpha"))
		if !errors.Is(err, provision.ErrNotManaged) || errors.Is(err, provision.ErrBlind) {
			t.Fatalf("Create = %v, want ErrNotManaged (not ErrBlind)", err)
		}
		got, _ := cs.RbacV1().RoleBindings(ns.Name).Get(context.Background(), NamespaceRoleBindingName, metav1.GetOptions{})
		if got.RoleRef.Name != "cluster-admin" {
			t.Errorf("the foreign binding was changed: %+v", got.RoleRef)
		}
	})
	for _, tc := range []struct {
		name     string
		roleRef  string
		subjects []rbacv1.Subject
	}{
		{"stale roleRef", "an-older-role",
			[]rbacv1.Subject{{Kind: "ServiceAccount", Name: testSAName, Namespace: testSANamespace}}},
		{"stale subject", testClusterRole,
			[]rbacv1.Subject{{Kind: "ServiceAccount", Name: "an-older-sa", Namespace: testSANamespace}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: NamespaceRoleBindingName, Namespace: ns.Name,
				Labels: instanceLabels("alpha")},
				RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: tc.roleRef},
				Subjects: tc.subjects}
			f := newRBACFake(t, ns.DeepCopy(), rb)
			d := bindingDriver(t, f.cs)
			for _, op := range []struct {
				name string
				do   func() error
			}{
				{"Get", func() error { _, err := d.Get(context.Background(), provision.Ref{Name: "alpha"}); return err }},
				{"Create", func() error { return d.Create(context.Background(), isolatedSpec("alpha")) }},
			} {
				err := op.do()
				if !errors.Is(err, provision.ErrDivergentSpec) || errors.Is(err, provision.ErrBlind) ||
					!strings.Contains(err.Error(), "delete the binding by hand") {
					t.Fatalf("%s = %v, want ErrDivergentSpec (not ErrBlind) naming the by-hand fix", op.name, err)
				}
			}
			got, err := f.cs.RbacV1().RoleBindings(ns.Name).Get(context.Background(), NamespaceRoleBindingName, metav1.GetOptions{})
			if err != nil || got.RoleRef.Name != tc.roleRef || got.Subjects[0].Name != tc.subjects[0].Name {
				t.Fatalf("binding = %+v, %v; want it left exactly as it was", got, err)
			}
			for _, a := range f.cs.Actions() {
				if a.GetResource().Resource == "rolebindings" && a.GetVerb() != "get" {
					t.Errorf("unexpected %s on rolebindings: a stale binding is refused, not replaced", a.GetVerb())
				}
				if a.GetVerb() == "create" {
					t.Errorf("%s created despite the refusal", a.GetResource().Resource)
				}
			}
		})
	}
}

// One agent namespace whose binding muster refuses to use must not blind List
// for the fleet: the healthy instance is listed as usual, the refused one is
// REPORTED (PhaseUnknown, the refusal as its Reason) rather than skipped — an
// observed list without it would read as "not running", which the
// stopped-agent reaper acts on — and List returns no error.
func TestOneRefusedRoleBindingDoesNotBlindListForTheFleet(t *testing.T) {
	for _, tc := range []struct {
		name string
		rb   *rbacv1.RoleBinding
		want error
	}{
		{"divergent", &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: NamespaceRoleBindingName,
			Namespace: "muster-agent-beta", Labels: instanceLabels("beta")},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "an-older-role"},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: testSAName, Namespace: testSANamespace}}},
			provision.ErrDivergentSpec},
		{"foreign", &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: NamespaceRoleBindingName,
			Namespace: "muster-agent-beta"},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"}},
			provision.ErrNotManaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortPropagation(t, time.Second)
			legacy, plain := perInstanceDriver(t)
			for _, n := range []string{"alpha", "beta"} {
				if err := legacy.Create(context.Background(), isolatedSpec(n)); err != nil {
					t.Fatalf("legacy Create %s: %v", n, err)
				}
			}
			if _, err := plain.RbacV1().RoleBindings("muster-agent-beta").Create(context.Background(), tc.rb,
				metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			f := enforceRBAC(plain)
			d := bindingDriver(t, f.cs)

			got, err := d.List(context.Background())
			if err != nil {
				t.Fatalf("List = %v, want no error: one refused namespace must not fail the fleet's list", err)
			}
			byName := map[string]provision.Instance{}
			for _, inst := range got {
				byName[inst.Ref.Name] = inst
			}
			if len(got) != 2 {
				t.Fatalf("List = %+v, want exactly alpha and beta", got)
			}
			if a, ok := byName["alpha"]; !ok || a.Phase == provision.PhaseUnknown {
				t.Errorf("alpha = %+v, want listed from its Deployment", a)
			}
			b, ok := byName["beta"]
			if !ok || b.Phase != provision.PhaseUnknown || b.Group != "muster-agent-beta" ||
				!strings.Contains(b.Reason, "RoleBindingRefused") {
				t.Errorf("beta = %+v, want reported as PhaseUnknown in muster-agent-beta with the refusal as its Reason", b)
			}
			// The seam the reaper reads: a stored-stopped agent over this entry
			// must NOT compute as stopped (it would be due for destruction).
			if st := agents.ComputeStatus(agents.Agent{Name: "beta", Status: agents.StatusStopped}, &b); st == agents.StatusStopped {
				t.Errorf("ComputeStatus over the refused entry = %q: the reaper would treat a live agent as stopped", st)
			}
			// Get of THAT agent still returns the refusal itself.
			if _, err := d.Get(context.Background(), provision.Ref{Name: "beta"}); !errors.Is(err, tc.want) {
				t.Errorf("Get beta = %v, want %v", err, tc.want)
			}
			if denied := f.deniedList(); len(denied) != 0 {
				t.Errorf("requests outside the narrow grant: %v", denied)
			}
		})
	}
}

func TestTheLayoutIsRefusedWithTheSharedNamespace(t *testing.T) {
	_, err := New(Config{Client: fake.NewClientset(), Namespace: "agents", NamespaceRoleBinding: testBindingConfig()})
	if err == nil || !strings.Contains(err.Error(), "NamespacePerInstance") {
		t.Fatalf("New = %v, want a refusal naming NamespacePerInstance", err)
	}
	_, err = New(Config{Client: fake.NewClientset(), NamespacePerInstance: true,
		NamespaceRoleBinding: &NamespaceRoleBindingConfig{ClusterRole: "x"}})
	if err == nil || !strings.Contains(err.Error(), "ServiceAccountNamespace") {
		t.Fatalf("New = %v, want a refusal naming the missing subject", err)
	}
}
