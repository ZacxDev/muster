package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// The tests for the name-filtered egress policy (egress.go) of an egress-locked
// pool's worker.
//
// 🔴 WHAT NONE OF THEM CAN SEE: ENFORCEMENT, for network_internal_test.go's
// reason. The fake dynamic client stores a CiliumNetworkPolicy and filters
// nothing; whether Cilium's DNS proxy and toFQDNs drop what they should is a
// property of the cluster.

func newFakeDynamic() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList"})
}

func fakeDyn(t *testing.T, d *Driver) *dynamicfake.FakeDynamicClient {
	t.Helper()
	f, ok := d.cfg.Dynamic.(*dynamicfake.FakeDynamicClient)
	if !ok {
		t.Fatalf("driver's dynamic client is %T, not the fake", d.cfg.Dynamic)
	}
	return f
}

// seedCNP puts a CiliumNetworkPolicy with these labels under name in ns.
func seedCNP(t *testing.T, d *Driver, ns, name string, labels map[string]string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("cilium.io/v2")
	u.SetKind("CiliumNetworkPolicy")
	u.SetName(name)
	u.SetNamespace(ns)
	u.SetLabels(labels)
	if _, err := d.cfg.Dynamic.Resource(ciliumNetworkPolicyGVR).Namespace(ns).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed cnp: %v", err)
	}
}

func getCNP(t *testing.T, d *Driver, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := d.cfg.Dynamic.Resource(ciliumNetworkPolicyGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cnp %s/%s: %v", ns, name, err)
	}
	return u
}

func ciliumActions(d *Driver) []string {
	var out []string
	for _, a := range d.cfg.Dynamic.(*dynamicfake.FakeDynamicClient).Actions() {
		out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
	}
	return out
}

