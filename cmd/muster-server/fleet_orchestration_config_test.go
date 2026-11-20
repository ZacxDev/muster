package main

import (
	"strings"
	"testing"
)

// TestFleetProposalsNeedTheDigestAndSessionState: MUSTER_FLEET_ORCHESTRATION
// is off (unset or "off") or propose; propose boots only with the fleet digest
// AND session state on (a proposal is filed on a stop: there is no "on
// without session state" mode, #796 round 0 D3); any other value refuses to
// boot, naming the variable.
func TestFleetProposalsNeedTheDigestAndSessionState(t *testing.T) {
	load := func(kv map[string]string) (config, error) {
		base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("q", 40)}
		for k, v := range kv {
			base[k] = v
		}
		return loadConfig(func(k string) string { return base[k] })
	}
	for _, v := range []string{"", "off", "OFF"} {
		if cfg, err := load(map[string]string{envFleetOrchestration: v}); err != nil || cfg.FleetPropose {
			t.Errorf("%q: propose %v err %v; want off", v, cfg.FleetPropose, err)
		}
	}
	cfg, err := load(map[string]string{envFleetOrchestration: "propose", envFleetDigest: "1", envSessionState: "1"})
	if err != nil || !cfg.FleetPropose {
		t.Fatalf("propose with both: %v %v", cfg.FleetPropose, err)
	}
	for name, kv := range map[string]map[string]string{
		"no digest":        {envFleetOrchestration: "propose", envSessionState: "1"},
		"no session state": {envFleetOrchestration: "propose", envFleetDigest: "1"},
		"a bad value":      {envFleetOrchestration: "auto", envFleetDigest: "1", envSessionState: "1"},
	} {
		if _, err := load(kv); err == nil || !strings.Contains(err.Error(), envFleetOrchestration) {
			t.Errorf("%s: err %v; want a refusal naming %s", name, err, envFleetOrchestration)
		}
	}
	if _, err := load(map[string]string{envFleetOrchestration: "propose", envFleetDigest: "1"}); err == nil || !strings.Contains(err.Error(), envSessionState) {
		t.Errorf("no session state: %v; want it named", err)
	}
}
