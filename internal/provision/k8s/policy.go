package k8s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// PolicyObjectName is the deterministic name of the RBAC objects for one
// (instance, policy) pair. Deterministic so Revoke can find them without a
// lookup table, and so a second Grant is an update rather than a duplicate.
//
// 🔴 THE DIGEST IS WHAT MAKES THE PAIR RECOVERABLE FROM THE NAME, AND IT IS NOT
// DECORATION. Joining the two components with a `-` is AMBIGUOUS, because `-`
// occurs inside both: (`agent-a`, `ops`) and (`agent`, `a-ops`) produced the
// same string. Two instances then shared one ClusterRole — the second Grant
// rewrote its rules, while the ClusterRoleBinding, which is create-only, kept
// the FIRST instance's ServiceAccount as its subject. The first instance
// silently gained the second's access, the second got nothing, and Grant
// returned nil for both. The digest is over the pair with a NUL between, which
// no name component can contain, so distinct pairs cannot produce one name.
//
// The readable prefix is kept because an operator reading `kubectl get
// clusterrole` needs to know whose it is; the digest is what the code relies
// on.
func PolicyObjectName(instance, policy string) string {
	sum := sha256.Sum256([]byte(instance + "\x00" + policy))
	return "muster-" + instance + "-" + policy + "-" + hex.EncodeToString(sum[:8])
}

// policyLabels mark an RBAC object as this driver's, and as belonging to one
// (instance, policy) pair.
//
// ⚠ THEY INCLUDE managedLabels, so owned() is the SAME predicate here as for
// every other object this driver creates. The policy-specific keys are what
// revokeAllPolicies enumerates on; the shared pair is what says muster made it.
func policyLabels(instance, policy string) map[string]string {
	out := map[string]string{
		policyManaged: "true",
		labelSubject:  instance,
		labelPolicy:   policy,
	}
	for k, v := range managedLabels {
		out[k] = v
	}
	return out
}