// lockedPerInstanceDriver is perInstanceDriver (the layout a claude-code worker
// runs in) able to filter by name, its DNS selector and cluster domain set to
// values no default uses.
func lockedPerInstanceDriver(t *testing.T) (*Driver, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	empty := ""
	np := internalNetworkPolicy()
	np.CiliumFQDN = true
	np.ClusterDomain = "cluster.example"
	d, err := New(Config{Client: cs, NamespacePerInstance: true, NamespacePrefix: "muster-agent-",
		WorkspaceStorageClass: &empty, NetworkPolicy: np, Dynamic: newFakeDynamic()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, cs
}

// poolSpec builds a REAL claude-code worker of pool `pool` (agentspec), in a
// deployment where pool "locked-review" is listed in PoolEgress with the extra
// host hq.example.test, and pool "private-review" has a deploy key.
func poolSpec(t *testing.T, name, pool, repo string) provision.Spec {
	t.Helper()
	spec, err := agentspec.Build(agents.Agent{
		ID: 7, Name: name, Namespace: "agents", HooksToken: "fixture-hooks-token-k8s", GatewaySecret: "fixture-turn-credential-k8s",
		Kind: agents.KindClaudeCode, CCAccount: "work", WorkerPool: pool, Repo: repo,
	}, agentspec.Config{
		ImageRepo: "registry.example.test/unused", APIBaseURL: "http://muster.example.test:8105",
		ClaudeCode: &agentspec.ClaudeCodeConfig{
			Image:    "ghcr.io/example-org/claude-code-agent:abc1234",
			Accounts: map[string]string{"work": fakeCCToken},
			PoolDeployKeys: map[string]agentspec.PoolDeployKey{
				"private-review": {Repo: "acme/private-app", Secret: "private-app-deploy-key"},
			},
			PoolEgress: map[string][]string{"locked-review": {"hq.example.test"}},
		},
	}, agentspec.Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return spec
}

// wantLockedCNPSpec is the CiliumNetworkPolicy spec a worker of pool
// "locked-review" named locked-heron gets from lockedPerInstanceDriver.
//
// 🔴 WRITTEN BY HAND FROM THE REQUIREMENT: DNS through the proxy for exactly
// the six default hosts, HQ and the cluster domain at any depth; TCP 443 to
// exactly those seven names; egress default-deny, ingress left to the
// NetworkPolicy; selecting this instance's pods only.
const wantLockedCNPSpec = `{
  "egress": [
    {
      "toEndpoints": [
        {
          "matchLabels": {
            "k8s-app": "kube-dns",
            "k8s:io.kubernetes.pod.namespace": "kube-system"
          }
        }
      ],
      "toPorts": [
        {
          "ports": [
            {
              "port": "53",
              "protocol": "UDP"
            },
            {
              "port": "53",
              "protocol": "TCP"
            }
          ],
          "rules": {
            "dns": [
              {
                "matchName": "api.anthropic.com"
              },
              {
                "matchName": "api.github.com"
              },
              {
                "matchName": "codeload.github.com"
              },
              {
                "matchName": "github.com"
              },
              {
                "matchName": "hq.example.test"
              },
              {
                "matchName": "objects.githubusercontent.com"
              },
              {
                "matchName": "ssh.github.com"
              },
              {
                "matchPattern": "**.cluster.example"
              }
            ]
          }
        }
      ]
    },
    {
      "toFQDNs": [
        {
          "matchName": "api.anthropic.com"
        },
        {
          "matchName": "api.github.com"
        },
        {
          "matchName": "codeload.github.com"
        },
        {
          "matchName": "github.com"
        },
        {
          "matchName": "hq.example.test"
        },
        {
          "matchName": "objects.githubusercontent.com"
        },
        {
          "matchName": "ssh.github.com"
        }
      ],
      "toPorts": [
        {
          "ports": [
            {
              "port": "443",
              "protocol": "TCP"
            }
          ]
        }
      ]
    }
  ],
  "enableDefaultDeny": {
    "egress": true,
    "ingress": false
  },
  "endpointSelector": {
    "matchLabels": {
      "app.kubernetes.io/instance": "locked-heron",
      "app.kubernetes.io/managed-by": "muster",
      "app.kubernetes.io/name": "muster-agent"
    }
  }
}`

// TestALockedPoolWorkerGetsExactlyThisCiliumPolicy pins the WHOLE written
// CiliumNetworkPolicy of a real egress-locked worker, and its selector against
// the pod template it must select.
func TestALockedPoolWorkerGetsExactlyThisCiliumPolicy(t *testing.T) {
	d, cs := lockedPerInstanceDriver(t)
	ctx := context.Background()
	if err := d.Create(ctx, poolSpec(t, "locked-heron", "locked-review", "")); err != nil {
		t.Fatalf("create: %v", err)
	}
	ns := "muster-agent-locked-heron"
	u := getCNP(t, d, ns, "locked-heron-egress")
	b, err := json.MarshalIndent(u.Object["spec"], "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != wantLockedCNPSpec {
		t.Errorf("the CiliumNetworkPolicy spec is not the pinned one.\n--- got ---\n%s\n--- want ---\n%s", got, wantLockedCNPSpec)
	}
	if u.GetKind() != "CiliumNetworkPolicy" || u.GetAPIVersion() != "cilium.io/v2" || !owned(u.GetLabels()) {
		t.Errorf("kind %q apiVersion %q labels %v: want a cilium.io/v2 CiliumNetworkPolicy muster owns",
			u.GetKind(), u.GetAPIVersion(), u.GetLabels())
	}
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "locked-heron", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sel, _, _ := unstructured.NestedStringMap(u.Object, "spec", "endpointSelector", "matchLabels")
	if len(sel) == 0 {
		t.Fatal("empty endpoint selector: it would select every pod in the namespace")
	}
	for k, v := range sel {
		if dep.Spec.Template.Labels[k] != v {
			t.Errorf("the policy selects %s=%s, which the pod template does not carry", k, v)
		}
	}
	// The pod is told not to try the CLI's optional hosts.
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Errorf("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = %q, want 1 on a locked worker", env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"])
	}
}

// TestALockedWorkersNetworkPolicyHasNoDNSOrPublicRule: the NetworkPolicy of a
// locked worker keeps its ingress rule and loses BOTH unfiltered egress rules —
// an unfiltered allow would union over the Cilium policy and widen it back.
func TestALockedWorkersNetworkPolicyHasNoDNSOrPublicRule(t *testing.T) {
	d, cs := lockedPerInstanceDriver(t)
	if err := d.Create(context.Background(), poolSpec(t, "locked-heron", "locked-review", "")); err != nil {
		t.Fatalf("create: %v", err)
	}
	np := getPolicy(t, cs, "muster-agent-locked-heron", "locked-heron-network")
	if len(np.Spec.Egress) != 0 {
		t.Errorf("a locked worker's NetworkPolicy has %d egress rule(s), want none (DNS and public move to the "+
			"Cilium policy; no connect port is configured here):\n%s", len(np.Spec.Egress), specJSON(t, np))
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.PolicyTypes) != 2 {
		t.Errorf("ingress %d, policyTypes %v: want the ingress rule and both types kept", len(np.Spec.Ingress), np.Spec.PolicyTypes)
	}
}

// TestAnUnlockedPoolWorkersPoliciesAreByteIdentical: on a driver that CAN write
// Cilium policies, a worker of a pool that is neither listed nor keyed gets the
// pinned claude-code NetworkPolicy byte for byte (wantClaudeCodePolicySpec, the
// golden every other pool had before), and NO cilium.io call at all. The
// control: a locked worker on the same driver does make one.
func TestAnUnlockedPoolWorkersPoliciesAreByteIdentical(t *testing.T) {
	d, cs := lockedPerInstanceDriver(t)
	ctx := context.Background()
	if err := d.Create(ctx, poolSpec(t, "quiet-heron", "pr-review", "")); err != nil {
		t.Fatalf("create: %v", err)
	}
	np := getPolicy(t, cs, "muster-agent-quiet-heron", "quiet-heron-network")
	if got := specJSON(t, np); got != wantClaudeCodePolicySpec {
		t.Errorf("an unlocked pool's NetworkPolicy changed.\n--- got ---\n%s\n--- want ---\n%s", got, wantClaudeCodePolicySpec)
	}
	if acts := ciliumActions(d); len(acts) != 0 {
		t.Errorf("an unlocked worker made cilium.io calls %v, want none", acts)
	}
	if err := d.Create(ctx, poolSpec(t, "locked-heron", "locked-review", "")); err != nil {
		t.Fatalf("control create: %v", err)
	}
	if acts := ciliumActions(d); len(acts) == 0 {
		t.Fatal("control: a locked worker made no cilium.io call either, so the zero above proves nothing")
	}
}

// TestADeployKeyPoolIsLockedWithoutBeingListed: a deploy-key pool not named in
// PoolEgress is locked to the defaults (no HQ).
func TestADeployKeyPoolIsLockedWithoutBeingListed(t *testing.T) {
	d, cs := lockedPerInstanceDriver(t)
	ctx := context.Background()
	ns := "muster-agent-private-heron"
	if err := d.Create(ctx, poolSpec(t, "private-heron", "private-review", "acme/private-app")); err != nil {
		t.Fatalf("create: %v", err)
	}
	fqdns, _, _ := unstructured.NestedSlice(getCNP(t, d, ns, "private-heron-egress").Object, "spec", "egress")
	b, _ := json.Marshal(fqdns)
	if !strings.Contains(string(b), `"ssh.github.com"`) || strings.Contains(string(b), "hq.example.test") {
		t.Errorf("deploy-key pool egress = %s, want the defaults (ssh.github.com included) and not another pool's HQ", b)
	}
	if len(getPolicy(t, cs, ns, "private-heron-network").Spec.Egress) != 0 {
		t.Error("the deploy-key worker's NetworkPolicy still has an unfiltered egress rule")
	}
}

// TestADriverThatCannotFilterByNameRefusesALockedWorker: without CiliumFQDN a
// locked worker is refused (ErrUnsupported) and NOTHING is written — not even
// its NetworkPolicy — so no pod starts with "any public address".
func TestADriverThatCannotFilterByNameRefusesALockedWorker(t *testing.T) {
	d, cs := perInstanceDriver(t)
	err := d.Create(context.Background(), poolSpec(t, "locked-heron", "locked-review", ""))
	if !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), "host names") {
		t.Fatalf("err = %v, want ErrUnsupported naming host names", err)
	}
	if len(cs.Actions()) != 0 {
		t.Errorf("the refused worker made %d API call(s), want none", len(cs.Actions()))
	}
	// Control: the same driver accepts an unlocked worker.
	if err := d.Create(context.Background(), poolSpec(t, "quiet-heron", "pr-review", "")); err != nil {
		t.Fatalf("control: an unlocked worker was refused too: %v", err)
	}
}

// TestNewRefusesCiliumFQDNWithoutADynamicClientOrADomain: each of the two
// requirements refused on its own; the control (both set) is accepted.
func TestNewRefusesCiliumFQDNWithoutADynamicClientOrADomain(t *testing.T) {
	np := internalNetworkPolicy()
	np.CiliumFQDN, np.ClusterDomain = true, "cluster.example"
	if _, err := New(Config{Client: fake.NewClientset(), Namespace: internalNS, NetworkPolicy: np, Dynamic: newFakeDynamic()}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if _, err := New(Config{Client: fake.NewClientset(), Namespace: internalNS, NetworkPolicy: np}); err == nil ||
		!strings.Contains(err.Error(), "Dynamic") {
		t.Fatalf("New = %v, want a refusal naming Config.Dynamic", err)
	}
	np.ClusterDomain = ""
	if _, err := New(Config{Client: fake.NewClientset(), Namespace: internalNS, NetworkPolicy: np, Dynamic: newFakeDynamic()}); err == nil ||
		!strings.Contains(err.Error(), "ClusterDomain") {
		t.Fatalf("New = %v, want a refusal naming ClusterDomain", err)
	}
}

// TestACiliumPolicyFailureStopsTheWorkerBeforeItRuns: a 403, and a cluster
// without the CRD (NotFound on create), each refuse with their own words and
// write no Deployment and no Secret.
func TestACiliumPolicyFailureStopsTheWorkerBeforeItRuns(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"forbidden", apierrors.NewForbidden(ciliumNetworkPolicyGVR.GroupResource(), "x", errors.New("rbac")), `"cilium.io"`},
		{"no crd", apierrors.NewNotFound(ciliumNetworkPolicyGVR.GroupResource(), ""), "does not run Cilium"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, cs := lockedPerInstanceDriver(t)
			fakeDyn(t, d).PrependReactor("create", "ciliumnetworkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, c.err
			})
			err := d.Create(context.Background(), poolSpec(t, "locked-heron", "locked-review", ""))
			if !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want ErrUnsupported containing %q", err, c.want)
			}
			ns := "muster-agent-locked-heron"
			if _, e := cs.AppsV1().Deployments(ns).Get(context.Background(), "locked-heron", metav1.GetOptions{}); !apierrors.IsNotFound(e) {
				t.Errorf("a Deployment exists after the refusal (%v)", e)
			}
			if l, _ := cs.CoreV1().Secrets(ns).List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
				t.Errorf("%d Secret(s) written after the refusal", len(l.Items))
			}
		})
	}
}

