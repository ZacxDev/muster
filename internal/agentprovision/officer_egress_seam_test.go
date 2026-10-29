package agentprovision

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// The officer egress lock, across the seam: the session source internal/api
// installs (UseClaudeCodeSession) -> agentspec -> the REAL kubernetes driver's
// NetworkPolicy. Each half has its own unit test; only here is the policy an
// OFFICER's pod gets actually rendered.
//
// ⚠ A FAKE CLIENTSET: this pins what muster WRITES, not what any network plugin
// enforces.

// egressSeamConnectPort is muster's connect listener in these fixtures. Distinct
// from every other number in the policy (53, 443, 18789) so a render that
// hardcoded one of them cannot pass.
const egressSeamConnectPort = 18191

func egressSeamAdapter(t *testing.T, row agents.Agent, officer *bool) (*Adapter, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	empty := ""
	driver, err := k8sdriver.New(k8sdriver.Config{
		Client: cs, NamespacePerInstance: true, NamespacePrefix: "muster-agent-", WorkspaceStorageClass: &empty,
		NetworkPolicy: &k8sdriver.NetworkPolicyConfig{
			ControllerNamespace: "control-plane-7",
			ControllerPodLabels: map[string]string{"app": "muster-server"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := seamSpecConfig()
	cfg.ConnectURL, cfg.ConnectPort = "http://muster.example:18191", egressSeamConnectPort
	a, err := New(Config{Driver: driver, Store: &recordingStore{agent: row}, Spec: cfg, KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	// The officer runtime's session source, reduced to the one field that
	// matters here; *officer is read at every build, so a test can flip an
	// agent from "task" to "officer" between two applies.
	a.UseClaudeCodeSession(func(agents.Agent) (agentspec.ClaudeCodeSession, bool) {
		if !*officer {
			return agentspec.ClaudeCodeSession{}, false
		}
		return agentspec.ClaudeCodeSession{AllowMCPTools: []string{"officer_claim"}, BypassPermissions: true}, true
	})
	return a, cs
}

func egressSeamPolicy(t *testing.T, cs *fake.Clientset) string {
	t.Helper()
	np, err := cs.NetworkingV1().NetworkPolicies(seamNamespace).Get(context.Background(), fixtureAgentName+"-network", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the agent's networkpolicy: %v", err)
	}
	b, err := json.MarshalIndent(np.Spec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wantTaskAgentPolicy is a claude-code TASK agent's policy on a deployment that
// offers the connect port. 🔴 THE BASELINE: captured from origin/main
// (556dd1a0) before this change and pinned here unchanged, so "task agents are
// unaffected" is a byte comparison, not a reading.
const wantTaskAgentPolicy = `{
  "podSelector": {
    "matchLabels": {
      "app.kubernetes.io/instance": "harbour-kestrel",
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
              "app": "muster-server"
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
              "app": "muster-server"
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
  "policyTypes": [
    "Ingress",
    "Egress"
  ]
}`

// wantOfficerPolicy is an officer instance's policy, WRITTEN BY HAND FROM THE
// REQUIREMENT: the task agent's, minus the public-443 ipBlock rule AND minus the
// port-53 rule to the cluster's DNS pods. Ingress unchanged; egress = muster's
// connect port on muster's own pods, and nothing else. DNS comes only from the
// cluster's name-filtered policy (an L4 :53 allow here would union it open).
const wantOfficerPolicy = `{
  "podSelector": {
    "matchLabels": {
      "app.kubernetes.io/instance": "harbour-kestrel",
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
              "app": "muster-server"
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
          "protocol": "TCP",
          "port": 18191
        }
      ],
      "to": [
        {
          "podSelector": {
            "matchLabels": {
              "app": "muster-server"
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
  "policyTypes": [
    "Ingress",
    "Egress"
  ]
}`

// TestAnOfficerInstanceIsWrittenAPolicyWithNoPublicEgress pins both policies
// WHOLE: an officer instance's has no public-443 rule and no DNS rule; a task agent's on the
// same deployment is byte-identical to the pre-change render.
func TestAnOfficerInstanceIsWrittenAPolicyWithNoPublicEgress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		officer bool
		want    string
	}{
		{"officer instance", true, wantOfficerPolicy},
		{"task agent", false, wantTaskAgentPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			officer := tc.officer
			a, cs := egressSeamAdapter(t, seamRow(), &officer)
			if err := a.Dispatch(context.Background(), fixtureAgentID, true); err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if got := egressSeamPolicy(t, cs); got != tc.want {
				t.Fatalf("policy:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestARunningOfficerIsLockedByStopThenStart is the documented rollout step: an
// officer instance created by a build that wrote the public rule keeps it until
// its spec is re-applied, and a Stop -> Start (what the officer wake loop does
// after its Deployment is scaled to 0) re-writes it locked.
func TestARunningOfficerIsLockedByStopThenStart(t *testing.T) {
	ctx := context.Background()
	officer := false // the old build: no lock for anyone
	row := seamRow()
	a, cs := egressSeamAdapter(t, row, &officer)
	if err := a.Dispatch(ctx, fixtureAgentID, true); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := egressSeamPolicy(t, cs); got != wantTaskAgentPolicy {
		t.Fatalf("premise: the old build's policy is not the public-443 one:\n%s", got)
	}
	officer = true // the new build
	if got := egressSeamPolicy(t, cs); got != wantTaskAgentPolicy {
		t.Fatal("the policy changed without an apply")
	}
	if err := a.Stop(ctx, fixtureAgentID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := a.Start(ctx, fixtureAgentID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := egressSeamPolicy(t, cs); got != wantOfficerPolicy {
		t.Fatalf("after stop/start the officer's policy is:\n%s\nwant:\n%s", got, wantOfficerPolicy)
	}
}
