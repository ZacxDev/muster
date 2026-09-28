package agentprivilege_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/agentprivilege"
	"github.com/ZacxDev/muster/internal/privilege"
	"github.com/ZacxDev/muster/internal/provision"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// ---------------------------------------------------------------------------
// WHAT THESE TESTS ARE FOR, AND WHAT THEY DELIBERATELY DO NOT STUB.
//
// 🔴 EVERY BEHAVIOURAL CASE HERE RUNS THE WHOLE SEAM: the applier, the
// provision.Grant chokepoint, the real kubernetes driver, and a fake clientset
// whose objects are then read back. A stub driver would have made each case
// pass while proving nothing about the ONE thing this package does — translate a
// stored profile into a payload some other package decodes STRICTLY. A payload
// with a mistyped field name round-trips through a stub perfectly and is REFUSED
// by the real decoder, which is the defect shape this package is most exposed
// to.
//
// ⚠ WHAT THE FAKE CLIENTSET STRUCTURALLY CANNOT SEE, stated because a green run
// here is not a claim about a cluster: admission, the `escalate`/`bind` verbs
// (a fake authorises everything), and therefore the single most likely
// production failure of this tier. That is named in cmd/muster-server's boot
// banner and in provisioner.go's prerequisite 1, not covered here.
// ---------------------------------------------------------------------------

const (
	// agentName is DNS-label-safe and deliberately shares no substring with the
	// profile name: PolicyObjectName joins both into one string, so fixtures that
	// overlap cannot distinguish a correct join from a wrong one.
	agentName   = "worker-seven"
	profileName = "cluster-triage"
	// storedNamespace is what an agents row carries (agents.NamespaceFor's shape).
	// The driver below is configured per-instance with the default "muster-"
	// prefix, so the namespace it actually uses is DIFFERENT — which is what makes
	// TestTheCallersNamespaceArgumentCannotRelocateTheRBAC falsifiable.
	storedNamespace = "devpod-worker-seven"
	driverNamespace = "muster-worker-seven"
)

// rbacProfile is the fixture. Every string is distinct from every other, and
// none of them equals a value the driver or this package could produce on its
// own — so an assertion cannot be satisfied by a constant.
func rbacProfile() privilege.Profile {
	return privilege.Profile{
		ID:   41,
		Name: profileName,
		Spec: privilege.Spec{
			ClusterRules: []privilege.PolicyRule{{
				APIGroups: []string{"apps"},
				Resources: []string{"statefulsets"},
				Verbs:     []string{"get", "watch"},
			}},
			NamespaceRules: []privilege.PolicyRule{{
				APIGroups:     []string{"batch"},
				Resources:     []string{"cronjobs"},
				Verbs:         []string{"patch"},
				ResourceNames: []string{"nightly-sweep"},
			}},
			// Env is present on purpose: it is the half this tier does NOT apply,
			// and a translation that leaked it into the rules payload would be
			// refused by the driver's DisallowUnknownFields decode.
			Env: []privilege.EnvVar{{Name: "EXAMPLE_FLAG", Value: "on"}},
		},
	}
}

// newK8sApplier builds the applier over a real kubernetes driver on a fake
// clientset, with an instance already created so its ServiceAccount exists and
// is muster-owned (the driver's Grant refuses otherwise, by design).
func newK8sApplier(t *testing.T) (*agentprivilege.Applier, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	d, err := k8sdriver.New(k8sdriver.Config{Client: cs, NamespacePerInstance: true})
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	if err := d.Create(context.Background(), provisiontest.MinimalSpec(agentName)); err != nil {
		t.Fatalf("premise: creating the instance: %v", err)
	}
	a, err := agentprivilege.New(agentprivilege.Config{Driver: d})
	if err != nil {
		t.Fatalf("agentprivilege.New: %v", err)
	}
	return a, cs
}

