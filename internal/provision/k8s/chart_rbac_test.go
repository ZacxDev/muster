//go:build chart

package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE HELM CHART'S RBAC COVERS WHAT THIS DRIVER DOES — measured, not listed.
//
// charts/muster/templates/rbac.yaml grants muster's ServiceAccount its rights in
// two layouts. This renders the chart (`helm template`), builds an
// RBAC-ENFORCING fake clientset out of exactly the ClusterRoles, Roles and
// bindings it rendered — plus the RoleBindings the driver writes itself, which
// is how the least-privilege layout works — configures a Driver from the
// SAME render's server environment (prefix, ClusterRole name, NetworkPolicy
// selectors), and runs an instance's whole life through it: an isolated
// claude-code-shaped instance with a deploy-key holder and an image pull Secret
// (so the Secret copies from muster's own namespace are on the path too).
//
// 🔴 ANY REQUEST THE RENDERED RBAC DOES NOT ALLOW IS A 403, AND A 403 FAILS THE
// TEST. The stock fake authorises everything; that is what this replaces.
//
// The two verbs this cannot drive through a fake clientset — `pods/exec`
// (a SPDY/WebSocket upgrade) and `pods/log` (a raw stream) — are checked
// against the driver's own declared needs (PodExecRBACPrerequisite, and the
// `get` GetLogs sends), through the same evaluator.
//
// Run: go test -tags chart -run TestChart ./internal/provision/k8s/  (helm on PATH)
// ---------------------------------------------------------------------------

const chartReleaseNS = "muster"

func TestChartRBACCoversAnInstancesWholeLife(t *testing.T) {
	for _, tc := range []struct{ set, layout string }{
		{"full.yaml", "default"},
		{"least-privilege.yaml", "least-privilege"},
	} {
		t.Run(tc.layout, func(t *testing.T) {
			r := renderChart(t, tc.set)
			if got := r.layout(); got != tc.layout {
				t.Fatalf("instrument check: %s renders the %q layout, want %q", tc.set, got, tc.layout)
			}
			ev, cs, d := r.enforcingDriver(t)
			if err := runWholeLife(d); err != nil {
				t.Errorf("%v (refused: %v)", err, ev.deniedList())
			}
			if denied := ev.deniedList(); len(denied) != 0 {
				t.Errorf("the chart's %s RBAC refused: %v", tc.layout, denied)
			}
			if ev.allowedCount() < 20 {
				t.Errorf("instrument check: only %d request(s) went through the evaluator", ev.allowedCount())
			}
			// The streaming subresources, against the driver's declared needs.
			ns := r.env["MUSTER_AGENT_NAMESPACE_PREFIX"] + "alpha"
			for _, v := range PodExecRBACPrerequisite {
				if !ev.allows(cs, "", "pods/exec", v, ns, "") {
					t.Errorf("the chart's %s RBAC does not grant %s on pods/exec in an agent namespace", tc.layout, v)
				}
			}
			if !ev.allows(cs, "", "pods/log", "get", ns, "") {
				t.Errorf("the chart's %s RBAC does not grant get on pods/log in an agent namespace", tc.layout)
			}
		})
	}
}

// TestChartRBACEvaluatorRefusesWhatItMust: the evaluator's negative controls.
// The same whole life against the least-privilege render with (a) the
// source-Secrets Role removed, and (b) the agent-namespace ClusterRole's
// networkpolicies rule removed, must be refused — each naming the request.
func TestChartRBACEvaluatorRefusesWhatItMust(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"no source-secrets Role", "get secrets " + chartReleaseNS},
		{"no networkpolicies rule", "networkpolicies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := renderChart(t, "least-privilege.yaml")
			switch tc.name {
			case "no source-secrets Role":
				r.roles = nil
			case "no networkpolicies rule":
				role := r.env["MUSTER_AGENT_NAMESPACE_CLUSTERROLE"]
				cr := r.clusterRoles[role]
				var kept []rbacv1.PolicyRule
				for _, rule := range cr.Rules {
					if !contains(rule.Resources, "networkpolicies") {
						kept = append(kept, rule)
					}
				}
				cr.Rules = kept
				r.clusterRoles[role] = cr
			}
			ev, _, d := r.enforcingDriver(t)
			err := runWholeLife(d)
			if err == nil {
				t.Errorf("with %s the whole life succeeded; the evaluator enforced nothing", tc.name)
			}
			joined := strings.Join(ev.deniedList(), "; ")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("with %s the evaluator refused %q, want a refusal naming %q", tc.name, joined, tc.want)
			}
		})
	}
}

