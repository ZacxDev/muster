package agentspec

import (
	"reflect"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// poolEgressConfig is deployKeyConfig plus pool "locked-review" listed with one
// extra host, and pool "bare-lock" listed with none.
func poolEgressConfig() Config {
	cfg := deployKeyConfig()
	cfg.ClaudeCode.PoolEgress = map[string][]string{
		"locked-review": {"hq.example.test", "github.com"},
		"bare-lock":     {},
	}
	return cfg
}

// wantDefaultHosts is DefaultPoolEgressHosts written out by hand from the
// requirement, so a change to the variable fails here.
var wantDefaultHosts = []string{"api.anthropic.com", "api.github.com", "codeload.github.com", "github.com",
	"objects.githubusercontent.com", "ssh.github.com"}

// TestWhichPoolsAreEgressLocked: a deploy-key pool (unlisted) gets the defaults;
// a listed pool the defaults plus its extras, sorted, a repeat of a default
// dropped; a pool listed with nothing the defaults; an unlisted keyless pool,
// an agent in no pool and a chief none.
func TestWhichPoolsAreEgressLocked(t *testing.T) {
	cfg := poolEgressConfig()
	for _, c := range []struct {
		name string
		a    agents.Agent
		want []string
		ok   bool
	}{
		{"deploy-key pool", deployKeyAgent("private-review", "acme/private-app"), wantDefaultHosts, true},
		{"listed pool", deployKeyAgent("locked-review", ""), []string{"api.anthropic.com", "api.github.com",
			"codeload.github.com", "github.com", "hq.example.test", "objects.githubusercontent.com", "ssh.github.com"}, true},
		{"listed with no host", deployKeyAgent("bare-lock", ""), wantDefaultHosts, true},
		{"other pool", deployKeyAgent("pr-review", ""), nil, false},
		{"no pool", ccAgent(), nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := PoolEgressHostsFor(c.a, cfg)
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("PoolEgressHostsFor = %v, %v; want %v, %v", got, ok, c.want, c.ok)
			}
		})
	}
	chief := deployKeyAgent("locked-review", "")
	chief.Name = agents.ChiefName
	if _, ok := PoolEgressHostsFor(chief, cfg); ok {
		t.Error("a chief carrying a locked pool's name was locked")
	}
}

// TestALockedWorkersSpecNamesItsHostsAndSilencesTheCLI: the built spec's network
// is the claude-code network with exactly those hosts, and the pod gets
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1. An unlocked worker's network is
// ClaudeCodeNetwork() exactly and it gets no such variable (the control).
func TestALockedWorkersSpecNamesItsHostsAndSilencesTheCLI(t *testing.T) {
	cfg := poolEgressConfig()
	locked := mustBuild(t, deployKeyAgent("private-review", "acme/private-app"), cfg, Options{})
	want := provision.Network{Isolate: true, PublicEgressTCPPorts: []int{443}, PublicEgressHosts: wantDefaultHosts}
	if !reflect.DeepEqual(locked.Network, want) {
		t.Errorf("locked network = %+v, want %+v", locked.Network, want)
	}
	if v, ok := envValue(locked, EnvClaudeDisableNonessentialTraffic); !ok || v != "1" {
		t.Errorf("%s = %q (%v) on a locked worker, want 1", EnvClaudeDisableNonessentialTraffic, v, ok)
	}
	open := mustBuild(t, deployKeyAgent("pr-review", ""), cfg, Options{})
	if !reflect.DeepEqual(open.Network, ClaudeCodeNetwork()) {
		t.Errorf("an unlocked worker's network = %+v, want ClaudeCodeNetwork() unchanged", open.Network)
	}
	if _, ok := envValue(open, EnvClaudeDisableNonessentialTraffic); ok {
		t.Errorf("an unlocked worker got %s", EnvClaudeDisableNonessentialTraffic)
	}
}