// TestNewRefusesAnApplierWithNoDriver.
//
// 🔴 IT IS NOT A NIL-CHECK NIT. cmd/muster-server assigns the CONCRETE pointer
// this constructor returns into an interface field, and api.Extensions.defects
// reads that field's nil-ness as "can this server apply what it records". An
// Applier built over no driver is a NON-nil interface that answers the readiness
// question YES and then nil-derefs on the first grant — fail-closed turned
// fail-open. Mutation: delete the check in New and this goes red on the error
// being nil.
func TestNewRefusesAnApplierWithNoDriver(t *testing.T) {
	a, err := agentprivilege.New(agentprivilege.Config{})
	if err == nil {
		t.Fatalf("New accepted a nil Driver and returned %#v", a)
	}
	if a != nil {
		t.Errorf("New returned a non-nil applier alongside its error: %#v", a)
	}
	for _, want := range []string{"Driver", "required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestApplyGrantAppliesBothRuleScopesToTheAgentsServiceAccount is the
// behavioural case, and every expectation in it is a LITERAL taken from the
// fixture rather than re-derived from the code under test.
//
// 🔴 IT ASSERTS THE RULE CONTENT FIELD BY FIELD, NOT "one rule exists". The
// translation from privilege.PolicyRule to rbacv1.PolicyRule is four assignments
// and a swap between any two of them compiles, type-checks, and grants the wrong
// access — verbs where resources should be is the version that grants everything
// on nothing, and apiGroups/resources swapped is the version that grants nothing
// while reporting success.
func TestApplyGrantAppliesBothRuleScopesToTheAgentsServiceAccount(t *testing.T) {
	a, cs := newK8sApplier(t)
	ctx := context.Background()

	if err := a.ApplyGrant(ctx, agentName, storedNamespace, rbacProfile()); err != nil {
		t.Fatalf("ApplyGrant: %v", err)
	}

	name := k8sdriver.PolicyObjectName(agentName, profileName)

	cr, err := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("clusterrole %q: %v", name, err)
	}
	wantCluster := []rbacv1.PolicyRule{{
		APIGroups: []string{"apps"},
		Resources: []string{"statefulsets"},
		Verbs:     []string{"get", "watch"},
	}}
	if !samePolicyRules(cr.Rules, wantCluster) {
		t.Errorf("clusterrole rules are %+v, want %+v", cr.Rules, wantCluster)
	}

	crb, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("clusterrolebinding %q: %v", name, err)
	}
	wantSubject := rbacv1.Subject{
		Kind:      "ServiceAccount",
		Name:      agentName,
		Namespace: driverNamespace,
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0] != wantSubject {
		t.Errorf("binding subjects are %+v, want exactly [%+v]", crb.Subjects, wantSubject)
	}
	if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != name {
		t.Errorf("binding roleRef is %+v, want a ClusterRole named %q", crb.RoleRef, name)
	}

	role, err := cs.RbacV1().Roles(driverNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("role %q in %q: %v", name, driverNamespace, err)
	}
	wantNamespaced := []rbacv1.PolicyRule{{
		APIGroups:     []string{"batch"},
		Resources:     []string{"cronjobs"},
		Verbs:         []string{"patch"},
		ResourceNames: []string{"nightly-sweep"},
	}}
	if !samePolicyRules(role.Rules, wantNamespaced) {
		t.Errorf("role rules are %+v, want %+v", role.Rules, wantNamespaced)
	}
	if _, err := cs.RbacV1().RoleBindings(driverNamespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("rolebinding %q in %q: %v", name, driverNamespace, err)
	}

	// 🔴 THE TWO SCOPES MUST NOT BE THE SAME RULE. Swapping ClusterRules and
	// NamespaceRules in the translation leaves both objects present with
	// plausible content, and every assertion above would still pass if the
	// fixture used one rule for both. This is the assertion that a swap is
	// visible at all.
	if samePolicyRules(cr.Rules, role.Rules) {
		t.Errorf("the ClusterRole and the Role carry the SAME rules (%+v); the two scopes "+
			"have been collapsed, and a swap between them would be invisible", cr.Rules)
	}
}

// TestApplyGrantIsIdempotent. The driver's contract requires it; this asserts
// the applier does not defeat it by, say, refusing an AlreadyExists.
func TestApplyGrantIsIdempotent(t *testing.T) {
	a, cs := newK8sApplier(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := a.ApplyGrant(ctx, agentName, storedNamespace, rbacProfile()); err != nil {
			t.Fatalf("ApplyGrant call %d: %v", i+1, err)
		}
	}
	name := k8sdriver.PolicyObjectName(agentName, profileName)
	crbs, err := cs.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list clusterrolebindings: %v", err)
	}
	if len(crbs.Items) != 1 || crbs.Items[0].Name != name {
		var got []string
		for i := range crbs.Items {
			got = append(got, crbs.Items[i].Name)
		}
		t.Errorf("two grants produced %d clusterrolebinding(s) %v, want exactly [%s]",
			len(crbs.Items), got, name)
	}
}

