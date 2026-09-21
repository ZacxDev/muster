package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/provision"
)

// Rules is the shape this driver understands inside provision.Policy.Rules.
//
// It is the ONLY shape it understands, and anything else is REFUSED rather
// than partially applied — see Grant.
type Rules struct {
	// ClusterRules become a ClusterRole bound to the instance's ServiceAccount.
	ClusterRules []rbacv1.PolicyRule `json:"clusterRules,omitempty"`
	// NamespaceRules become a Role in the instance's own namespace.
	NamespaceRules []rbacv1.PolicyRule `json:"namespaceRules,omitempty"`
}

const (
	labelPolicy   = "muster.dev/policy"
	labelSubject  = "muster.dev/policy-subject"
	policyManaged = "muster.dev/policy-managed"
)

// policyObjectName is the deterministic name of the RBAC objects for one
// (instance, policy) pair. Deterministic so Revoke can find them without a
// lookup table, and so a second Grant is an update rather than a duplicate.
func policyObjectName(instance, policy string) string {
	return "muster-" + instance + "-" + policy
}

func policyLabels(instance, policy string) map[string]string {
	return map[string]string{
		labelManagedBy: managedBy,
		policyManaged:  "true",
		labelSubject:   instance,
		labelPolicy:    policy,
	}
}

// Grant implements provision.PolicyGranter.
//
// 🔴 A POLICY THIS DRIVER CANNOT INTERPRET IS REFUSED, NOT IGNORED. That is the
// whole reason Rules is opaque at the core and typed here: a grant recorded as
// applied, whose rules nobody read, is a security defect that READS AS
// COVERAGE. Every refusal below returns provision.ErrUnsupported with a reason
// a user interface can show and a machine endpoint can return as a 409.
//
// ⚠ NAMED LIMIT OF THIS DRIVER: it applies Rules ONLY. A policy carrying Env or
// Files is refused, because applying them means rolling the instance's pod and
// this driver does not do that from the grant path. That limit is enforced
// here rather than documented and forgotten — refusing is the behaviour the
// contract demands of anything a driver cannot do.
func (d *Driver) Grant(ctx context.Context, ref provision.Ref, pol provision.Policy) error {
	if d.cfg.PolicyDisabled {
		return fmt.Errorf("%w: this driver is configured with PolicyDisabled, so policy %q was NOT granted",
			provision.ErrUnsupported, pol.Name)
	}
	if pol.Name == "" {
		return fmt.Errorf("%w: a policy must be named; Revoke finds its objects by name", provision.ErrInvalidSpec)
	}
	if len(pol.Env) > 0 || len(pol.Files) > 0 {
		return fmt.Errorf("%w: the kubernetes driver applies authorisation rules only, and policy %q carries "+
			"%d env var(s) and %d file(s); applying those means rolling the pod, which this driver does not do "+
			"from the grant path",
			provision.ErrUnsupported, pol.Name, len(pol.Env), len(pol.Files))
	}
	if !pol.HasRules() {
		return fmt.Errorf("%w: policy %q carries no rules this driver can apply; granting it would record "+
			"access that was never given", provision.ErrUnsupported, pol.Name)
	}

	rules, err := decodeRules(pol.Rules)
	if err != nil {
		return fmt.Errorf("%w: the kubernetes driver cannot interpret policy %q: %v",
			provision.ErrUnsupported, pol.Name, err)
	}
	if len(rules.ClusterRules) == 0 && len(rules.NamespaceRules) == 0 {
		return fmt.Errorf("%w: policy %q parsed but declares neither clusterRules nor namespaceRules",
			provision.ErrUnsupported, pol.Name)
	}

	ns := d.namespaceFor(ref.Name)
	// The instance must exist: granting access to a ServiceAccount that is not
	// there creates a binding whose subject a FUTURE namesake would inherit.
	if _, err := d.cfg.Client.CoreV1().ServiceAccounts(ns).Get(ctx, ref.Name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return notFound(ref.Name)
		}
		return blind("get serviceaccount "+ref.Name, err)
	}

	name := policyObjectName(ref.Name, pol.Name)
	labels := policyLabels(ref.Name, pol.Name)
	subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: ref.Name, Namespace: ns}

	if len(rules.ClusterRules) > 0 {
		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Rules:      rules.ClusterRules,
		}
		if err := upsert(ctx,
			func() error {
				_, e := d.cfg.Client.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
				return e
			},
			func() error {
				_, e := d.cfg.Client.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{})
				return e
			}); err != nil {
			return blind("apply clusterrole "+name, err)
		}
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
			Subjects:   []rbacv1.Subject{subject},
		}
		// ⚠ CREATE-ONLY, AND NOT A SHORTCUT. RoleRef is immutable on a binding,
		// so an Update that changes it is rejected; and because both the name
		// and the subject are deterministic, an existing binding is already the
		// one we want.
		if _, err := d.cfg.Client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return blind("create clusterrolebinding "+name, err)
		}
	}

	if len(rules.NamespaceRules) > 0 {
		r := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Rules:      rules.NamespaceRules,
		}
		if err := upsert(ctx,
			func() error {
				_, e := d.cfg.Client.RbacV1().Roles(ns).Create(ctx, r, metav1.CreateOptions{})
				return e
			},
			func() error {
				_, e := d.cfg.Client.RbacV1().Roles(ns).Update(ctx, r, metav1.UpdateOptions{})
				return e
			}); err != nil {
			return blind("apply role "+name, err)
		}
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects:   []rbacv1.Subject{subject},
		}
		if _, err := d.cfg.Client.RbacV1().RoleBindings(ns).Create(ctx, rb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return blind("create rolebinding "+name, err)
		}
	}
	return nil
}