// Grant implements provision.PolicyGranter.
//
// 🔴 A POLICY THIS DRIVER CANNOT INTERPRET IS REFUSED, NOT IGNORED. That is the
// whole reason Rules is opaque at the core and typed here: a grant recorded as
// applied, whose rules nobody read, is a security defect that READS AS
// COVERAGE. Every refusal below returns provision.ErrUnsupported with a reason
// a user interface can show and a machine endpoint can return as a 409.
//
// ⚠ NAMED LIMIT OF THIS DRIVER: it applies Rules ONLY, because applying a
// grant's env or files means rolling the instance's pod and this driver does
// not do that from the grant path. There is nothing to refuse here any more —
// provision.Policy no longer HAS those fields, so the limit is expressed by the
// type rather than by a check. It previously WAS a check, and that was a
// defect: this driver reports Capabilities.Files true, so provision.Grant's
// file guard passed and this one refused the same policy a layer lower. Two
// refusals for one rule, disagreeing about which layer owns it. When policy
// files return, the refusal goes in provision.Grant, once.
func (d *Driver) Grant(ctx context.Context, ref provision.Ref, pol provision.Policy) error {
	if d.cfg.PolicyDisabled {
		return fmt.Errorf("%w: this driver is configured with PolicyDisabled, so policy %q was NOT granted",
			provision.ErrUnsupported, pol.Name)
	}
	if pol.Name == "" {
		return fmt.Errorf("%w: a policy must be named; Revoke finds its objects by name", provision.ErrInvalidSpec)
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
	//
	// 🔴 AND IT MUST BE MUSTER'S. A name is not an identity here either: this
	// read was existence-only, so a ServiceAccount somebody else owns under the
	// instance's name satisfied it and the binding below then attached the
	// policy's rules to THEIR identity — muster escalating a stranger's
	// workload, and reporting the grant applied. The refusal is
	// ErrNotManaged-flavoured rather than ErrNotFound: Grant is an ACTION, and
	// "there is no such instance" is not what happened.
	sa, err := d.cfg.Client.CoreV1().ServiceAccounts(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return notFound(ref.Name)
		}
		return blind("get serviceaccount "+ref.Name, err)
	}
	if !owned(sa.Labels) {
		return fmt.Errorf("refusing to grant policy %q to %q: %w", pol.Name, ref.Name,
			notManagedAction("serviceaccount", ref.Name, ns, sa.Labels))
	}

	name := PolicyObjectName(ref.Name, pol.Name)
	labels := policyLabels(ref.Name, pol.Name)
	subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: ref.Name, Namespace: ns}

	if len(rules.ClusterRules) > 0 {
		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Rules:      rules.ClusterRules,
		}
		if err := d.upsertOwned("clusterrole", name, "",
			func() (map[string]string, error) {
				return labelsOf(d.cfg.Client.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{}))
			},
			func() error {
				_, e := d.cfg.Client.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
				return e
			},
			func() error {
				_, e := d.cfg.Client.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{})
				return e
			}); err != nil {
			return applyFailed("apply clusterrole "+name, err)
		}
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
			Subjects:   []rbacv1.Subject{subject},
		}
		// ⚠ CREATE-ONLY, AND NOT A SHORTCUT. RoleRef is immutable on a binding,
		// so an Update that changes it is rejected; and because both the name
		// and the subject are deterministic, an existing binding OF OURS is
		// already the one we want.
		//
		// 🔴 "OF OURS" IS THE PART THAT WAS MISSING. An AlreadyExists was
		// treated as success outright, so a stranger's co-named
		// ClusterRoleBinding — pointing at whatever ClusterRole and whatever
		// subjects they chose — made Grant return nil with the policy NOT
		// applied to this instance. createOwned is the same create-only
		// predicate apply's workspace claim uses.
		if err := d.createOwned("clusterrolebinding", name, "",
			func() (map[string]string, error) {
				return labelsOf(d.cfg.Client.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{}))
			},
			func() error {
				_, e := d.cfg.Client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
				return e
			}); err != nil {
			return applyFailed("apply clusterrolebinding "+name, err)
		}
	}

	if len(rules.NamespaceRules) > 0 {
		r := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Rules:      rules.NamespaceRules,
		}
		if err := d.upsertOwned("role", name, ns,
			func() (map[string]string, error) {
				return labelsOf(d.cfg.Client.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{}))
			},
			func() error {
				_, e := d.cfg.Client.RbacV1().Roles(ns).Create(ctx, r, metav1.CreateOptions{})
				return e
			},
			func() error {
				_, e := d.cfg.Client.RbacV1().Roles(ns).Update(ctx, r, metav1.UpdateOptions{})
				return e
			}); err != nil {
			return applyFailed("apply role "+name, err)
		}
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects:   []rbacv1.Subject{subject},
		}
		// Create-only for the same reason as the ClusterRoleBinding above, and
		// ownership-checked for the same reason.
		if err := d.createOwned("rolebinding", name, ns,
			func() (map[string]string, error) {
				return labelsOf(d.cfg.Client.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{}))
			},
			func() error {
				_, e := d.cfg.Client.RbacV1().RoleBindings(ns).Create(ctx, rb, metav1.CreateOptions{})
				return e
			}); err != nil {
			return applyFailed("apply rolebinding "+name, err)
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
//
// 🔴 EVERY DELETE IS OWNERSHIP-CHECKED, AND IT WAS NOT. This method resolved
// four objects by their DERIVED NAME and deleted them unconditionally, treating
// IsNotFound as success. Measured: with a stranger's co-named ClusterRole in the
// way it deleted that ClusterRole AND its cluster-wide ClusterRoleBinding and
// returned nil — the PR's own central invariant ("deleting by name alone
// destroys a stranger's objects and reports success"), live at CLUSTER SCOPE, in
// the one method a caller uses to withdraw a privilege. The discriminator was
// by-name versus by-label: revokeAllPolicies, which enumerates the same objects
// by LABEL, left the same stranger's object alone.
//
// ⚠ A FOREIGN OBJECT IS A LOGGED SKIP, NOT A REFUSAL, AND THAT IS THE SAME
// ANSWER revokeAllPolicies GIVES. These are satellites, not identity anchors
// (see deleteIfOwned): a label selector simply does not match a stranger's
// object, so a loud Revoke here would disagree with the teardown path about the
// same object. It is also coherent with Grant, which now REFUSES to write a
// policy object a stranger holds — so muster cannot have granted through one,
// and "the grant is absent" is the state the caller asked for.
func (d *Driver) Revoke(ctx context.Context, ref provision.Ref, policyName string) error {
	ns := d.namespaceFor(ref.Name)
	name := PolicyObjectName(ref.Name, policyName)
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
	del("clusterrolebinding", d.deleteIfOwned("clusterrolebinding", name,
		func() (map[string]string, error) {
			return labelsOf(c.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{}))
		},
		func() error { return c.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{}) }))
	del("clusterrole", d.deleteIfOwned("clusterrole", name,
		func() (map[string]string, error) {
			return labelsOf(c.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{}))
		},
		func() error { return c.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{}) }))
	del("rolebinding", d.deleteIfOwned("rolebinding", name,
		func() (map[string]string, error) {
			return labelsOf(c.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{}))
		},
		func() error { return c.RbacV1().RoleBindings(ns).Delete(ctx, name, metav1.DeleteOptions{}) }))
	del("role", d.deleteIfOwned("role", name,
		func() (map[string]string, error) {
			return labelsOf(c.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{}))
		},
		func() error { return c.RbacV1().Roles(ns).Delete(ctx, name, metav1.DeleteOptions{}) }))
	return firstErr
}

