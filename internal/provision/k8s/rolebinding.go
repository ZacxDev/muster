package k8s

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NamespaceRoleBindingConfig turns on the least-privilege RBAC layout: instead
// of holding cluster-wide rights on every object it creates, muster's
// ServiceAccount holds cluster-wide rights on NAMESPACES and on RoleBindings
// only, plus `bind` on ONE named ClusterRole, and grants itself that
// ClusterRole inside each per-instance namespace with a RoleBinding — written
// immediately after the namespace and before any other object in it.
//
// The cluster-wide grant a deployment then needs is
// NamespaceRoleBindingClusterRBAC; the ClusterRole named here holds the
// namespaced verbs (serviceaccounts, secrets, configmaps, services, PVCs,
// deployments, pods, pods/log, pods/exec, networkpolicies).
//
// 🔴 IT REQUIRES Config.NamespacePerInstance. In the shared layout there is no
// namespace muster creates, so there is nowhere for it to bind itself; the
// operator grants a Role in the shared namespace directly. New refuses the
// combination.
//
// ⚠ WHAT THE NARROW GRANT STILL ALLOWS, NAMED SO IT IS NOT READ AS MORE:
// `create` on rolebindings is cluster-wide (the namespaces are dynamic), so the
// ServiceAccount can bind ITSELF to ClusterRole inside ANY namespace — kube-system
// included — and so reach that namespace's Secrets in two deliberate, audited
// steps instead of one standing permission. Kubernetes RBAC cannot scope a
// grant by namespace-name pattern; closing that needs an admission policy that
// restricts this ServiceAccount's RoleBindings to namespaces under the prefix
// (docs/provisioner.md carries an example).
type NamespaceRoleBindingConfig struct {
	// ClusterRole is the ClusterRole bound inside each per-instance namespace.
	ClusterRole string
	// ServiceAccountNamespace and ServiceAccountName are muster's own
	// ServiceAccount — the RoleBinding's one subject.
	ServiceAccountNamespace string
	ServiceAccountName      string
}

func (c *NamespaceRoleBindingConfig) validate() error {
	for _, f := range []struct{ field, v string }{
		{"ClusterRole", c.ClusterRole},
		{"ServiceAccountNamespace", c.ServiceAccountNamespace},
		{"ServiceAccountName", c.ServiceAccountName},
	} {
		if strings.TrimSpace(f.v) == "" {
			return fmt.Errorf("k8s: NamespaceRoleBinding.%s is required", f.field)
		}
	}
	return nil
}

// NamespaceRoleBindingName is the name of the RoleBinding muster writes into
// each per-instance namespace.
const NamespaceRoleBindingName = "muster-provisioner"

// NamespaceRoleBindingClusterRBAC is the whole CLUSTER-WIDE grant the driver
// needs once NamespaceRoleBinding is set, as ClusterRole rules (resource ->
// verbs). `bind` is restricted by resourceNames to the configured ClusterRole.
//
//   - namespaces: get, list, create, delete — ensureNamespace, Destroy, and
//     List's enumeration of the per-instance namespaces;
//   - rolebindings.rbac.authorization.k8s.io: get, create, delete — the binding
//     itself (delete only to replace one whose roleRef or subject is no longer
//     the configured one; see ensureRoleBinding);
//   - clusterroles.rbac.authorization.k8s.io: bind, resourceNames [ClusterRole].
//
// TestTheNarrowClusterGrantCoversAnInstancesWholeLife measures it off an
// instance's whole life against an RBAC-enforcing fake.
var NamespaceRoleBindingClusterRBAC = map[string][]string{
	"namespaces":   {"get", "list", "create", "delete"},
	"rolebindings": {"get", "create", "delete"},
	"clusterroles": {"bind"},
}

// rbacPropagationTimeout bounds how long ensureRoleBinding waits for a new
// RoleBinding to take effect; rbacPropagationPoll is how often it probes.
// Vars so a test can shorten them.
var (
	rbacPropagationTimeout = 30 * time.Second
	rbacPropagationPoll    = 250 * time.Millisecond
)

// bindingMode reports whether the least-privilege layout is on.
func (d *Driver) bindingMode() bool {
	return d.cfg.NamespaceRoleBinding != nil && d.cfg.NamespacePerInstance
}