// decodeRules parses the opaque payload STRICTLY.
//
// 🔴 DisallowUnknownFields IS THE POINT. A policy written for another driver —
// or a newer version of this one — must be REFUSED, not silently applied minus
// the parts this build does not know about. A lenient parser turns "muster does
// not understand your policy" into "muster applied some of your policy", which
// is exactly the state that reads as coverage.
func decodeRules(raw json.RawMessage) (Rules, error) {
	var out Rules
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Rules{}, err
	}
	return out, nil
}

// Revoke implements provision.PolicyGranter. Every object is attempted; a
// missing one is success, because absent is the state the caller asked for.
func (d *Driver) Revoke(ctx context.Context, ref provision.Ref, policyName string) error {
	ns := d.namespaceFor(ref.Name)
	name := policyObjectName(ref.Name, policyName)
	c := d.cfg.Client

	var firstErr error
	del := func(what string, err error) {
		if err == nil || apierrors.IsNotFound(err) {
			return
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("delete %s %s: %w", what, name, err)
		}
	}
	del("clusterrolebinding", c.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{}))
	del("clusterrole", c.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{}))
	del("rolebinding", c.RbacV1().RoleBindings(ns).Delete(ctx, name, metav1.DeleteOptions{}))
	del("role", c.RbacV1().Roles(ns).Delete(ctx, name, metav1.DeleteOptions{}))
	return firstErr
}

// revokeAllClusterPolicies removes every cluster-scoped policy object this
// driver created for ref, whatever it was called.
//
// 🔴 IT ENUMERATES BY LABEL RATHER THAN BY A LIST OF GRANTED NAMES, AND THAT IS
// DELIBERATE. The obvious implementation asks the caller which policies were
// granted — but by the time a teardown runs, the record of the grant is
// usually already gone (a cascading delete), so the list comes back EMPTY and
// the teardown removes nothing while reporting success. The cluster is the
// authority on what exists in the cluster.
func (d *Driver) revokeAllClusterPolicies(ctx context.Context, ref provision.Ref) error {
	sel := policyManaged + "=true," + labelSubject + "=" + ref.Name
	c := d.cfg.Client

	bindings, err := c.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return blind("list clusterrolebindings for "+ref.Name, err)
	}
	var firstErr error
	for i := range bindings.Items {
		n := bindings.Items[i].Name
		if err := c.RbacV1().ClusterRoleBindings().Delete(ctx, n, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = fmt.Errorf("delete clusterrolebinding %s: %w", n, err)
		}
	}
	roles, err := c.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return blind("list clusterroles for "+ref.Name, err)
	}
	for i := range roles.Items {
		n := roles.Items[i].Name
		if err := c.RbacV1().ClusterRoles().Delete(ctx, n, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = fmt.Errorf("delete clusterrole %s: %w", n, err)
		}
	}
	return firstErr
}
