package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// The tests for the per-instance NetworkPolicy (network.go).
//
// 🔴 WHAT NONE OF THEM CAN SEE: ENFORCEMENT. Every one runs against the fake
// clientset, which stores a NetworkPolicy and applies it to nothing. A green run
// here means the object is the one the code says it is, written in the order the
// code says, and refused when the code says — not that any packet was dropped.

// perInstanceDriver is internalDriver in the namespace-per-instance layout, which
// is the layout a claude-code agent runs in.
func perInstanceDriver(t *testing.T) (*Driver, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	empty := ""
	d, err := New(Config{Client: cs, NamespacePerInstance: true, NamespacePrefix: "muster-agent-",
		WorkspaceStorageClass: &empty, NetworkPolicy: internalNetworkPolicy()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, cs
}

// isolatedSpec is a synthetic isolated spec whose every number differs from the
// claude-code profile's, so a render that hardcoded that profile's values cannot
// pass the case that uses it.
func isolatedSpec(name string) provision.Spec {
	s := internalSpec(name)
	s.Network = provision.Network{Isolate: true, PublicEgressTCPPorts: []int{8443}}
	return s
}

func getPolicy(t *testing.T, cs *fake.Clientset, ns, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	np, err := cs.NetworkingV1().NetworkPolicies(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy %s/%s: %v", ns, name, err)
	}
	return np
}

func policyCount(t *testing.T, cs *fake.Clientset) int {
	t.Helper()
	l, err := cs.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list networkpolicies: %v", err)
	}
	return len(l.Items)
}

// networkingActions is every call the driver made against networkpolicies, as
// "verb" strings in order.
func networkingActions(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "networkpolicies" {
			out = append(out, a.GetVerb())
		}
	}
	return out
}

