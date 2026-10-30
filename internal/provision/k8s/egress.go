package k8s

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ZacxDev/muster/internal/provision"
)

// The per-instance name-filtered egress policy: a CiliumNetworkPolicy, written
// for a spec that names Network.PublicEgressHosts, on a driver whose
// NetworkPolicyConfig.CiliumFQDN says the cluster runs Cilium.
//
// 🔴 IT REPLACES TWO RULES OF THE NetworkPolicy, IT DOES NOT SIT BESIDE THEM.
// Network plugins union allows, so a toFQDNs allow next to the NetworkPolicy's
// "any public address on 443" narrows nothing, and a name-filtered DNS rule next
// to its plain "port 53 to the DNS pods" is widened back to every name. For such
// a spec renderNetworkPolicy writes NEITHER of those two rules (its ingress and
// controller rules are unchanged), and this policy carries both, filtered:
//
//   - DNS to the cluster's DNS pods (UDP/TCP 53) through Cilium's DNS proxy,
//     answering only for the spec's hosts and for names under the cluster
//     domain at any depth (`**.<domain>`: muster's own Service names, and the
//     search-path expansions every lookup is first tried with under ndots:5);
//   - each host by exact name (toFQDNs matchName) on the spec's public ports.
//
// enableDefaultDeny: egress true (anything else is dropped, even should the
// NetworkPolicy be missing), ingress false (the NetworkPolicy owns ingress).
//
// ⚠ "WRITTEN", NOT "ENFORCED", for the NetworkPolicy's reason: on a cluster
// without Cilium the apiserver has no such kind and the write fails (closed);
// on one with Cilium but a different DNS setup, lookups fail (closed, loud).

// ciliumNetworkPolicyGVR is the resource the policy is written as.
var ciliumNetworkPolicyGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

// egressPolicyName is the per-instance CiliumNetworkPolicy's name, shared by the
// render and Destroy.
func egressPolicyName(instance string) string { return instance + "-egress" }

// CiliumPolicyRBACPrerequisite is every verb the muster ServiceAccount must hold
// on `ciliumnetworkpolicies.cilium.io` when CiliumFQDN is set: applyEgressPolicy's
// get/create/update, and Destroy's get/delete.
//
//   - apiGroups: ["cilium.io"]
//     resources: ["ciliumnetworkpolicies"]
//     verbs: ["get", "create", "update", "delete"]
var CiliumPolicyRBACPrerequisite = []string{"get", "create", "update", "delete"}

var ciliumPolicyRBACHint = "grant muster's ClusterRole apiGroups [\"cilium.io\"] resources " +
	"[\"ciliumnetworkpolicies\"] verbs [" + quotedVerbs(CiliumPolicyRBACPrerequisite) + "]"

// namedEgress reports whether spec's public egress is confined by name, which
// is what moves its DNS and public rules out of the NetworkPolicy.
func namedEgress(spec provision.Spec) bool {
	return spec.Network.Isolate && len(spec.Network.PublicEgressHosts) > 0
}

// renderEgressPolicy is the CiliumNetworkPolicy for spec, or nil when the spec
// names no hosts.
func (d *Driver) renderEgressPolicy(spec provision.Spec, ns string) (*unstructured.Unstructured, error) {
	if !namedEgress(spec) {
		return nil, nil
	}
	cfg := d.cfg.NetworkPolicy
	if cfg == nil || !cfg.CiliumFQDN || d.cfg.Dynamic == nil {
		// Unreachable through checkSpec (Capabilities.NetworkEgressByName); an
		// error, never nil, for renderNetworkPolicy's reason.
		return nil, fmt.Errorf("%w: the spec confines egress to host names and this driver has no Cilium "+
			"FQDN policy configured to render it", provision.ErrUnsupported)
	}
	hosts := append([]string(nil), spec.Network.PublicEgressHosts...)
	sort.Strings(hosts)
	ports := append([]int(nil), spec.Network.PublicEgressTCPPorts...)
	sort.Ints(ports)

	dnsSel := map[string]any{"k8s:io.kubernetes.pod.namespace": cfg.dnsNamespace()}
	for k, v := range cfg.dnsPodLabels() {
		dnsSel[k] = v
	}
	dnsNames := make([]any, 0, len(hosts)+1)
	fqdns := make([]any, 0, len(hosts))
	for _, h := range hosts {
		dnsNames = append(dnsNames, map[string]any{"matchName": h})
		fqdns = append(fqdns, map[string]any{"matchName": h})
	}
	dnsNames = append(dnsNames, map[string]any{"matchPattern": "**." + cfg.ClusterDomain})
	publicPorts := make([]any, 0, len(ports))
	for _, p := range ports {
		publicPorts = append(publicPorts, map[string]any{"port": fmt.Sprint(p), "protocol": "TCP"})
	}
	selector := map[string]any{}
	for k, v := range instanceLabels(spec.Ref.Name) {
		selector[k] = v
	}
	labels := map[string]any{}
	for k, v := range mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels) {
		labels[k] = v
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]any{
			"name":      egressPolicyName(spec.Ref.Name),
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]any{
			"endpointSelector":  map[string]any{"matchLabels": selector},
			"enableDefaultDeny": map[string]any{"ingress": false, "egress": true},
			"egress": []any{
				map[string]any{
					"toEndpoints": []any{map[string]any{"matchLabels": dnsSel}},
					"toPorts": []any{map[string]any{
						"ports": []any{
							map[string]any{"port": fmt.Sprint(dnsPort), "protocol": "UDP"},
							map[string]any{"port": fmt.Sprint(dnsPort), "protocol": "TCP"},
						},
						"rules": map[string]any{"dns": dnsNames},
					}},
				},
				map[string]any{
					"toFQDNs": fqdns,
					"toPorts": []any{map[string]any{"ports": publicPorts}},
				},
			},
		},
	}}, nil
}