// TestTheCallersNamespaceArgumentCannotRelocateTheRBAC.
//
// 🔴 THE ARGUMENT IS A CLAIM FROM A DATABASE ROW AND THE DRIVER IS THE AUTHORITY.
// internal/api passes agents.Agent.Namespace, written by the HTTP handler from
// agents.NamespaceFor ("devpod-<name>"); where an instance actually lives is the
// driver's NamespacePerInstance/NamespacePrefix answer ("muster-<name>" here).
// agents.NamespacePrefix's own doc says those two subsystems must agree and that
// a disagreement is INVISIBLE. If this method honoured the argument, a stale row
// would decide where cluster RBAC lands: a Role bound in a namespace the pod is
// not in, with the grant reported applied.
//
// ⚠ IT PASSES A THIRD NAMESPACE, NOT THE ROW'S. Passing the row's value would
// make the case pass equally if the argument were used and happened to match
// nothing; a value that is neither the row's nor the driver's leaves exactly one
// namespace the Role may be in. Mutation: make ApplyGrant honour the argument
// (build a driver per call namespace, or pass it down) and the Role appears in
// `example-elsewhere` while this fails on both assertions.
func TestTheCallersNamespaceArgumentCannotRelocateTheRBAC(t *testing.T) {
	a, cs := newK8sApplier(t)
	ctx := context.Background()
	const wrongNamespace = "example-elsewhere"

	if err := a.ApplyGrant(ctx, agentName, wrongNamespace, rbacProfile()); err != nil {
		t.Fatalf("ApplyGrant with a wrong namespace argument: %v", err)
	}

	name := k8sdriver.PolicyObjectName(agentName, profileName)
	if _, err := cs.RbacV1().Roles(driverNamespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("the Role is NOT in the driver's namespace %q: %v", driverNamespace, err)
	}
	if _, err := cs.RbacV1().Roles(wrongNamespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Errorf("the Role landed in %q — the caller's namespace argument decided where cluster "+
			"RBAC went, so a stale agents row can bind a Role in a namespace the pod is not in",
			wrongNamespace)
	}
	// The binding's subject is the other half: it names a ServiceAccount BY
	// namespace, so honouring the argument would bind a ServiceAccount that does
	// not exist and grant nothing while reporting success.
	crb, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("clusterrolebinding: %v", err)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Namespace != driverNamespace {
		t.Errorf("binding subjects are %+v, want one whose namespace is %q", crb.Subjects, driverNamespace)
	}
}

// TestRemoveGrantDeletesWhatWasAppliedAndLeavesAStrangersObjectAlone.
//
// 🔴 THE SECOND HALF IS THE POINT AND IT IS NOT HYPOTHETICAL. The driver's own
// Revoke doc records that it once resolved four objects by DERIVED NAME and
// deleted them unconditionally — destroying a stranger's cluster-scoped objects
// and returning nil, in the one method a caller uses to withdraw a privilege.
// This asserts the applier reaches the ownership-checked path rather than any
// by-name shortcut of its own.
func TestRemoveGrantDeletesWhatWasAppliedAndLeavesAStrangersObjectAlone(t *testing.T) {
	a, cs := newK8sApplier(t)
	ctx := context.Background()

	// A stranger's ClusterRole under the name a DIFFERENT profile would derive.
	// Its labels are a plausible set that does not include muster's pair.
	const strangerProfile = "monitoring-readonly"
	strangerName := k8sdriver.PolicyObjectName(agentName, strangerProfile)
	if _, err := cs.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: strangerName,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "grafana",
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the stranger's clusterrole: %v", err)
	}

	if err := a.ApplyGrant(ctx, agentName, storedNamespace, rbacProfile()); err != nil {
		t.Fatalf("ApplyGrant: %v", err)
	}
	name := k8sdriver.PolicyObjectName(agentName, profileName)
	if _, err := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("premise: the clusterrole must exist before the revoke: %v", err)
	}

	if err := a.RemoveGrant(ctx, agentName, storedNamespace, profileName); err != nil {
		t.Fatalf("RemoveGrant: %v", err)
	}
	for _, check := range []struct {
		what string
		get  func() error
	}{
		{"clusterrole", func() error {
			_, e := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
			return e
		}},
		{"clusterrolebinding", func() error {
			_, e := cs.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{})
			return e
		}},
		{"role", func() error {
			_, e := cs.RbacV1().Roles(driverNamespace).Get(ctx, name, metav1.GetOptions{})
			return e
		}},
		{"rolebinding", func() error {
			_, e := cs.RbacV1().RoleBindings(driverNamespace).Get(ctx, name, metav1.GetOptions{})
			return e
		}},
	} {
		if err := check.get(); err == nil {
			t.Errorf("the %s survived RemoveGrant; the access it granted is still live", check.what)
		}
	}

	if _, err := cs.RbacV1().ClusterRoles().Get(ctx, strangerName, metav1.GetOptions{}); err != nil {
		t.Errorf("the stranger's ClusterRole %q was deleted by a revoke that was not about it: %v",
			strangerName, err)
	}
}