// TestUpdateRewritesTheCiliumPolicyWithItsVersion: a re-apply updates the
// policy muster owns (carrying the read resourceVersion: a custom resource is
// not unconditionally updatable), restoring an altered one.
func TestUpdateRewritesTheCiliumPolicyWithItsVersion(t *testing.T) {
	d, _ := lockedPerInstanceDriver(t)
	ctx := context.Background()
	spec := poolSpec(t, "locked-heron", "locked-review", "")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	ns := "muster-agent-locked-heron"
	c := d.cfg.Dynamic.Resource(ciliumNetworkPolicyGVR).Namespace(ns)
	u := getCNP(t, d, ns, "locked-heron-egress")
	u.SetResourceVersion("41")
	_ = unstructured.SetNestedField(u.Object, map[string]any{"egress": false}, "spec", "enableDefaultDeny")
	if _, err := c.Update(ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	var sawVersion string
	fakeDyn(t, d).PrependReactor("update", "ciliumnetworkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sawVersion = a.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured).GetResourceVersion()
		return false, nil, nil
	})
	if err := d.Update(ctx, spec); err != nil {
		t.Fatalf("update: %v", err)
	}
	if sawVersion != "41" {
		t.Errorf("the update carried resourceVersion %q, want the one read (41)", sawVersion)
	}
	if v, _, _ := unstructured.NestedBool(getCNP(t, d, ns, "locked-heron-egress").Object, "spec", "enableDefaultDeny", "egress"); !v {
		t.Error("the altered policy was not restored")
	}
}

// TestDestroyRemovesTheCiliumPolicyAndLeavesAStrangersAlone (shared layout,
// where no namespace deletion would take it).
func TestDestroyRemovesTheCiliumPolicyAndLeavesAStrangersAlone(t *testing.T) {
	d, _ := internalDriver(t)
	ctx := context.Background()
	s := internalSpec("gone")
	s.Network = provision.Network{Isolate: true, PublicEgressTCPPorts: []int{8443}, PublicEgressHosts: []string{"svc.example.test"}}
	if err := d.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	getCNP(t, d, internalNS, "gone-egress") // positive control: it was written
	seedCNP(t, d, internalNS, "other-egress", internalForeignMeta("other-egress").Labels)
	if err := d.Destroy(ctx, provision.Ref{Name: "gone"}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := d.cfg.Dynamic.Resource(ciliumNetworkPolicyGVR).Namespace(internalNS).Get(ctx, "gone-egress", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the instance's CiliumNetworkPolicy survived Destroy (%v)", err)
	}
	if err := d.Destroy(ctx, provision.Ref{Name: "other"}); err != nil {
		t.Fatalf("destroy other: %v", err)
	}
	getCNP(t, d, internalNS, "other-egress")
}
