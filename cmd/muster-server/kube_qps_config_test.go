package main

import (
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// TestAgentKubeQPSIsReadDefaultedAndBounded: MUSTER_AGENT_KUBE_QPS reaches
// the config — 50 unset, the integer when set within 1-1000 — and anything
// else refuses to boot, naming the variable. The rest.Config the kubernetes
// driver's client is built from then carries it, with a burst of twice it:
// never client-go's own 5/10, which a rest.Config with QPS 0 gets.
func TestAgentKubeQPSIsReadDefaultedAndBounded(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	load := func(v string, set bool) (config, error) {
		return loadConfig(func(k string) string {
			if k == envAgentKubeQPS && set {
				return v
			}
			return base[k]
		})
	}
	for _, c := range []struct {
		v         string
		set       bool
		want      int
		wantQPS   float32
		wantBurst int
	}{
		{"", false, 50, 50, 100},
		{"", true, 50, 50, 100},
		{"7", true, 7, 7, 14},
		{" 120 ", true, 120, 120, 240},
		{"1", true, 1, 1, 2},
		{"1000", true, 1000, 1000, 2000},
	} {
		cfg, err := load(c.v, c.set)
		if err != nil {
			t.Fatalf("%s=%q refused: %v", envAgentKubeQPS, c.v, err)
		}
		if cfg.AgentKubeQPS != c.want {
			t.Errorf("%s=%q (set %v) = %d, want %d", envAgentKubeQPS, c.v, c.set, cfg.AgentKubeQPS, c.want)
		}
		rc := &rest.Config{}
		kubeClientRateLimit(rc, cfg.AgentKubeQPS)
		if rc.QPS != c.wantQPS || rc.Burst != c.wantBurst {
			t.Errorf("%s=%q: the client's rest.Config has QPS %v burst %d, want %v and %d",
				envAgentKubeQPS, c.v, rc.QPS, rc.Burst, c.wantQPS, c.wantBurst)
		}
	}
	for _, bad := range []string{"0", "-5", "1001", "2.5", "fast"} {
		if _, err := load(bad, true); err == nil || !strings.Contains(err.Error(), envAgentKubeQPS) {
			t.Errorf("%s=%q: err = %v, want a refusal naming the variable", envAgentKubeQPS, bad, err)
		}
	}
	// A config built without loadConfig (tests build them) still gets the
	// default, not client-go's 5.
	rc := &rest.Config{}
	kubeClientRateLimit(rc, 0)
	if rc.QPS != 50 || rc.Burst != 100 {
		t.Errorf("unset AgentKubeQPS gives QPS %v burst %d, want 50 and 100", rc.QPS, rc.Burst)
	}
}