// TestRemoveGrantOfSomethingNeverGrantedSucceeds — absent is the state the
// caller asked for. It also covers internal/api's revoke path, which removes the
// RBAC before dropping the record and must not fail a revoke over a grant whose
// RBAC half was never applied (an env-only profile, or an unarmed deployment
// that was later armed).
func TestRemoveGrantOfSomethingNeverGrantedSucceeds(t *testing.T) {
	a, _ := newK8sApplier(t)
	if err := a.RemoveGrant(context.Background(), agentName, storedNamespace, "never-granted"); err != nil {
		t.Fatalf("RemoveGrant of an ungranted profile: %v", err)
	}
}

// TestApplyGrantRefusesADriverThatCannotApplyPolicy.
//
// 🔴 IT PINS THAT THIS PACKAGE GOES THROUGH provision.Grant RATHER THAN
// TYPE-ASSERTING PolicyGranter ITSELF. The noop driver implements no granter, so
// the chokepoint refuses with provision.ErrUnsupported and names the driver. An
// applier that asserted the interface and skipped the assertion's failure — or
// that skipped the Capabilities.Policy half — would return nil here, and
// internal/api's grantProfile records the grant on a nil error. That is the
// "granted chip over a ServiceAccount with none of the permissions" the whole
// readiness defect exists to prevent, reached from inside the tier meant to
// prevent it.
func TestApplyGrantRefusesADriverThatCannotApplyPolicy(t *testing.T) {
	noop, err := provision.NewNoop()
	if err != nil {
		t.Fatalf("NewNoop: %v", err)
	}
	a, err := agentprivilege.New(agentprivilege.Config{Driver: noop})
	if err != nil {
		t.Fatalf("agentprivilege.New: %v", err)
	}
	err = a.ApplyGrant(context.Background(), agentName, storedNamespace, rbacProfile())
	if err == nil {
		t.Fatal("ApplyGrant returned nil over a driver that cannot apply policy; the caller " +
			"records the grant on a nil error")
	}
	if !errors.Is(err, provision.ErrUnsupported) {
		t.Errorf("the refusal is not provision.ErrUnsupported, so a caller cannot tell a "+
			"permanent build property from a transient fault: %v", err)
	}
	if !strings.Contains(err.Error(), profileName) {
		t.Errorf("the refusal does not name the profile it refused (%q): %v", profileName, err)
	}
}

// TestRemoveGrantOverADriverThatCannotGrantIsANoop — the deliberate asymmetry
// with the test above, restated as a guard because the pair reads as
// inconsistent and the next reader's instinct is to "fix" it. provision.Revoke
// owns the rule: a driver that cannot grant cannot have granted.
func TestRemoveGrantOverADriverThatCannotGrantIsANoop(t *testing.T) {
	noop, err := provision.NewNoop()
	if err != nil {
		t.Fatalf("NewNoop: %v", err)
	}
	a, err := agentprivilege.New(agentprivilege.Config{Driver: noop})
	if err != nil {
		t.Fatalf("agentprivilege.New: %v", err)
	}
	if err := a.RemoveGrant(context.Background(), agentName, storedNamespace, profileName); err != nil {
		t.Fatalf("RemoveGrant over a non-granting driver must be a no-op, got %v", err)
	}
}

// TestApplyGrantOfAProfileWithNoRulesIsRefusedRatherThanReportedApplied.
//
// ⚠ IT IS AN INVARIANT GUARD, NOT A REGRESSION TEST, AND IS LABELLED AS ONE. No
// caller reaches this: both grant paths in internal/api gate on
// privilege.Spec.HasRBAC first. It is here because agentprivilege deliberately
// adds NO no-rules check of its own — internal/provision/k8s's policy.go records
// what two layers refusing one rule costs — so this asserts the refusal it
// relies on actually fires, rather than assuming it.
func TestApplyGrantOfAProfileWithNoRulesIsRefusedRatherThanReportedApplied(t *testing.T) {
	a, cs := newK8sApplier(t)
	envOnly := privilege.Profile{
		Name: "env-only",
		Spec: privilege.Spec{Env: []privilege.EnvVar{{Name: "EXAMPLE_FLAG", Value: "on"}}},
	}
	err := a.ApplyGrant(context.Background(), agentName, storedNamespace, envOnly)
	if err == nil {
		t.Fatal("ApplyGrant returned nil for a profile carrying no RBAC at all")
	}
	if !errors.Is(err, provision.ErrUnsupported) {
		t.Errorf("want provision.ErrUnsupported, got %v", err)
	}
	crs, lerr := cs.RbacV1().ClusterRoles().List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatalf("list clusterroles: %v", lerr)
	}
	if len(crs.Items) != 0 {
		t.Errorf("the refused grant still wrote %d clusterrole(s)", len(crs.Items))
	}
}