// renderRoleBinding renders the binding of muster's ServiceAccount to the
// configured ClusterRole inside ns. It carries instanceLabels, so owned()
// recognises it like every other object this driver writes.
func (d *Driver) renderRoleBinding(instance, ns string) *rbacv1.RoleBinding {
	rb := d.cfg.NamespaceRoleBinding
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NamespaceRoleBindingName,
			Namespace: ns,
			Labels:    instanceLabels(instance),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     rb.ClusterRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      rb.ServiceAccountName,
			Namespace: rb.ServiceAccountNamespace,
		}},
	}
}

// namespaceAccess is called before the first namespaced request on an
// instance's namespace. In binding mode it makes sure muster's RoleBinding is
// there, and answers whether there is a namespace to make requests in at all.
// Outside binding mode it is a no-op that answers true, which is today's
// behaviour exactly.
//
// 🔴 "ABSENT" IS DECIDED HERE, FROM THE CLUSTER-SCOPED NAMESPACE READ, BECAUSE
// UNDER THE NARROW GRANT A NAMESPACED READ CANNOT DECIDE IT. Without a binding
// in a namespace, a Get of a Deployment there is 403 — whether or not the
// namespace exists — so the NotFound every caller branches on would arrive as
// ErrBlind. present=false means: no instance lives here. It is returned for
//
//   - a namespace that does not exist;
//   - a namespace that is TERMINATING — the instance is being destroyed, and a
//     RoleBinding cannot be written into it anyway (Create still waits it out,
//     in ensureNamespace);
//   - a namespace muster does NOT own. 🔴 muster must never bind itself into a
//     namespace it did not create: that would be granting itself a stranger's
//     Secrets. Create's apply then refuses it in ensureNamespace with
//     ErrNotManaged, and Destroy refuses it in checkNamespaceOwned, both
//     unchanged; the by-name reads report the instance absent.
//
// 🔴 AN EXISTING NAMESPACE WITHOUT A BINDING — ONE CREATED BEFORE THIS LAYOUT
// WAS TURNED ON — GETS ONE HERE, so an upgraded muster still manages its older
// agents once the cluster-wide object grant is removed.
func (d *Driver) namespaceAccess(ctx context.Context, instance, ns string) (bool, error) {
	if !d.bindingMode() {
		return true, nil
	}
	cur, err := d.cfg.Client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, blind("get namespace "+ns, err)
	}
	if !owned(cur.Labels) || terminating(cur) {
		return false, nil
	}
	return true, d.ensureRoleBinding(ctx, instance, ns)
}

func terminating(ns *corev1.Namespace) bool {
	return ns.Status.Phase == corev1.NamespaceTerminating || ns.DeletionTimestamp != nil
}

// ensureRoleBinding creates muster's RoleBinding in ns if it is missing (or
// corrects it), and on any write waits until it has taken effect.
//
// 🔴 IT REFUSES A CO-NAMED BINDING MUSTER DID NOT WRITE, with ErrNotManaged,
// for upsertOwned's reason: its subject and roleRef are somebody else's choice.
//
// ⚠ A BINDING WHOSE roleRef OR SUBJECT IS NOT THE CONFIGURED ONE (the
// ClusterRole renamed, muster's ServiceAccount renamed) IS DELETED AND CREATED
// AGAIN: roleRef is immutable, and doing the subject the same way keeps
// `update` out of the cluster-wide grant.
func (d *Driver) ensureRoleBinding(ctx context.Context, instance, ns string) error {
	want := d.renderRoleBinding(instance, ns)
	rbs := d.cfg.Client.RbacV1().RoleBindings(ns)
	cur, err := rbs.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err := rbs.Create(ctx, want, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			// A concurrent caller wrote it between the two calls: re-read and
			// judge THAT object, once.
			if cur, err = rbs.Get(ctx, want.Name, metav1.GetOptions{}); err != nil {
				return blind("get rolebinding "+want.Name+" in "+ns, err)
			}
			break
		}
		if err != nil {
			return roleBindingFailed("create", ns, err)
		}
		return d.waitForAccess(ctx, instance, ns)
	case err != nil:
		return roleBindingFailed("get", ns, err)
	}

	if !owned(cur.Labels) {
		return fmt.Errorf("apply rolebinding %s: %w", want.Name,
			notManagedAction("rolebinding", want.Name, ns, cur.Labels))
	}
	if cur.RoleRef == want.RoleRef && reflect.DeepEqual(cur.Subjects, want.Subjects) {
		return nil
	}
	uid := cur.UID
	if err := rbs.Delete(ctx, want.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	}); err != nil && !apierrors.IsNotFound(err) {
		return roleBindingFailed("delete", ns, err)
	}
	if _, err := rbs.Create(ctx, want, metav1.CreateOptions{}); err != nil {
		return roleBindingFailed("create", ns, err)
	}
	return d.waitForAccess(ctx, instance, ns)
}

