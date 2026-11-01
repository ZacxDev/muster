package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestTheNamespaceClusterRoleIsRefusedWhereNothingWouldActOnIt(t *testing.T) {
	ok := provisionerTestConfig(provisionerK8s)
	ok.AgentNamespaceClusterRole = "muster-agent-namespace"
	if err := ok.validateNamespaceClusterRole(); err != nil {
		t.Fatalf("kubernetes, per-instance: %v", err)
	}
	unset := provisionerTestConfig(provisionerNoop)
	if err := unset.validateNamespaceClusterRole(); err != nil {
		t.Fatalf("unset: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*config)
		want string
	}{
		{"noop driver", func(c *config) { c.AgentProvisioner = provisionerNoop }, "only the kubernetes driver"},
		{"shared layout", func(c *config) { c.AgentNamespaceShared, c.AgentNamespace = true, "agents" }, envAgentNSShared},
		{"not a name", func(c *config) { c.AgentNamespaceClusterRole = "a/b" }, "not a ClusterRole name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.edit(&c)
			err := c.validateNamespaceClusterRole()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), envAgentNSClusterRole) {
				t.Fatalf("got %v, want a refusal naming %s and %q", err, envAgentNSClusterRole, tc.want)
			}
			if verr := c.validateProvisioner(); verr == nil || !strings.Contains(verr.Error(), envAgentNSClusterRole) {
				t.Fatalf("validateProvisioner = %v, want the same refusal reached at boot", verr)
			}
		})
	}
}

func TestTheNamespaceClusterRoleReachesTheDriver(t *testing.T) {
	cfg := provisionerTestConfig(provisionerK8s)
	if dc := k8sDriverConfig(cfg, nil); dc.NamespaceRoleBinding != nil {
		t.Fatalf("unset: NamespaceRoleBinding = %+v, want nil (today's layout)", dc.NamespaceRoleBinding)
	}
	cfg.AgentNamespaceClusterRole = "role-x7"
	dc := k8sDriverConfig(cfg, nil)
	if dc.NamespaceRoleBinding == nil || dc.NamespaceRoleBinding.ClusterRole != "role-x7" {
		t.Fatalf("set: NamespaceRoleBinding = %+v, want ClusterRole role-x7", dc.NamespaceRoleBinding)
	}
	env := map[string]string{
		envAgentProvisioner:   "kubernetes",
		envAgentImageRepo:     "registry.example.test/muster/agent-runtime",
		envAgentAPIURL:        "http://muster.example.test:8105",
		envAgentNSClusterRole: " role-x7 ",
	}
	loaded, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AgentNamespaceClusterRole != "role-x7" {
		t.Fatalf("loaded %q, want role-x7 (trimmed)", loaded.AgentNamespaceClusterRole)
	}
}

func jwt(payload string) []byte {
	enc := base64.RawURLEncoding.EncodeToString
	return []byte(enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(payload)) + ".c2ln\n")
}

func TestTheRoleBindingSubjectIsReadOffThePodsOwnToken(t *testing.T) {
	ns, name, err := serviceAccountFromToken(jwt(`{"sub":"system:serviceaccount:ops-3:muster-sa"}`))
	if err != nil || ns != "ops-3" || name != "muster-sa" {
		t.Fatalf("= %q, %q, %v; want ops-3, muster-sa", ns, name, err)
	}
	for _, bad := range [][]byte{
		[]byte("not-a-jwt"),
		jwt(`{"sub":"system:node:worker-1"}`),
		jwt(`{"sub":"system:serviceaccount:ops-3"}`),
		jwt(`{"sub":"system:serviceaccount::muster-sa"}`),
		jwt(`{"sub":"system:serviceaccount:a:b:c"}`),
		jwt(`not json`),
	} {
		if _, _, err := serviceAccountFromToken(bad); err == nil {
			t.Errorf("%q: accepted, want a refusal", bad)
		}
	}
}
