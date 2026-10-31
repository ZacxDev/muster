package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// The rendered half of a pool's egress lock: a REAL worker (agentspec builds
// it, this driver writes it) in a deployment where pool "locked-review" is
// listed and pool "private-review" has a deploy key.
//
// 🔴 WHAT THESE CANNOT SEE: the operator's cluster policy, which is what
// allows a locked worker anything. Here only muster's half is pinned — that its
// own policy allows nothing public and no DNS, and that the pod carries the
// labels such a policy selects.

func lockedPoolSpec(t *testing.T, name, pool, repo string) provision.Spec {
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
			EgressLockedPools: []string{"locked-review"},
		},
	}, agentspec.Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return spec
}

// TestALockedWorkersPolicyAllowsNoDNSAndNoPublicAddress: for both kinds of
// locked pool, the written NetworkPolicy keeps its ingress rule and both
// policy types and has NO egress rule (no connect port is configured here, so
// not even the controller rule), and the pod template carries
// muster.agent/pool=<pool> and muster.agent/egress=locked.
func TestALockedWorkersPolicyAllowsNoDNSAndNoPublicAddress(t *testing.T) {
	for _, c := range []struct{ name, pool, repo string }{
		{"listed-heron", "locked-review", ""},
		{"private-heron", "private-review", "acme/private-app"},
	} {
		t.Run(c.pool, func(t *testing.T) {
			d, cs := perInstanceDriver(t)
			ctx := context.Background()
			if c.repo != "" {
				// The deploy-key worker's Secret: this driver copies nothing, so it
				// is the operator's to place; the pod only references it.
				d.cfg.SecretSourceNamespace = ""
			}
			if err := d.Create(ctx, lockedPoolSpec(t, c.name, c.pool, c.repo)); err != nil {
				t.Fatalf("create: %v", err)
			}
			ns := "muster-agent-" + c.name
			np := getPolicy(t, cs, ns, c.name+"-network")
			if len(np.Spec.Egress) != 0 {
				t.Errorf("a locked worker's NetworkPolicy has %d egress rule(s), want none:\n%s", len(np.Spec.Egress), specJSON(t, np))
			}
			if len(np.Spec.Ingress) != 1 || len(np.Spec.PolicyTypes) != 2 {
				t.Errorf("ingress %d, policyTypes %v: want the ingress rule and both types kept", len(np.Spec.Ingress), np.Spec.PolicyTypes)
			}
			dep, err := cs.AppsV1().Deployments(ns).Get(ctx, c.name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			l := dep.Spec.Template.Labels
			if l["muster.agent/pool"] != c.pool || l["muster.agent/egress"] != "locked" {
				t.Errorf("pod template labels %v: want muster.agent/pool=%s and muster.agent/egress=locked", l, c.pool)
			}
			// The selector is the instance's own, so a label added here cannot
			// make the (immutable) Deployment selector change.
			if _, ok := dep.Spec.Selector.MatchLabels["muster.agent/egress"]; ok {
				t.Error("the Deployment selector carries the egress label")
			}
		})
	}
}

// TestAnUnlockedPoolWorkerIsByteIdentical: a worker of a pool that is neither
// listed nor keyed, in that same deployment, gets the pinned claude-code
// NetworkPolicy byte for byte (wantClaudeCodePolicySpec) and exactly the pod
// template labels a claude-code pod had before the lock (no pool label).
// The control: a locked worker's policy differs from the golden.
func TestAnUnlockedPoolWorkerIsByteIdentical(t *testing.T) {
	d, cs := perInstanceDriver(t)
	ctx := context.Background()
	if err := d.Create(ctx, lockedPoolSpec(t, "quiet-heron", "pr-review", "")); err != nil {
		t.Fatalf("create: %v", err)
	}
	ns := "muster-agent-quiet-heron"
	if got := specJSON(t, getPolicy(t, cs, ns, "quiet-heron-network")); got != wantClaudeCodePolicySpec {
		t.Errorf("an unlocked pool's NetworkPolicy changed.\n--- got ---\n%s\n--- want ---\n%s", got, wantClaudeCodePolicySpec)
	}
	got, _ := cs.AppsV1().Deployments(ns).Get(ctx, "quiet-heron", metav1.GetOptions{})
	// Written by hand: the labels every claude-code pod carried before the lock
	// existed. Not derived from a second build, which would share a mutation.
	want := map[string]string{
		"app.kubernetes.io/instance":   "quiet-heron",
		"app.kubernetes.io/managed-by": "muster",
		"app.kubernetes.io/name":       "muster-agent",
		"muster.agent/name":            "quiet-heron",
		"muster.agent/group":           "agents",
		"muster.agent/kind":            "claude-code",
	}
	if !mapsEqual(got.Spec.Template.Labels, want) {
		t.Errorf("unlocked pod labels %v, want exactly %v", got.Spec.Template.Labels, want)
	}

	// Same name, so only the lock can make it differ from the golden (which
	// pins quiet-heron's selector).
	d3, cs3 := perInstanceDriver(t)
	if err := d3.Create(ctx, lockedPoolSpec(t, "quiet-heron", "locked-review", "")); err != nil {
		t.Fatal(err)
	}
	if specJSON(t, getPolicy(t, cs3, ns, "quiet-heron-network")) == wantClaudeCodePolicySpec {
		t.Fatal("control: a locked worker's policy equals the golden too, so the equality above proves nothing")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