func specJSON(t *testing.T, np *networkingv1.NetworkPolicy) string {
	t.Helper()
	b, err := json.MarshalIndent(np.Spec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wantClaudeCodePolicySpec is the NetworkPolicy spec a claude-code agent named
// quiet-heron gets from a driver configured with internalNetworkPolicy().
//
// 🔴 WRITTEN BY HAND FROM THE REQUIREMENT, NOT PASTED FROM A RUN. Every value in
// it is one the policy exists to pin: the one ingress port and its one peer, the
// two DNS ports and their one peer, the one public port, the public block, and
// the five ranges cut out of it.
const wantClaudeCodePolicySpec = `{
  "podSelector": {
    "matchLabels": {
      "app.kubernetes.io/instance": "quiet-heron",
      "app.kubernetes.io/managed-by": "muster",
      "app.kubernetes.io/name": "muster-agent"
    }
  },
  "ingress": [
    {
      "ports": [
        {
          "protocol": "TCP",
          "port": 18789
        }
      ],
      "from": [
        {
          "podSelector": {
            "matchLabels": {
              "app": "muster-server",
              "tier": "control"
            }
          },
          "namespaceSelector": {
            "matchLabels": {
              "kubernetes.io/metadata.name": "control-plane-7"
            }
          }
        }
      ]
    }
  ],
  "egress": [
    {
      "ports": [
        {
          "protocol": "UDP",
          "port": 53
        },
        {
          "protocol": "TCP",
          "port": 53
        }
      ],
      "to": [
        {
          "podSelector": {
            "matchLabels": {
              "k8s-app": "kube-dns"
            }
          },
          "namespaceSelector": {
            "matchLabels": {
              "kubernetes.io/metadata.name": "kube-system"
            }
          }
        }
      ]
    },
    {
      "ports": [
        {
          "protocol": "TCP",
          "port": 443
        }
      ],
      "to": [
        {
          "ipBlock": {
            "cidr": "0.0.0.0/0",
            "except": [
              "10.0.0.0/8",
              "100.64.0.0/10",
              "169.254.0.0/16",
              "172.16.0.0/12",
              "192.168.0.0/16"
            ]
          }
        }
      ]
    }
  ],
  "policyTypes": [
    "Ingress",
    "Egress"
  ]
}`

// TestTheClaudeCodeAgentGetsExactlyThisNetworkPolicy pins the WHOLE rendered
// policy for a real claude-code spec (built by agentspec, not by hand), as the
// object the driver actually wrote to the cluster.
func TestTheClaudeCodeAgentGetsExactlyThisNetworkPolicy(t *testing.T) {
	d, cs := perInstanceDriver(t)
	if err := d.Create(context.Background(), ccSpec(t)); err != nil {
		t.Fatalf("create: %v", err)
	}
	np := getPolicy(t, cs, "muster-agent-quiet-heron", "quiet-heron-network")
	if got := specJSON(t, np); got != wantClaudeCodePolicySpec {
		t.Errorf("the claude-code NetworkPolicy spec is not the pinned one.\n--- got ---\n%s\n--- want ---\n%s", got, wantClaudeCodePolicySpec)
	}
	// It carries the driver's ownership labels, or Destroy would leave it behind
	// as a stranger's object.
	if !owned(np.Labels) || np.Labels[labelInstance] != "quiet-heron" {
		t.Errorf("the policy's labels do not mark it as this instance's: %v", np.Labels)
	}
	// And it selects the pod the Deployment actually creates: the policy's pod
	// selector must be a subset of the pod template's labels, or it confines
	// nothing.
	dep, err := cs.AppsV1().Deployments("muster-agent-quiet-heron").Get(context.Background(), "quiet-heron", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.PodSelector.MatchLabels) == 0 {
		t.Fatal("the policy's pod selector is EMPTY, which selects every pod in the namespace")
	}
	for k, v := range np.Spec.PodSelector.MatchLabels {
		if dep.Spec.Template.Labels[k] != v {
			t.Errorf("the policy selects %s=%s, which the instance's pod template does not carry (%v): "+
				"the policy would select nothing", k, v, dep.Spec.Template.Labels)
		}
	}
}

// TestTheRenderedPolicyFollowsTheSpecAndTheConfiguration is the control for the
// golden above: with different ports, a different caller and a different DNS
// selector, every one of those values moves. A render that returned the
// claude-code policy for everything would pass the golden and fail here.
func TestTheRenderedPolicyFollowsTheSpecAndTheConfiguration(t *testing.T) {
	cs := fake.NewClientset()
	d, err := New(Config{Client: cs, Namespace: internalNS, NetworkPolicy: &NetworkPolicyConfig{
		ControllerNamespace: "ops-east",
		ControllerPodLabels: map[string]string{"role": "dispatcher"},
		DNSNamespace:        "dns-system",
		DNSPodLabels:        map[string]string{"component": "resolver"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := provision.Spec{
		Ref:     provision.Ref{Name: "tall-ibis"},
		Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"},
		Ports: []provision.Port{
			{Name: "metrics", Port: 9105, Protocol: "UDP"},
			{Name: provision.DefaultPortName, Port: 8421},
		},
		// Unsorted on purpose: the render must not depend on the caller's order.
		Network: provision.Network{Isolate: true, PublicEgressTCPPorts: []int{8443, 993}},
		// A caller's label must not be able to widen or move the pod selector.
		Labels: map[string]string{labelInstance: "somebody-else", "team": "blue"},
	}
	np, err := d.renderNetworkPolicy(spec, internalNS)
	if err != nil || np == nil {
		t.Fatalf("render: %v / %v", np, err)
	}
	const want = `{
  "podSelector": {
    "matchLabels": {
      "app.kubernetes.io/instance": "tall-ibis",
      "app.kubernetes.io/managed-by": "muster",
      "app.kubernetes.io/name": "muster-agent"
    }
  },
  "ingress": [
    {
      "ports": [
        {
          "protocol": "TCP",
          "port": 8421
        },
        {
          "protocol": "UDP",
          "port": 9105
        }
      ],
      "from": [
        {
          "podSelector": {
            "matchLabels": {
              "role": "dispatcher"
            }
          },
          "namespaceSelector": {
            "matchLabels": {
              "kubernetes.io/metadata.name": "ops-east"
            }
          }
        }
      ]
    }
  ],
  "egress": [
    {
      "ports": [
        {
          "protocol": "UDP",
          "port": 53
        },
        {
          "protocol": "TCP",
          "port": 53
        }
      ],
      "to": [
        {
          "podSelector": {
            "matchLabels": {
              "component": "resolver"
            }
          },
          "namespaceSelector": {
            "matchLabels": {
              "kubernetes.io/metadata.name": "dns-system"
            }
          }
        }
      ]
    },
    {
      "ports": [
        {
          "protocol": "TCP",
          "port": 993
        },
        {
          "protocol": "TCP",
          "port": 8443
        }
      ],
      "to": [
        {
          "ipBlock": {
            "cidr": "0.0.0.0/0",
            "except": [
              "10.0.0.0/8",
              "100.64.0.0/10",
              "169.254.0.0/16",
              "172.16.0.0/12",
              "192.168.0.0/16"
            ]
          }
        }
      ]
    }
  ],
  "policyTypes": [
    "Ingress",
    "Egress"
  ]
}`
	if got := specJSON(t, np); got != want {
		t.Errorf("rendered policy differs.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if np.Name != "tall-ibis-network" || np.Namespace != internalNS {
		t.Errorf("policy is %s/%s, want %s/tall-ibis-network", np.Namespace, np.Name, internalNS)
	}
	if np.Labels[labelInstance] != "tall-ibis" || np.Labels["team"] != "blue" {
		t.Errorf("policy labels = %v: the driver's instance label must win and the caller's own must survive", np.Labels)
	}
}

// TestEachPeerIsOneSelectorPairNotTwo: the namespace and pod selectors of the
// caller (and of DNS) are ONE peer. As two peers they would be ORed — every pod
// in the caller's namespace, plus every namespace's pods carrying the labels —
// and the JSON golden would be the only thing standing between that and a pass.
// This states the property directly.
func TestEachPeerIsOneSelectorPairNotTwo(t *testing.T) {
	d, _ := internalDriver(t)
	np, err := d.renderNetworkPolicy(isolatedSpec("one-peer"), internalNS)
	if err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 {
		t.Fatalf("ingress = %+v, want one rule with one peer", np.Spec.Ingress)
	}
	in := np.Spec.Ingress[0].From[0]
	if in.NamespaceSelector == nil || in.PodSelector == nil || in.IPBlock != nil {
		t.Errorf("the ingress peer must carry BOTH selectors and no ipBlock: %+v", in)
	}
	if len(in.PodSelector.MatchLabels) == 0 || len(in.NamespaceSelector.MatchLabels) == 0 {
		t.Errorf("an empty selector matches everything: %+v", in)
	}
	dns := np.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector == nil || dns.To[0].PodSelector == nil || dns.To[0].IPBlock != nil {
		t.Errorf("the DNS peer must be one namespace+pod selector pair: %+v", dns.To)
	}
	// 🔴 NO EGRESS RULE MAY HAVE AN EMPTY `to` OR EMPTY `ports`: either one means
	// "everything" to the apiserver.
	for i, r := range np.Spec.Egress {
		if len(r.To) == 0 || len(r.Ports) == 0 {
			t.Errorf("egress rule %d is open-ended (to=%d ports=%d)", i, len(r.To), len(r.Ports))
		}
	}
	for i, r := range np.Spec.Ingress {
		if len(r.From) == 0 || len(r.Ports) == 0 {
			t.Errorf("ingress rule %d is open-ended (from=%d ports=%d)", i, len(r.From), len(r.Ports))
		}
	}
}

// TestAnIsolatedSpecWithNothingToAllowAllowsNothing: no declared port means no
// ingress rule at all, and no public port means no public rule — NOT a rule with
// an empty port list, which matches every port. Both policy types stay listed, or
// the direction with no rule would be unrestricted.
func TestAnIsolatedSpecWithNothingToAllowAllowsNothing(t *testing.T) {
	d, _ := internalDriver(t)
	spec := provision.Spec{
		Ref:     provision.Ref{Name: "mute-crane"},
		Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"},
		Network: provision.Network{Isolate: true},
	}
	np, err := d.renderNetworkPolicy(spec, internalNS)
	if err != nil || np == nil {
		t.Fatalf("render: %v / %v", np, err)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("a spec with no declared port rendered ingress %+v; an ingress rule with no ports admits every port", np.Spec.Ingress)
	}
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("egress = %+v, want the DNS rule alone", np.Spec.Egress)
	}
	if got := np.Spec.Egress[0].Ports; len(got) != 2 || got[0].Port.IntVal != 53 || got[1].Port.IntVal != 53 {
		t.Errorf("the one egress rule is not DNS: %+v", got)
	}
	types := fmt.Sprint(np.Spec.PolicyTypes)
	if types != "[Ingress Egress]" {
		t.Errorf("policyTypes = %s, want [Ingress Egress] even with no ingress rule — an unlisted type is an unrestricted direction", types)
	}
}

// TestASpecDeclaringNoIsolationRendersNoPolicyAndMakesNoNetworkingCall is the
// gateway kind's guarantee, on a driver that IS configured for isolation: its
// specs declare none, so Create, Update and Scale touch networking.k8s.io zero
// times and no policy exists.
//
// 🔴 THE ZERO IS REPORTED WITH ITS POSITIVE CONTROL. The same counter over the
// same driver sees an isolated spec's calls, so "0" is not a counter wired to
// nothing.
func TestASpecDeclaringNoIsolationRendersNoPolicyAndMakesNoNetworkingCall(t *testing.T) {
	ctx := context.Background()
	d, cs := internalDriver(t)
	plain := internalSpec("plain-otter")
	if np, err := d.renderNetworkPolicy(plain, internalNS); np != nil || err != nil {
		t.Fatalf("a spec declaring no isolation rendered %v / %v", np, err)
	}
	if err := d.Create(ctx, plain); err != nil {
		t.Fatalf("create: %v", err)
	}
	plain.Env = []provision.EnvVar{{Name: "CHANGED", Value: "1"}}
	if err := d.Update(ctx, plain); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := d.Scale(ctx, plain.Ref, 0); err != nil {
		t.Fatalf("scale: %v", err)
	}
	if got := networkingActions(cs); len(got) != 0 {
		t.Errorf("a spec declaring no isolation made networking calls %v; a deployment with no "+
			"networkpolicies RBAC would fail every one of its agents", got)
	}
	if n := policyCount(t, cs); n != 0 {
		t.Errorf("%d NetworkPolicy object(s) exist for a spec that declared no isolation", n)
	}

	// The positive control.
	cs.ClearActions()
	if err := d.Create(ctx, isolatedSpec("fenced-otter")); err != nil {
		t.Fatalf("control create: %v", err)
	}
	if got := networkingActions(cs); len(got) == 0 {
		t.Fatal("control: an ISOLATED spec made no networking call either, so the zero above measured nothing")
	}
	if n := policyCount(t, cs); n != 1 {
		t.Fatalf("control: %d policy object(s) after one isolated create, want 1", n)
	}
}

// TestTheNetworkPolicyIsWrittenBeforeAnythingThatRunsOrHoldsACredential pins
// apply's order: namespace, then the policy, then everything else, with the
// Deployment last.
func TestTheNetworkPolicyIsWrittenBeforeAnythingThatRunsOrHoldsACredential(t *testing.T) {
	d, cs := perInstanceDriver(t)
	if err := d.Create(context.Background(), ccSpec(t)); err != nil {
		t.Fatalf("create: %v", err)
	}
	firstCreate := map[string]int{}
	for i, a := range cs.Actions() {
		if a.GetVerb() != "create" {
			continue
		}
		if _, seen := firstCreate[a.GetResource().Resource]; !seen {
			firstCreate[a.GetResource().Resource] = i
		}
	}
	for _, r := range []string{"namespaces", "networkpolicies", "serviceaccounts", "secrets", "persistentvolumeclaims", "services", "deployments"} {
		if _, ok := firstCreate[r]; !ok {
			t.Fatalf("instrument check: no create of %s was recorded (%v)", r, firstCreate)
		}
	}
	np := firstCreate["networkpolicies"]
	if firstCreate["namespaces"] > np {
		t.Errorf("the policy was created before its namespace")
	}
	for _, later := range []string{"serviceaccounts", "secrets", "persistentvolumeclaims", "services", "deployments"} {
		if firstCreate[later] < np {
			t.Errorf("%s was created at step %d, BEFORE the NetworkPolicy at step %d: a refused policy "+
				"would leave it behind", later, firstCreate[later], np)
		}
	}
	for r, at := range firstCreate {
		if r != "deployments" && at > firstCreate["deployments"] {
			t.Errorf("%s was created after the Deployment", r)
		}
	}
}

func forbidden(verb string) k8stesting.ReactionFunc {
	return func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "",
			fmt.Errorf("fixture RBAC: the service account cannot %s this resource", verb))
	}
}

// assertNothingRunnable fails if the instance has a Deployment, a Secret or a
// ServiceAccount: the three things a refused isolated instance must not leave.
func assertNothingRunnable(t *testing.T, cs *fake.Clientset, d *Driver, ns, name string) {
	t.Helper()
	ctx := context.Background()
	deps, _ := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	secs, _ := cs.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	sas, _ := cs.CoreV1().ServiceAccounts(ns).List(ctx, metav1.ListOptions{})
	if len(deps.Items) != 0 || len(secs.Items) != 0 || len(sas.Items) != 0 {
		t.Errorf("the refused instance left %d deployment(s), %d secret(s), %d serviceaccount(s) in %s; "+
			"it must not start, and its credential must not be written", len(deps.Items), len(secs.Items), len(sas.Items), ns)
	}
	if _, err := d.Get(ctx, provision.Ref{Name: name}); !errors.Is(err, provision.ErrNotFound) {
		t.Errorf("Get after the refusal = %v, want ErrNotFound: nothing may be running", err)
	}
}

// TestAForbiddenNetworkPolicyStopsTheInstanceAndNamesTheVerb is the fail-closed
// case. Whichever of the three calls the apiserver refuses with a 403, the
// instance is not started, the error names THAT verb and the rule to add, and it
// is ErrUnsupported — not ErrBlind, which a caller retries.
func TestAForbiddenNetworkPolicyStopsTheInstanceAndNamesTheVerb(t *testing.T) {
	ctx := context.Background()
	const ns = "muster-agent-quiet-heron"
	cases := []struct {
		verb string
		// seed makes the driver reach the verb under test.
		seed func(t *testing.T, cs *fake.Clientset)
	}{
		{verb: "create", seed: func(*testing.T, *fake.Clientset) {}},
		{verb: "get", seed: func(t *testing.T, cs *fake.Clientset) {
			// An existing policy turns the create into AlreadyExists, which is
			// what makes the driver read it.
			seedOwnedPolicy(t, cs, ns, "quiet-heron")
		}},
		{verb: "update", seed: func(t *testing.T, cs *fake.Clientset) {
			seedOwnedPolicy(t, cs, ns, "quiet-heron")
		}},
	}
	for _, c := range cases {
		for _, op := range []string{"Create", "Update"} {
			t.Run(c.verb+"/"+op, func(t *testing.T) {
				d, cs := perInstanceDriver(t)
				c.seed(t, cs)
				cs.PrependReactor(c.verb, "networkpolicies", forbidden(c.verb))

				var err error
				if op == "Create" {
					err = d.Create(ctx, ccSpec(t))
				} else {
					err = d.Update(ctx, ccSpec(t))
				}
				if err == nil {
					t.Fatalf("%s succeeded although muster may not %s the NetworkPolicy", op, c.verb)
				}
				if !errors.Is(err, provision.ErrUnsupported) {
					t.Errorf("want ErrUnsupported, got %v", err)
				}
				if errors.Is(err, provision.ErrBlind) {
					t.Errorf("a 403 must NOT be ErrBlind — the backend answered, and a caller retries ErrBlind for ever: %v", err)
				}
				msg := err.Error()
				for _, want := range []string{
					"may not " + c.verb + " networkpolicies.networking.k8s.io",
					`"quiet-heron-network" in namespace "` + ns + `"`,
					"NetworkPolicy was NOT written and neither was anything after it",
					"an instance that was not running was NOT started",
					"One that was ALREADY RUNNING was left exactly as it was, and muster did not confirm it has a policy",
					"`kubectl -n " + ns + " get networkpolicy`",
					`grant muster's ClusterRole apiGroups ["networking.k8s.io"] resources ["networkpolicies"] verbs ["get", "create", "update", "delete"]`,
				} {
					if !strings.Contains(msg, want) {
						t.Errorf("the refusal does not say %q:\n  %s", want, msg)
					}
				}
				if strings.Contains(msg, fakeCCToken) {
					t.Errorf("the refusal carries the account token")
				}
				assertNothingRunnable(t, cs, d, ns, "quiet-heron")
			})
		}
	}
}

func seedOwnedPolicy(t *testing.T, cs *fake.Clientset, ns, instance string) {
	t.Helper()
	_, err := cs.NetworkingV1().NetworkPolicies(ns).Create(context.Background(), &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: networkPolicyName(instance), Namespace: ns, Labels: instanceLabels(instance)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed policy: %v", err)
	}
}

// TestANetworkPolicyFailureThatIsNotA403StillStopsTheInstance: any other
// apiserver failure on the policy is ErrBlind like every other upsert's — and
// still leaves nothing running. The fail-closed order does not depend on the
// error being the one this file has a message for.
func TestANetworkPolicyFailureThatIsNotA403StillStopsTheInstance(t *testing.T) {
	d, cs := perInstanceDriver(t)
	cs.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("fixture: apiserver overloaded")
	})
	err := d.Create(context.Background(), ccSpec(t))
	if !errors.Is(err, provision.ErrBlind) || errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("want ErrBlind alone for a non-403 failure, got %v", err)
	}
	assertNothingRunnable(t, cs, d, "muster-agent-quiet-heron", "quiet-heron")
}

// preIsolation is the spec an older build produced for the same agent: identical
// but for the Network declaration this change adds to the kind.
func preIsolation(spec provision.Spec) provision.Spec {
	spec.Network = provision.Network{}
	return spec
}

// TestAnInstanceCreatedBeforeIsolationGetsItsPolicyOnItsNextUpdate is the
// pre-existing agent: a claude-code Deployment written by a build that rendered
// no policy. Its next Update (which is what a claude-code Start is) writes the
// policy; a Create does NOT reconcile it, and says so.
func TestAnInstanceCreatedBeforeIsolationGetsItsPolicyOnItsNextUpdate(t *testing.T) {
	ctx := context.Background()
	const ns = "muster-agent-quiet-heron"
	d, cs := perInstanceDriver(t)
	now := ccSpec(t)
	old := preIsolation(now)
	if err := d.Create(ctx, old); err != nil {
		t.Fatalf("create the pre-isolation instance: %v", err)
	}
	if n := policyCount(t, cs); n != 0 {
		t.Fatalf("premise: the pre-isolation instance must have NO policy, has %d", n)
	}
	before, err := cs.AppsV1().Deployments(ns).Get(ctx, "quiet-heron", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before.Annotations[annFingerprint] == provision.Fingerprint(now) {
		t.Fatal("premise: declaring isolation must move the fingerprint, or nothing would ever reconcile this instance")
	}

	// Create is not the reconcile: it refuses the changed spec and writes nothing.
	if err := d.Create(ctx, now); !errors.Is(err, provision.ErrDivergentSpec) {
		t.Fatalf("Create over a pre-isolation instance = %v, want ErrDivergentSpec", err)
	}
	if n := policyCount(t, cs); n != 0 {
		t.Fatalf("a refused Create wrote %d policy object(s)", n)
	}

	if err := d.Update(ctx, now); err != nil {
		t.Fatalf("update: %v", err)
	}
	np := getPolicy(t, cs, ns, "quiet-heron-network")
	if got := specJSON(t, np); got != wantClaudeCodePolicySpec {
		t.Errorf("the reconciled policy is not the pinned one:\n%s", got)
	}
	after, _ := cs.AppsV1().Deployments(ns).Get(ctx, "quiet-heron", metav1.GetOptions{})
	if after.Annotations[annFingerprint] != provision.Fingerprint(now) {
		t.Errorf("the Deployment's fingerprint was not moved to the isolated spec's")
	}
}

// TestAStoppedPreIsolationInstanceIsNotStartedWhenItsPolicyIsRefused: the same
// pre-existing agent, stopped, then started (Update) on a cluster where muster
// may not write NetworkPolicies. It must stay stopped — replicas 0, its old
// fingerprint, no policy — rather than come back up unconfined.
func TestAStoppedPreIsolationInstanceIsNotStartedWhenItsPolicyIsRefused(t *testing.T) {
	ctx := context.Background()
	const ns = "muster-agent-quiet-heron"
	d, cs := perInstanceDriver(t)
	now := ccSpec(t)
	if err := d.Create(ctx, preIsolation(now)); err != nil {
		t.Fatal(err)
	}
	if err := d.Scale(ctx, now.Ref, 0); err != nil {
		t.Fatal(err)
	}
	stopped, _ := cs.AppsV1().Deployments(ns).Get(ctx, "quiet-heron", metav1.GetOptions{})
	oldFP := stopped.Annotations[annFingerprint]

	cs.PrependReactor("create", "networkpolicies", forbidden("create"))
	err := d.Update(ctx, now)
	if !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), "may not create networkpolicies") {
		t.Fatalf("update = %v, want the networkpolicies refusal", err)
	}
	dep, gerr := cs.AppsV1().Deployments(ns).Get(ctx, "quiet-heron", metav1.GetOptions{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		t.Errorf("replicas = %v after the refused start, want 0: the agent came back up with no policy", dep.Spec.Replicas)
	}
	if dep.Annotations[annFingerprint] != oldFP {
		t.Errorf("the refused start rewrote the Deployment")
	}
	if n := policyCount(t, cs); n != 0 {
		t.Errorf("%d policy object(s) after a refused create", n)
	}
}

// TestARunningPreIsolationInstanceIsLeftRunningWhenItsPolicyIsRefused pins the
// case the refusal does NOT close, so it is a decision on the page rather than a
// surprise: the pre-existing agent is UP when a reconcile reaches it, and muster
// may not write its policy. apply returns before touching any of the instance's
// own objects (its namespace already exists here), so the pod keeps running
// exactly as it was — with no policy.
//
// 🔴 THIS IS NOT "FAIL CLOSED" AND THE TEST DOES NOT SAY IT IS. Nothing here
// stops a running pod. What is pinned is narrower than "changes nothing": the
// Deployment's replica count and fingerprint are as they were (so the spec was
// not half-applied), no policy exists, and the message names this case and the
// command that checks for it.
func TestARunningPreIsolationInstanceIsLeftRunningWhenItsPolicyIsRefused(t *testing.T) {
	ctx := context.Background()
	const ns = "muster-agent-quiet-heron"
	d, cs := perInstanceDriver(t)
	now := ccSpec(t)
	if err := d.Create(ctx, preIsolation(now)); err != nil {
		t.Fatal(err)
	}
	running, _ := cs.AppsV1().Deployments(ns).Get(ctx, "quiet-heron", metav1.GetOptions{})
	if running.Spec.Replicas == nil || *running.Spec.Replicas != 1 {
		t.Fatalf("premise: the pre-isolation instance must be running, replicas = %v", running.Spec.Replicas)
	}
	oldFP := running.Annotations[annFingerprint]

	cs.PrependReactor("create", "networkpolicies", forbidden("create"))
	err := d.Update(ctx, now)
	if !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("update = %v, want the networkpolicies refusal", err)
	}
	if want := "if it predates network isolation it is STILL RUNNING WITHOUT ONE — check `kubectl -n " + ns +
		" get networkpolicy`, and stop the agent if there is none"; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not tell the operator the instance may still be running:\n  %v", err)
	}
	dep, gerr := cs.AppsV1().Deployments(ns).Get(ctx, "quiet-heron", metav1.GetOptions{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 || dep.Annotations[annFingerprint] != oldFP {
		t.Errorf("the refused update changed the running instance's Deployment (replicas %v, fingerprint moved %v)",
			dep.Spec.Replicas, dep.Annotations[annFingerprint] != oldFP)
	}
	if n := policyCount(t, cs); n != 0 {
		t.Errorf("%d policy object(s) after a refused create", n)
	}
}

// TestUpdateRestoresAMissingOrAlteredPolicy: the policy is reconciled like the
// instance's other objects — an Update puts back one that was deleted out of
// band, and overwrites one that was widened.
func TestUpdateRestoresAMissingOrAlteredPolicy(t *testing.T) {
	ctx := context.Background()
	const ns = "muster-agent-quiet-heron"
	d, cs := perInstanceDriver(t)
	spec := ccSpec(t)
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	api := cs.NetworkingV1().NetworkPolicies(ns)

	if err := api.Delete(ctx, "quiet-heron-network", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := policyCount(t, cs); n != 0 {
		t.Fatalf("premise: the policy was deleted, %d remain", n)
	}
	if err := d.Update(ctx, spec); err != nil {
		t.Fatalf("update after delete: %v", err)
	}
	if got := specJSON(t, getPolicy(t, cs, ns, "quiet-heron-network")); got != wantClaudeCodePolicySpec {
		t.Errorf("the restored policy is not the pinned one:\n%s", got)
	}

	// Widened out of band: every egress rule replaced by allow-all.
	wide := getPolicy(t, cs, ns, "quiet-heron-network").DeepCopy()
	wide.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	wide.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
	if _, err := api.Update(ctx, wide, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := specJSON(t, getPolicy(t, cs, ns, "quiet-heron-network")); got == wantClaudeCodePolicySpec {
		t.Fatal("premise: the out-of-band edit did not change the policy")
	}
	if err := d.Update(ctx, spec); err != nil {
		t.Fatalf("update after widening: %v", err)
	}
	if got := specJSON(t, getPolicy(t, cs, ns, "quiet-heron-network")); got != wantClaudeCodePolicySpec {
		t.Errorf("Update left a widened policy in place:\n%s", got)
	}
}

// TestASpecThatStopsDeclaringIsolationKeepsItsPolicy pins the one sweep apply
// deliberately does NOT do: a reconcile never removes confinement.
func TestASpecThatStopsDeclaringIsolationKeepsItsPolicy(t *testing.T) {
	ctx := context.Background()
	d, cs := internalDriver(t)
	spec := isolatedSpec("kept-wren")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	spec.Network = provision.Network{}
	if err := d.Update(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if n := policyCount(t, cs); n != 1 {
		t.Errorf("%d policy object(s) after the spec dropped isolation, want the 1 it had: a reconcile "+
			"must not remove confinement", n)
	}
}

// TestDestroyRemovesTheNetworkPolicyLast: the policy is deleted with the agent,
// in both layouts, and after the Deployment's delete was issued.
func TestDestroyRemovesTheNetworkPolicyLast(t *testing.T) {
	ctx := context.Background()
	for _, layout := range []struct {
		name  string
		build func(*testing.T) (*Driver, *fake.Clientset)
	}{
		{"namespace-per-instance", perInstanceDriver},
		{"shared-namespace", internalDriver},
	} {
		t.Run(layout.name, func(t *testing.T) {
			d, cs := layout.build(t)
			if err := d.Create(ctx, isolatedSpec("brief-lark")); err != nil {
				t.Fatal(err)
			}
			if n := policyCount(t, cs); n != 1 {
				t.Fatalf("premise: %d policy object(s) after create, want 1", n)
			}
			cs.ClearActions()
			if err := d.Destroy(ctx, provision.Ref{Name: "brief-lark"}); err != nil {
				t.Fatalf("destroy: %v", err)
			}
			if n := policyCount(t, cs); n != 0 {
				t.Errorf("%d policy object(s) survived Destroy", n)
			}
			depDelete, npDelete := -1, -1
			for i, a := range cs.Actions() {
				if a.GetVerb() != "delete" {
					continue
				}
				switch a.GetResource().Resource {
				case "deployments":
					depDelete = i
				case "networkpolicies":
					npDelete = i
				}
			}
			if depDelete < 0 || npDelete < 0 {
				t.Fatalf("instrument check: deployment delete at %d, policy delete at %d", depDelete, npDelete)
			}
			if npDelete < depDelete {
				t.Errorf("the policy was deleted at step %d, before the Deployment at step %d: the pod's "+
					"last seconds would be unconfined", npDelete, depDelete)
			}
			for i, a := range cs.Actions() {
				if a.GetVerb() == "delete" && i > npDelete && a.GetResource().Resource != "namespaces" {
					t.Errorf("%s was deleted after the NetworkPolicy; only the namespace may follow it", a.GetResource().Resource)
				}
			}
		})
	}
}

// TestDestroyLeavesAStrangersCoNamedPolicyAlone: the delete is ownership-checked
// like every other satellite's.
func TestDestroyLeavesAStrangersCoNamedPolicyAlone(t *testing.T) {
	ctx := context.Background()
	d, cs := internalDriver(t)
	if err := d.Create(ctx, internalSpec("shy-finch")); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.NetworkingV1().NetworkPolicies(internalNS).Create(ctx,
		&networkingv1.NetworkPolicy{ObjectMeta: internalForeignMeta("shy-finch-network")}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, provision.Ref{Name: "shy-finch"}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if n := policyCount(t, cs); n != 1 {
		t.Errorf("Destroy removed a NetworkPolicy muster did not create (%d remain, want 1)", n)
	}
}

// TestDestroyDoesNotReportSuccessOverAPolicyItCouldNotRemove: Destroy's nil means
// removed or already absent. A NetworkPolicy the apiserver would not let muster
// delete is neither, so the call reports it — after the Deployment's delete was
// issued, because stopping the workload does not wait on tidying.
//
// 🔴 THE SECOND CASE IS A COST, PINNED SO IT IS NOT DISCOVERED: on a driver
// CONFIGURED for isolation, Destroy reads networkpolicies for EVERY instance —
// it has no spec to tell it which ones were isolated. So on such a deployment a
// missing RBAC rule fails the destroy of an instance that never had a policy.
// It fails loudly and removes the workload first; it does not report success.
func TestDestroyDoesNotReportSuccessOverAPolicyItCouldNotRemove(t *testing.T) {
	ctx := context.Background()
	deploymentGone := func(t *testing.T, cs *fake.Clientset, name string) {
		t.Helper()
		if _, err := cs.AppsV1().Deployments(internalNS).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("the Deployment was not removed before the policy failure was reported (get: %v)", err)
		}
	}

	t.Run("an isolated instance whose policy cannot be deleted", func(t *testing.T) {
		d, cs := internalDriver(t)
		if err := d.Create(ctx, isolatedSpec("stuck-heron")); err != nil {
			t.Fatal(err)
		}
		cs.PrependReactor("delete", "networkpolicies", forbidden("delete"))
		err := d.Destroy(ctx, provision.Ref{Name: "stuck-heron"})
		if err == nil || !strings.Contains(err.Error(), "delete networkpolicy stuck-heron-network") {
			t.Fatalf("destroy = %v, want an error naming the policy it could not delete", err)
		}
		deploymentGone(t, cs, "stuck-heron")
		if n := policyCount(t, cs); n != 1 {
			t.Fatalf("instrument check: the refused delete left %d policy object(s), want 1", n)
		}
	})

	t.Run("an unisolated instance on a configured driver that may not read policies", func(t *testing.T) {
		d, cs := internalDriver(t)
		if err := d.Create(ctx, internalSpec("plain-heron")); err != nil {
			t.Fatal(err)
		}
		cs.PrependReactor("get", "networkpolicies", forbidden("get"))
		err := d.Destroy(ctx, provision.Ref{Name: "plain-heron"})
		if err == nil || !strings.Contains(err.Error(), "delete networkpolicy plain-heron-network") {
			t.Fatalf("destroy = %v, want an error naming the policy read that was refused", err)
		}
		// The instance never had a policy, so the error has to say what to DO:
		// "networkpolicies is forbidden" alone reads as a non sequitur here.
		if want := `grant muster's ClusterRole apiGroups ["networking.k8s.io"] resources ["networkpolicies"] verbs ["get", "create", "update", "delete"]`; !strings.Contains(err.Error(), want) {
			t.Errorf("the destroy failure does not name the rule to add:\n  %v", err)
		}
		deploymentGone(t, cs, "plain-heron")
	})
}

// TestADriverNotConfiguredForIsolationRefusesItAndNeverTouchesNetworking: with no
// Config.NetworkPolicy the capability is off, an isolated spec is refused before
// anything is written, and Destroy makes no networking call — a deployment that
// enables no isolated kind needs no networkpolicies RBAC at all.
func TestADriverNotConfiguredForIsolationRefusesItAndNeverTouchesNetworking(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	d, err := New(Config{Client: cs, Namespace: internalNS})
	if err != nil {
		t.Fatal(err)
	}
	if d.Capabilities().NetworkIsolation {
		t.Fatal("NetworkIsolation is claimed with no Config.NetworkPolicy")
	}
	if on, _ := internalDriver(t); !on.Capabilities().NetworkIsolation {
		t.Fatal("control: a configured driver does not claim NetworkIsolation")
	}

	// The CAPABILITY refusal, by its own words: renderNetworkPolicy has a guard of
	// its own that also says "network isolation", and it fires one API call later.
	err = d.Create(ctx, isolatedSpec("bare-swift"))
	if !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), "driver cannot isolate an instance's network") {
		t.Fatalf("create = %v, want the capability refusal", err)
	}
	if len(cs.Actions()) != 0 {
		t.Errorf("a refused spec reached the cluster: %d action(s)", len(cs.Actions()))
	}
	// renderNetworkPolicy's own guard, which CheckSpec normally keeps unreachable:
	// it must be an error, never a nil policy read as "this spec asks for none".
	if np, rerr := d.renderNetworkPolicy(isolatedSpec("bare-swift"), internalNS); np != nil || !errors.Is(rerr, provision.ErrUnsupported) {
		t.Errorf("render without a configuration = %v / %v, want nil and ErrUnsupported", np, rerr)
	}

	if err := d.Create(ctx, internalSpec("bare-swift")); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, provision.Ref{Name: "bare-swift"}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := networkingActions(cs); len(got) != 0 {
		t.Errorf("an unconfigured driver made networking calls %v", got)
	}
	// The positive control for that zero: the configured driver's Destroy does.
	on, onCS := internalDriver(t)
	if err := on.Create(ctx, internalSpec("bare-swift")); err != nil {
		t.Fatal(err)
	}
	onCS.ClearActions()
	if err := on.Destroy(ctx, provision.Ref{Name: "bare-swift"}); err != nil {
		t.Fatal(err)
	}
	if got := networkingActions(onCS); len(got) == 0 {
		t.Fatal("control: the configured driver's Destroy made no networking call either")
	}
}

// TestNewRefusesANetworkPolicyConfigurationThatAdmitsTooMuch: an empty caller
// namespace or an empty caller pod selector is refused at construction — the
// second would select every pod in the caller's namespace.
func TestNewRefusesANetworkPolicyConfigurationThatAdmitsTooMuch(t *testing.T) {
	ok := func() *NetworkPolicyConfig { return internalNetworkPolicy() }
	if _, err := New(Config{Client: fake.NewClientset(), Namespace: internalNS, NetworkPolicy: ok()}); err != nil {
		t.Fatalf("control: the valid configuration is refused: %v", err)
	}
	for name, c := range map[string]struct {
		mut  func(*NetworkPolicyConfig)
		want string
	}{
		"no namespace":    {func(c *NetworkPolicyConfig) { c.ControllerNamespace = " " }, "ControllerNamespace is required"},
		"no labels":       {func(c *NetworkPolicyConfig) { c.ControllerPodLabels = nil }, "must not be empty"},
		"empty label map": {func(c *NetworkPolicyConfig) { c.ControllerPodLabels = map[string]string{} }, "must not be empty"},
		"empty value":     {func(c *NetworkPolicyConfig) { c.ControllerPodLabels = map[string]string{"app": ""} }, "empty key or value"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := ok()
			c.mut(cfg)
			_, err := New(Config{Client: fake.NewClientset(), Namespace: internalNS, NetworkPolicy: cfg})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// TestTheRBACPrerequisiteIsTheVerbsTheDriverCalls: the declared prerequisite is
// exactly the verbs the driver uses on networkpolicies across an instance's whole
// life — applyNetworkPolicy's and Destroy's — measured off the fake's action log,
// so the rule an operator is told to add cannot drift from what the code calls.
func TestTheRBACPrerequisiteIsTheVerbsTheDriverCalls(t *testing.T) {
	ctx := context.Background()
	d, cs := internalDriver(t)
	spec := isolatedSpec("whole-life")
	if err := d.Create(ctx, spec); err != nil { // create
		t.Fatal(err)
	}
	if err := d.Update(ctx, spec); err != nil { // create -> AlreadyExists -> get -> update
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, spec.Ref); err != nil { // get -> delete
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, v := range networkingActions(cs) {
		used[v] = true
	}
	declared := map[string]bool{}
	for _, v := range NetworkPolicyRBACPrerequisite {
		declared[v] = true
	}
	if len(used) == 0 {
		t.Fatal("instrument check: no networking call was recorded")
	}
	for v := range used {
		if !declared[v] {
			t.Errorf("the driver calls %q on networkpolicies and NetworkPolicyRBACPrerequisite does not declare it", v)
		}
	}
	for v := range declared {
		if !used[v] {
			t.Errorf("NetworkPolicyRBACPrerequisite declares %q, which a create/update/destroy cycle never calls", v)
		}
	}
}
