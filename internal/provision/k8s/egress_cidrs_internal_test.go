package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// The tests for Network.EgressCIDRs (network.go): named public blocks, one
// rule per (block, port).
//
// 🔴 LIKE EVERY TEST IN THIS PACKAGE THEY SEE THE OBJECT, NOT ENFORCEMENT: the
// fake clientset applies a NetworkPolicy to nothing.

// wantOfficerEgress is the egress list of an officer-shaped spec (no public
// rule, no DNS, the connect port, and three named blocks) — WRITTEN BY HAND
// from the requirement: each block alone in its rule with its own port, sorted
// by block then port, no except, and the controller rule last. The fixture
// lists the blocks out of order and gives one block two ports, and one port is
// not 443, so a render that hardcoded 443, merged rules, or kept input order
// cannot produce this.
const wantOfficerEgress = `[
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
          "cidr": "153.61.224.0/22"
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
          "cidr": "160.79.104.0/23"
        }
      }
    ]
  },
  {
    "ports": [
      {
        "protocol": "TCP",
        "port": 8443
      }
    ],
    "to": [
      {
        "ipBlock": {
          "cidr": "160.79.104.0/23"
        }
      }
    ]
  },
  {
    "ports": [
      {
        "protocol": "TCP",
        "port": 18191
      }
    ],
    "to": [
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
]`

func officerEgressSpec(name string) provision.Spec {
	return provision.Spec{
		Ref:     provision.Ref{Name: name},
		Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"},
		Network: provision.Network{Isolate: true, ControllerEgressTCPPorts: []int{18191}, NoClusterDNS: true,
			EgressCIDRs: []provision.CIDRPort{
				{CIDR: "160.79.104.0/23", Port: 8443},
				{CIDR: "160.79.104.0/23", Port: 443},
				{CIDR: "153.61.224.0/22", Port: 443},
			}},
	}
}

func egressJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEgressCIDRsRenderExactlyTheNamedBlocksAndPorts: an officer-shaped spec's
// egress is exactly its named blocks (one rule each, sorted) and the connect
// port — no DNS, no public block — as the object the driver WROTE.
func TestEgressCIDRsRenderExactlyTheNamedBlocksAndPorts(t *testing.T) {
	d, cs := perInstanceDriver(t)
	if err := d.Create(context.Background(), officerEgressSpec("calm-osprey")); err != nil {
		t.Fatalf("create: %v", err)
	}
	np := getPolicy(t, cs, "muster-agent-calm-osprey", "calm-osprey-network")
	if got := egressJSON(t, np.Spec.Egress); got != wantOfficerEgress {
		t.Errorf("egress is not the pinned one.\n--- got ---\n%s\n--- want ---\n%s", got, wantOfficerEgress)
	}
}

// TestEgressCIDRsWithDNSAddThePort53RuleFirst: the same spec with cluster DNS
// kept renders the DNS rule ahead of the blocks, and nothing else changes.
func TestEgressCIDRsWithDNSAddThePort53RuleFirst(t *testing.T) {
	d, _ := internalDriver(t)
	spec := officerEgressSpec("calm-osprey")
	spec.Network.NoClusterDNS = false
	np, err := d.renderNetworkPolicy(spec, internalNS)
	if err != nil || np == nil {
		t.Fatalf("render: %v / %v", np, err)
	}
	if len(np.Spec.Egress) != 5 || np.Spec.Egress[0].Ports[0].Port.IntVal != 53 {
		t.Fatalf("want the DNS rule then the four others, got %s", egressJSON(t, np.Spec.Egress))
	}
	if got := egressJSON(t, np.Spec.Egress[1:]); got != wantOfficerEgress {
		t.Errorf("the rules after DNS moved.\n--- got ---\n%s\n--- want ---\n%s", got, wantOfficerEgress)
	}
}

// TestASpecWithoutEgressCIDRsRendersAsBefore: the same spec with no blocks
// renders only the controller rule — no empty ipBlock rule, which would match
// every address. (The claude-code golden, TestTheClaudeCodeAgentGetsExactly
// ThisNetworkPolicy, pins the unchanged whole policy for a real spec.)
func TestASpecWithoutEgressCIDRsRendersAsBefore(t *testing.T) {
	d, _ := internalDriver(t)
	spec := officerEgressSpec("calm-osprey")
	spec.Network.EgressCIDRs = nil
	np, err := d.renderNetworkPolicy(spec, internalNS)
	if err != nil || np == nil {
		t.Fatalf("render: %v / %v", np, err)
	}
	if len(np.Spec.Egress) != 1 || np.Spec.Egress[0].To[0].IPBlock != nil || np.Spec.Egress[0].Ports[0].Port.IntVal != 18191 {
		t.Fatalf("want the controller rule alone, got %s", egressJSON(t, np.Spec.Egress))
	}
}

// TestCreateRefusesANonPublicEgressCIDR: the driver path goes through
// Spec.Validate, so a block reaching the cluster's own range is refused before
// anything is written.
func TestCreateRefusesANonPublicEgressCIDR(t *testing.T) {
	d, cs := perInstanceDriver(t)
	spec := officerEgressSpec("calm-osprey")
	spec.Network.EgressCIDRs = []provision.CIDRPort{{CIDR: "10.244.0.0/16", Port: 443}}
	err := d.Create(context.Background(), spec)
	if !errors.Is(err, provision.ErrInvalidSpec) || !strings.Contains(err.Error(), "overlaps the non-public range 10.0.0.0/8") {
		t.Fatalf("a block inside 10.0.0.0/8: err = %v, want ErrInvalidSpec naming the range", err)
	}
	if n := policyCount(t, cs); n != 0 {
		t.Fatalf("%d policies written for a refused spec", n)
	}
}