// applyEgressPolicy upserts the instance's CiliumNetworkPolicy when the spec
// names hosts, and makes NO cilium.io call when it does not — so a deployment
// whose specs name none needs no RBAC on it and no CRD. Like the NetworkPolicy,
// a spec that stops naming hosts keeps its policy until Destroy (leaving it is
// harmless: plugins union allows, and it allows only what the spec allowed).
func (d *Driver) applyEgressPolicy(ctx context.Context, spec provision.Spec, ns string) error {
	obj, err := d.renderEgressPolicy(spec, ns)
	if err != nil || obj == nil {
		return err
	}
	c := d.cfg.Dynamic.Resource(ciliumNetworkPolicyGVR).Namespace(ns)
	name := obj.GetName()
	verb, resourceVersion := "", ""
	err = d.upsertOwned("ciliumnetworkpolicy", name, ns,
		func() (map[string]string, error) {
			verb = "get"
			cur, e := c.Get(ctx, name, metav1.GetOptions{})
			if e != nil {
				return nil, e
			}
			resourceVersion = cur.GetResourceVersion()
			return cur.GetLabels(), nil
		},
		func() error {
			verb = "create"
			_, e := c.Create(ctx, obj, metav1.CreateOptions{})
			return e
		},
		func() error {
			verb = "update"
			// A custom resource is not unconditionally updatable: the write
			// carries the version the ownership read saw.
			upd := obj.DeepCopy()
			upd.SetResourceVersion(resourceVersion)
			_, e := c.Update(ctx, upd, metav1.UpdateOptions{})
			return e
		})
	switch {
	case err == nil:
		return nil
	case apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause):
		return applyFailed("apply ciliumnetworkpolicy "+name, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: the apiserver refused (403 Forbidden) muster's %s of ciliumnetworkpolicies.cilium.io %q "+
			"in namespace %q: %v. Nothing after it was written, so an instance that was not running was NOT started. If "+
			"the refusal is about RBAC, %s", provision.ErrUnsupported, verb, name, ns, err, ciliumPolicyRBACHint)
	case apierrors.IsNotFound(err) && verb == "create":
		return fmt.Errorf("%w: the apiserver has no ciliumnetworkpolicies.cilium.io (%v): this cluster does not "+
			"run Cilium, so the instance's egress cannot be confined by name and it was NOT started",
			provision.ErrUnsupported, err)
	default:
		return applyFailed("apply ciliumnetworkpolicy "+name, err)
	}
}

// deleteEgressPolicy is Destroy's removal of the instance's CiliumNetworkPolicy
// (a no-op when the driver is not configured for it).
func (d *Driver) deleteEgressPolicy(ctx context.Context, ns, instance string) error {
	if d.cfg.NetworkPolicy == nil || !d.cfg.NetworkPolicy.CiliumFQDN || d.cfg.Dynamic == nil {
		return nil
	}
	c := d.cfg.Dynamic.Resource(ciliumNetworkPolicyGVR).Namespace(ns)
	name := egressPolicyName(instance)
	err := d.deleteIfOwned("ciliumnetworkpolicy", name,
		func() (map[string]string, error) {
			cur, e := c.Get(ctx, name, metav1.GetOptions{})
			if e != nil {
				return nil, e
			}
			return cur.GetLabels(), nil
		},
		func() error { return c.Delete(ctx, name, metav1.DeleteOptions{}) })
	if apierrors.IsForbidden(err) {
		err = fmt.Errorf("%w — if this refusal is about RBAC, %s", err, ciliumPolicyRBACHint)
	}
	return err
}