// TestTheRuleTranslationIsFieldForField is the SHAPE guard for the one thing
// this package computes, reached without the driver.
//
// 🔴 IT EXISTS BECAUSE privilege.PolicyRule IS A SUBSET OF rbacv1.PolicyRule AND
// THE TWO ARE NOT INTERCHANGEABLE. The stored type has no NonResourceURLs. A
// translation written as a type conversion, or one that gained a field on one
// side only, compiles — and the observable is a rule the apiserver accepts with
// one component silently missing. Each of the four fields carries a value that
// appears nowhere else in this file, so a cross-assignment between any two of
// them is visible here and in exactly one place.
//
// ⚠ IT READS THE RESULT THROUGH THE DRIVER'S STRICT DECODER, WHICH IS THE
// SECOND HALF OF THE CLAIM. internal/provision/k8s decodes the rules payload
// with DisallowUnknownFields, so a field name this package encodes that the
// driver does not know is a REFUSAL rather than a silently dropped rule — and a
// test that inspected only this package's own struct could not see it.
func TestTheRuleTranslationIsFieldForField(t *testing.T) {
	a, cs := newK8sApplier(t)
	ctx := context.Background()
	prof := privilege.Profile{
		Name: "field-probe",
		Spec: privilege.Spec{
			ClusterRules: []privilege.PolicyRule{{
				APIGroups:     []string{"example.io"},
				Resources:     []string{"widgets"},
				Verbs:         []string{"deletecollection"},
				ResourceNames: []string{"only-this-one"},
			}},
		},
	}
	if err := a.ApplyGrant(ctx, agentName, storedNamespace, prof); err != nil {
		t.Fatalf("ApplyGrant: %v", err)
	}
	cr, err := cs.RbacV1().ClusterRoles().Get(ctx,
		k8sdriver.PolicyObjectName(agentName, "field-probe"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("clusterrole: %v", err)
	}
	want := []rbacv1.PolicyRule{{
		APIGroups:     []string{"example.io"},
		Resources:     []string{"widgets"},
		Verbs:         []string{"deletecollection"},
		ResourceNames: []string{"only-this-one"},
	}}
	if !samePolicyRules(cr.Rules, want) {
		t.Errorf("rules are %+v, want %+v", cr.Rules, want)
	}
}

// TestTheApplierReportsTheDriverItAppliesThrough — the boot banner reads this to
// say which backend is in force, and the banner's whole value is that the value
// an operator SET and the object that got BUILT are separate claims.
func TestTheApplierReportsTheDriverItAppliesThrough(t *testing.T) {
	a, _ := newK8sApplier(t)
	if got := a.Driver(); got != "kubernetes" {
		t.Errorf("Driver() = %q over the kubernetes driver", got)
	}
	noop, err := provision.NewNoop()
	if err != nil {
		t.Fatalf("NewNoop: %v", err)
	}
	n, err := agentprivilege.New(agentprivilege.Config{Driver: noop})
	if err != nil {
		t.Fatalf("agentprivilege.New: %v", err)
	}
	if got := n.Driver(); got != "noop" {
		t.Errorf("Driver() = %q over the noop driver", got)
	}
}

// samePolicyRules compares rule slices field by field.
//
// ⚠ IT IS SPELLED OUT RATHER THAN reflect.DeepEqual BECAUSE DeepEqual
// DISTINGUISHES nil FROM AN EMPTY SLICE, and a round trip through JSON does not
// preserve that for an omitempty field. A comparison that failed on
// nil-versus-[] would be a test about encoding trivia reporting itself as an
// access defect.
func samePolicyRules(got, want []rbacv1.PolicyRule) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !sameStrings(got[i].APIGroups, want[i].APIGroups) ||
			!sameStrings(got[i].Resources, want[i].Resources) ||
			!sameStrings(got[i].Verbs, want[i].Verbs) ||
			!sameStrings(got[i].ResourceNames, want[i].ResourceNames) ||
			!sameStrings(got[i].NonResourceURLs, want[i].NonResourceURLs) {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