// runWholeLife returns the first step that failed, naming it.
func runWholeLife(d *Driver) error {
	ctx := context.Background()
	ref := provision.Ref{Name: "alpha"}
	spec := chartLifeSpec("alpha")
	for _, s := range []struct {
		name string
		do   func() error
	}{
		{"Create", func() error { return d.Create(ctx, spec) }},
		{"Create again", func() error { return d.Create(ctx, spec) }},
		{"Get", func() error { _, err := d.Get(ctx, ref); return err }},
		{"List", func() error { _, err := d.List(ctx); return err }},
		{"Scale 0", func() error { return d.Scale(ctx, ref, 0) }},
		{"Scale 1", func() error { return d.Scale(ctx, ref, 1) }},
		{"Endpoint", func() error { _, err := d.Endpoint(ctx, ref); return err }},
		{"Update", func() error { return d.Update(ctx, spec) }},
		{"Destroy", func() error { return d.Destroy(ctx, ref) }},
		{"Destroy again", func() error { return d.Destroy(ctx, ref) }},
	} {
		if err := s.do(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// chartLifeSpec: an isolated instance with a deploy-key holder and an image
// pull Secret, both sourced from muster's own namespace.
func chartLifeSpec(name string) provision.Spec {
	s := isolatedSpec(name)
	s.Security.RunAsUser = 1000
	s.KeyHolder = &provision.KeyHolder{
		Name: "deploy-key", RunAsUser: 1001, Command: []string{"sh", "-c", "sleep 1"},
		SecretMounts: []provision.SecretMount{{Secret: "repo-deploy-key", Key: "ssh-privatekey", Path: "/etc/muster/deploy-key/ssh-privatekey"}},
		SharedDir:    "/var/lib/muster-mirror",
	}
	s.ImagePullSecret = "runner-pull"
	return s
}

// --- the render ---------------------------------------------------------------

type chartRender struct {
	sa                  rbacv1.Subject
	clusterRoles        map[string]rbacv1.ClusterRole
	clusterRoleBindings []rbacv1.ClusterRoleBinding
	roles               map[string]rbacv1.Role // in chartReleaseNS
	roleBindings        []rbacv1.RoleBinding   // in chartReleaseNS
	env                 map[string]string      // the server container's literal values
}

func (r *chartRender) layout() string {
	if r.env["MUSTER_AGENT_NAMESPACE_CLUSTERROLE"] != "" {
		return "least-privilege"
	}
	return "default"
}

func renderChart(t *testing.T, set string) *chartRender {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "../../..")
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatalf("helm is not on PATH: this test renders the chart (the CI job installs it)")
	}
	cmd := exec.Command(helm, "template", "t", filepath.Join(root, "charts/muster"), "-n", chartReleaseNS,
		"-f", filepath.Join(root, "charts/muster/ci", set))
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template %s: %v\n%s", set, err, stderr.String())
	}
	r := &chartRender{clusterRoles: map[string]rbacv1.ClusterRole{}, roles: map[string]rbacv1.Role{}, env: map[string]string{}}
	dec := k8syaml.NewYAMLOrJSONDecoder(&out, 4096)
	for {
		var raw k8sruntime.RawExtension
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(raw.Raw)) == 0 {
			continue
		}
		var meta metav1.TypeMeta
		mustJSON(t, raw.Raw, &meta)
		switch meta.Kind {
		case "ServiceAccount":
			var sa corev1.ServiceAccount
			mustJSON(t, raw.Raw, &sa)
			r.sa = rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: chartReleaseNS}
		case "ClusterRole":
			var o rbacv1.ClusterRole
			mustJSON(t, raw.Raw, &o)
			r.clusterRoles[o.Name] = o
		case "ClusterRoleBinding":
			var o rbacv1.ClusterRoleBinding
			mustJSON(t, raw.Raw, &o)
			r.clusterRoleBindings = append(r.clusterRoleBindings, o)
		case "Role":
			var o rbacv1.Role
			mustJSON(t, raw.Raw, &o)
			r.roles[o.Name] = o
		case "RoleBinding":
			var o rbacv1.RoleBinding
			mustJSON(t, raw.Raw, &o)
			r.roleBindings = append(r.roleBindings, o)
		case "Deployment":
			var o struct {
				Metadata metav1.ObjectMeta
				Spec     struct {
					Template struct{ Spec corev1.PodSpec }
				}
			}
			mustJSON(t, raw.Raw, &o)
			if o.Metadata.Labels["app.kubernetes.io/component"] != "server" {
				continue
			}
			for _, c := range o.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					if e.ValueFrom == nil {
						r.env[e.Name] = e.Value
					}
				}
			}
		}
	}
	if r.sa.Name == "" || len(r.clusterRoles) == 0 || r.env["MUSTER_AGENT_PROVISIONER"] != "kubernetes" {
		t.Fatalf("instrument check: %s rendered no ServiceAccount, no ClusterRole or no kubernetes agent plane", set)
	}
	return r
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := k8syaml.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// enforcingDriver: a driver configured from the render's environment, on a
// fake clientset holding the source Secrets, behind the render's RBAC.
func (r *chartRender) enforcingDriver(t *testing.T) (*chartRBAC, *fake.Clientset, *Driver) {
	t.Helper()
	cs := fake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "repo-deploy-key", Namespace: chartReleaseNS},
			Type: corev1.SecretTypeSSHAuth, Data: map[string][]byte{"ssh-privatekey": []byte("fixture-key\n")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runner-pull", Namespace: chartReleaseNS},
			Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)}},
	)
	ev := &chartRBAC{r: r}
	cs.PrependReactor("*", "*", func(a k8stesting.Action) (bool, k8sruntime.Object, error) { return ev.react(cs, a) })

	labels := map[string]string{}
	for _, kv := range strings.Split(r.env["MUSTER_AGENT_NETPOL_FROM_POD_LABELS"], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("MUSTER_AGENT_NETPOL_FROM_POD_LABELS %q is not key=value pairs", r.env["MUSTER_AGENT_NETPOL_FROM_POD_LABELS"])
		}
		labels[k] = v
	}
	cfg := Config{Client: cs, NamespacePerInstance: true, NamespacePrefix: r.env["MUSTER_AGENT_NAMESPACE_PREFIX"],
		SecretSourceNamespace: r.env["MUSTER_AGENT_NETPOL_FROM_NAMESPACE"],
		NetworkPolicy: &NetworkPolicyConfig{ControllerNamespace: r.env["MUSTER_AGENT_NETPOL_FROM_NAMESPACE"],
			ControllerPodLabels: labels}}
	empty := ""
	cfg.WorkspaceStorageClass = &empty
	if role := r.env["MUSTER_AGENT_NAMESPACE_CLUSTERROLE"]; role != "" {
		cfg.NamespaceRoleBinding = &NamespaceRoleBindingConfig{ClusterRole: role,
			ServiceAccountNamespace: r.sa.Namespace, ServiceAccountName: r.sa.Name}
	}
	shortPropagation(t, 200*time.Millisecond)
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ev, cs, d
}