// roleBindingFailed attributes a failed RoleBinding request. A 403 names the
// cluster-wide rule to add, conditionally (a 403 is usually RBAC and not
// always — see networkPolicyForbidden).
func roleBindingFailed(verb, ns string, err error) error {
	if apierrors.IsForbidden(err) {
		err = fmt.Errorf("%w — if this refusal is about RBAC, grant muster's ClusterRole %s on "+
			"rolebindings.rbac.authorization.k8s.io and `bind` on the configured ClusterRole (resourceNames)",
			err, quotedVerbs(NamespaceRoleBindingClusterRBAC["rolebindings"]))
	}
	return blind(verb+" rolebinding "+NamespaceRoleBindingName+" in "+ns, err)
}

// waitForAccess waits, bounded, for a RoleBinding just written to take effect.
//
// 🔴 RBAC IS NOT READ-YOUR-WRITES. The apiserver authorises from an informer
// cache, so the first namespaced request right after the binding is created
// can still be 403. This probes with a REAL request — a Get of the instance's
// ServiceAccount, which the bound ClusterRole must allow — until it is not 403,
// rather than asking an access review (which answered yes for a request that
// then got 403, in the project this came from; see Exec). NotFound is success:
// it means the request was authorised.
//
// ⚠ WHAT IT DOES NOT CLAIM: that every apiserver of a multi-apiserver control
// plane has caught up. Each has its own cache; this proves the one that
// answered the probe has. A later 403 from a lagging one surfaces as ErrBlind
// and clears on retry.
func (d *Driver) waitForAccess(ctx context.Context, instance, ns string) error {
	deadline := time.Now().Add(rbacPropagationTimeout)
	for {
		_, err := d.cfg.Client.CoreV1().ServiceAccounts(ns).Get(ctx, instance, metav1.GetOptions{})
		if err == nil || !apierrors.IsForbidden(err) {
			if err != nil && !apierrors.IsNotFound(err) {
				return blind("probe access in "+ns, err)
			}
			return nil
		}
		if !time.Now().Before(deadline) {
			return blind("probe access in "+ns, fmt.Errorf("the RoleBinding %q binding ClusterRole %q in namespace %q "+
				"did not take effect within %s: a get of serviceaccounts there is still forbidden (%v). Check that the "+
				"ClusterRole exists and grants get on serviceaccounts",
				NamespaceRoleBindingName, d.cfg.NamespaceRoleBinding.ClusterRole, ns, rbacPropagationTimeout, err))
		}
		select {
		case <-ctx.Done():
			return blind("probe access in "+ns, errors.Join(ctx.Err(), err))
		case <-time.After(rbacPropagationPoll):
		}
	}
}

// instanceNamespaces lists the per-instance namespaces List reads in binding
// mode: muster-owned (managedSelector), under this driver's prefix, and not
// terminating.
//
// 🔴 THE PREFIX FILTER IS A SAFETY PROPERTY, NOT TIDINESS. A second muster
// installation on the same cluster labels its namespaces the same way, and
// namespaceAccess writes a RoleBinding into every namespace List reads — so an
// unfiltered list would bind THIS installation into the other's namespaces.
func (d *Driver) instanceNamespaces(ctx context.Context) ([]corev1.Namespace, error) {
	list, err := d.cfg.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
	if err != nil {
		return nil, blind("list namespaces", err)
	}
	prefix := d.namespacePrefix()
	out := make([]corev1.Namespace, 0, len(list.Items))
	for _, ns := range list.Items {
		if !strings.HasPrefix(ns.Name, prefix) || !owned(ns.Labels) || terminating(&ns) {
			continue
		}
		out = append(out, ns)
	}
	return out, nil
}
