package privilege

import "testing"

// TestSpecHasRBAC covers the one pure decision in this package: whether a
// profile spec grants any live-applied Kubernetes RBAC (vs. env/kubeconfig only,
// which are applied at next dispatch). The RBAC applier keys off this.
func TestSpecHasRBAC(t *testing.T) {
	rule := PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}
	tests := []struct {
		name string
		spec Spec
		want bool
	}{
		{"empty spec", Spec{}, false},
		{"env only", Spec{Env: []EnvVar{{Name: "K", Value: "V"}}}, false},
		{"kubeconfig only", Spec{KubeconfigSecret: "kc"}, false},
		{"cluster rule", Spec{ClusterRules: []PolicyRule{rule}}, true},
		{"namespace rule", Spec{NamespaceRules: []PolicyRule{rule}}, true},
		{"both rule kinds", Spec{ClusterRules: []PolicyRule{rule}, NamespaceRules: []PolicyRule{rule}}, true},
		{"env plus rule still has rbac", Spec{Env: []EnvVar{{Name: "K"}}, NamespaceRules: []PolicyRule{rule}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.HasRBAC(); got != tt.want {
				t.Errorf("Spec.HasRBAC() = %v, want %v", got, tt.want)
			}
		})
	}
}