// --- the evaluator --------------------------------------------------------------

// chartRBAC answers each request as the apiserver's RBAC authorizer would for
// muster's ServiceAccount, holding the rendered objects plus every RoleBinding
// present in the fake's tracker (the ones the driver wrote).
type chartRBAC struct {
	r       *chartRender
	mu      sync.Mutex
	denied  []string
	allowed int
}

func (ev *chartRBAC) react(cs *fake.Clientset, a k8stesting.Action) (bool, k8sruntime.Object, error) {
	gr := a.GetResource()
	res := gr.Resource
	if a.GetSubresource() != "" {
		res += "/" + a.GetSubresource()
	}
	name := ""
	if n, ok := a.(interface{ GetName() string }); ok {
		name = n.GetName()
	}
	ok := ev.allows(cs, gr.Group, res, a.GetVerb(), a.GetNamespace(), name)
	if ok && gr.Group == rbacv1.GroupName && res == "rolebindings" && a.GetVerb() == "create" {
		// Writing a binding to a ClusterRole needs `bind` on THAT role (muster
		// does not hold every right it grants, so escalation is not the path).
		rb := a.(k8stesting.CreateAction).GetObject().(*rbacv1.RoleBinding)
		if !ev.allows(cs, rbacv1.GroupName, "clusterroles", "bind", "", rb.RoleRef.Name) {
			ok = false
		}
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if !ok {
		ev.denied = append(ev.denied, fmt.Sprintf("%s %s %s", a.GetVerb(), res, a.GetNamespace()))
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: gr.Group, Resource: res}, name,
			fmt.Errorf("the chart's RBAC does not allow this"))
	}
	ev.allowed++
	return false, nil, nil
}

