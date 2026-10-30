package agentspec

import (
	"reflect"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// lockedConfig is deployKeyConfig (pool "private-review" has a deploy key)
// plus pool "locked-review" listed as egress-locked.
func lockedConfig() Config {
	cfg := deployKeyConfig()
	cfg.ClaudeCode.EgressLockedPools = []string{"locked-review"}
	return cfg
}

// TestWhichPoolsAreEgressLocked: a deploy-key pool (unlisted) and a listed pool
// are locked; an unlisted keyless pool, an agent in no pool and a chief carrying
// a locked pool's name are not.
func TestWhichPoolsAreEgressLocked(t *testing.T) {
	cfg := lockedConfig()
	chief := deployKeyAgent("locked-review", "")
	chief.Name = agents.ChiefName
	for _, c := range []struct {
		name string
		a    agents.Agent
		want bool
	}{
		{"deploy-key pool", deployKeyAgent("private-review", "acme/private-app"), true},
		{"listed pool", deployKeyAgent("locked-review", ""), true},
		{"other pool", deployKeyAgent("pr-review", ""), false},
		{"no pool", ccAgent(), false},
		{"chief", chief, false},
	} {
		if got := PoolEgressLocked(c.a, cfg); got != c.want {
			t.Errorf("%s: PoolEgressLocked = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestALockedWorkerHasNoDNSOrPublicEgressAndCarriesTheLabels: a locked worker's
// network is the claude-code network WITHOUT the public rule and WITHOUT DNS
// (the officer lock), it carries muster.agent/pool=<pool> and
// muster.agent/egress=locked, and CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1.
// Pool names that differ per case, so a label hardcoding one cannot pass.
func TestALockedWorkerHasNoDNSOrPublicEgressAndCarriesTheLabels(t *testing.T) {
	cfg := lockedConfig()
	for _, c := range []struct{ pool, repo string }{{"private-review", "acme/private-app"}, {"locked-review", ""}} {
		spec := mustBuild(t, deployKeyAgent(c.pool, c.repo), cfg, Options{})
		want := provision.Network{Isolate: true, NoClusterDNS: true}
		if !reflect.DeepEqual(spec.Network, want) {
			t.Errorf("%s: network = %+v, want %+v (no public rule, no DNS)", c.pool, spec.Network, want)
		}
		if spec.Labels["muster.agent/pool"] != c.pool || spec.Labels["muster.agent/egress"] != "locked" {
			t.Errorf("%s: labels = %v, want muster.agent/pool=%s and muster.agent/egress=locked", c.pool, spec.Labels, c.pool)
		}
		if v, ok := envValue(spec, EnvClaudeDisableNonessentialTraffic); !ok || v != "1" {
			t.Errorf("%s: %s = %q (%v), want 1", c.pool, EnvClaudeDisableNonessentialTraffic, v, ok)
		}
	}
}

// TestAnUnlockedWorkerIsUnchanged: a worker of an unlocked pool gets
// ClaudeCodeNetwork() exactly, neither label, and no nonessential-traffic
// variable — the spec it had before the lock existed. Its whole label set is
// compared with the same agent's under a configuration that locks nothing.
func TestAnUnlockedWorkerIsUnchanged(t *testing.T) {
	a := deployKeyAgent("pr-review", "")
	spec := mustBuild(t, a, lockedConfig(), Options{})
	base := mustBuild(t, a, ccConfig(), Options{})
	if !reflect.DeepEqual(spec.Network, ClaudeCodeNetwork()) {
		t.Errorf("network = %+v, want ClaudeCodeNetwork()", spec.Network)
	}
	if !reflect.DeepEqual(spec.Labels, base.Labels) {
		t.Errorf("labels = %v, want the unlocked set %v", spec.Labels, base.Labels)
	}
	if _, ok := spec.Labels["muster.agent/egress"]; ok {
		t.Error("an unlocked worker carries muster.agent/egress")
	}
	if _, ok := envValue(spec, EnvClaudeDisableNonessentialTraffic); ok {
		t.Errorf("an unlocked worker got %s", EnvClaudeDisableNonessentialTraffic)
	}
}