// revokeAllPolicies removes every policy object this driver created for ref,
// whatever it was called — cluster-scoped AND namespaced.
//
// 🔴 IT ENUMERATES BY LABEL RATHER THAN BY A LIST OF GRANTED NAMES, AND THAT IS
// DELIBERATE. The obvious implementation asks the caller which policies were
// granted — but by the time a teardown runs, the record of the grant is
// usually already gone (a cascading delete), so the list comes back EMPTY and
// the teardown removes nothing while reporting success. The cluster is the
// authority on what exists in the cluster.
//
// 🔴 THE NAMESPACED HALF IS NOT REDUNDANT WITH THE NAMESPACE DELETION. It was
// once, under the assumption that Destroy always deletes a namespace — which is
// true only when the driver owns one per instance. In a SHARED namespace
// nothing deletes the Role and the RoleBinding, so they survive the instance
// with a subject naming a ServiceAccount that Create will recreate verbatim for
// the next instance to take the name: it inherits access nobody granted it.
// 🔴 THE SELECTOR INCLUDES managedSelector(), so the set this enumerates is
// exactly the set owned() admits. It used to ask only for the two POLICY keys,
// which anybody can put on an object: a foreign object carrying
// muster.dev/policy-managed=true would have been enumerated and deleted here,
// while every by-name path in this driver refuses it. The server-side selector
// and the client-side predicate are one rule, and this is its server-side
// spelling — see managedLabels.
func (d *Driver) revokeAllPolicies(ctx context.Context, ref provision.Ref, ns string) error {
	sel := managedSelector() + "," + policyManaged + "=true," + labelSubject + "=" + ref.Name
	c := d.cfg.Client
	opts := metav1.ListOptions{LabelSelector: sel}

	var firstErr error
	note := func(err error) {
		if err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = err
		}
	}

	bindings, err := c.RbacV1().ClusterRoleBindings().List(ctx, opts)
	if err != nil {
		return blind("list clusterrolebindings for "+ref.Name, err)
	}
	for i := range bindings.Items {
		n := bindings.Items[i].Name
		note(wrapDelete("clusterrolebinding", n, c.RbacV1().ClusterRoleBindings().Delete(ctx, n, metav1.DeleteOptions{})))
	}
	roles, err := c.RbacV1().ClusterRoles().List(ctx, opts)
	if err != nil {
		return blind("list clusterroles for "+ref.Name, err)
	}
	for i := range roles.Items {
		n := roles.Items[i].Name
		note(wrapDelete("clusterrole", n, c.RbacV1().ClusterRoles().Delete(ctx, n, metav1.DeleteOptions{})))
	}

	nsBindings, err := c.RbacV1().RoleBindings(ns).List(ctx, opts)
	if err != nil {
		return blind("list rolebindings for "+ref.Name, err)
	}
	for i := range nsBindings.Items {
		n := nsBindings.Items[i].Name
		note(wrapDelete("rolebinding", n, c.RbacV1().RoleBindings(ns).Delete(ctx, n, metav1.DeleteOptions{})))
	}
	nsRoles, err := c.RbacV1().Roles(ns).List(ctx, opts)
	if err != nil {
		return blind("list roles for "+ref.Name, err)
	}
	for i := range nsRoles.Items {
		n := nsRoles.Items[i].Name
		note(wrapDelete("role", n, c.RbacV1().Roles(ns).Delete(ctx, n, metav1.DeleteOptions{})))
	}
	return firstErr
}

func wrapDelete(what, name string, err error) error {
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("delete %s %s: %w", what, name, err)
}