func (ev *chartRBAC) allows(cs *fake.Clientset, group, res, verb, ns, name string) bool {
	r := ev.r
	for _, b := range r.clusterRoleBindings {
		if hasSubject(b.Subjects, r.sa) && b.RoleRef.Kind == "ClusterRole" && rulesAllow(r.clusterRoles[b.RoleRef.Name].Rules, group, res, verb, name) {
			return true
		}
	}
	if ns == "" {
		return false
	}
	var bindings []rbacv1.RoleBinding
	if ns == chartReleaseNS {
		bindings = append(bindings, r.roleBindings...)
	}
	if l, err := cs.Tracker().List(rbacv1.SchemeGroupVersion.WithResource("rolebindings"),
		rbacv1.SchemeGroupVersion.WithKind("RoleBinding"), ns); err == nil {
		for _, rb := range l.(*rbacv1.RoleBindingList).Items {
			bindings = append(bindings, rb)
		}
	}
	for _, b := range bindings {
		if !hasSubject(b.Subjects, r.sa) {
			continue
		}
		var rules []rbacv1.PolicyRule
		switch b.RoleRef.Kind {
		case "ClusterRole":
			rules = r.clusterRoles[b.RoleRef.Name].Rules
		case "Role":
			if ns == chartReleaseNS {
				rules = r.roles[b.RoleRef.Name].Rules
			}
		}
		if rulesAllow(rules, group, res, verb, name) {
			return true
		}
	}
	return false
}

func hasSubject(subjects []rbacv1.Subject, sa rbacv1.Subject) bool {
	for _, s := range subjects {
		if s.Kind == sa.Kind && s.Name == sa.Name && s.Namespace == sa.Namespace {
			return true
		}
	}
	return false
}

func rulesAllow(rules []rbacv1.PolicyRule, group, res, verb, name string) bool {
	for _, rule := range rules {
		if (contains(rule.APIGroups, group) || contains(rule.APIGroups, "*")) &&
			(contains(rule.Resources, res) || contains(rule.Resources, "*")) &&
			(contains(rule.Verbs, verb) || contains(rule.Verbs, "*")) &&
			(len(rule.ResourceNames) == 0 || contains(rule.ResourceNames, name)) {
			return true
		}
	}
	return false
}

func (ev *chartRBAC) deniedList() []string {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return append([]string(nil), ev.denied...)
}

func (ev *chartRBAC) allowedCount() int {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return ev.allowed
}
