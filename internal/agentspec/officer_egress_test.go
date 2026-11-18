package agentspec

import (
	"reflect"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// The officer egress settings (Config.OfficerEgressCIDRs, OfficerEgressDNS):
// an officer instance gets exactly the named blocks, DNS only when asked, and
// the connect port — nothing else; every other agent is unchanged.

var officerSession = Options{ClaudeCodeSession: ClaudeCodeSession{BypassPermissions: true}}

// officerEgressBlocks differ in block AND port, so a spec that hardcoded 443 or
// one block cannot pass.
var officerEgressBlocks = []provision.CIDRPort{{CIDR: "160.79.104.0/23", Port: 443}, {CIDR: "198.51.100.0/24", Port: 8443}}

func officerEgressConfig(dns bool) Config {
	cfg := lockedConfig()
	cfg.ConnectURL, cfg.ConnectPort = "http://muster.example.test:18191", 18191
	cfg.OfficerEgressCIDRs = append([]provision.CIDRPort(nil), officerEgressBlocks...)
	cfg.OfficerEgressDNS = dns
	return cfg
}

// TestAnOfficerGetsExactlyTheAdmittedBlocksAndDNSPerTheFlag: the officer's
// network is the lock (no public rule) plus the blocks and the connect port;
// DNS only with the flag. The blocks are a COPY: mutating the spec's slice
// leaves the configuration alone.
func TestAnOfficerGetsExactlyTheAdmittedBlocksAndDNSPerTheFlag(t *testing.T) {
	for _, dns := range []bool{false, true} {
		cfg := officerEgressConfig(dns)
		spec := mustBuild(t, ccAgent(), cfg, officerSession)
		want := provision.Network{Isolate: true, ControllerEgressTCPPorts: []int{18191}, NoClusterDNS: !dns,
			EgressCIDRs: officerEgressBlocks}
		if !reflect.DeepEqual(spec.Network, want) {
			t.Errorf("dns=%v: officer network = %+v, want %+v", dns, spec.Network, want)
		}
		spec.Network.EgressCIDRs[0].CIDR = "mutated"
		if cfg.OfficerEgressCIDRs[0].CIDR != "160.79.104.0/23" {
			t.Fatal("the spec shares the configuration's block slice")
		}
	}
	// DNS alone, no blocks: the lock plus DNS and the connect port.
	cfg := officerEgressConfig(true)
	cfg.OfficerEgressCIDRs = nil
	if got, want := mustBuild(t, ccAgent(), cfg, officerSession).Network,
		(provision.Network{Isolate: true, ControllerEgressTCPPorts: []int{18191}}); !reflect.DeepEqual(got, want) {
		t.Errorf("DNS only: officer network = %+v, want %+v", got, want)
	}
}

// TestTheOfficerEgressSettingsReachNoOtherAgent: with both settings on, a task
// agent, the chief, an unlocked pool worker, a locked pool worker — and a
// locked pool worker that is ALSO an officer session — each get exactly the
// network they get with both settings off.
func TestTheOfficerEgressSettingsReachNoOtherAgent(t *testing.T) {
	chief := ccAgent()
	chief.Name = agents.ChiefName
	off := officerEgressConfig(false)
	off.OfficerEgressCIDRs = nil
	on := officerEgressConfig(true)
	for _, c := range []struct {
		name string
		a    agents.Agent
		opts Options
	}{
		{"task agent", ccAgent(), Options{}},
		{"chief", chief, Options{}},
		{"unlocked worker", deployKeyAgent("pr-review", ""), Options{}},
		{"locked worker", deployKeyAgent("locked-review", ""), Options{}},
		{"deploy-key worker", deployKeyAgent("private-review", "acme/private-app"), Options{}},
		{"locked worker, officer session", deployKeyAgent("locked-review", ""), officerSession},
	} {
		got := mustBuild(t, c.a, on, c.opts).Network
		base := mustBuild(t, c.a, off, c.opts).Network
		if !reflect.DeepEqual(got, base) {
			t.Errorf("%s: network with officer egress on = %+v, want the same as off %+v", c.name, got, base)
		}
		if len(got.EgressCIDRs) > 0 {
			t.Errorf("%s: got officer egress blocks %v", c.name, got.EgressCIDRs)
		}
	}
	// Positive control: the same "on" configuration DOES reach an officer, so
	// the equalities above are not a configuration that reaches nobody.
	if got := mustBuild(t, ccAgent(), on, officerSession).Network; len(got.EgressCIDRs) != 2 || got.NoClusterDNS {
		t.Fatalf("control: the on configuration does not reach an officer: %+v", got)
	}
}

// TestEmptyOfficerEgressLeavesTheOfficerLockAsToday: with neither setting the
// officer network is the lock exactly as before the settings existed.
func TestEmptyOfficerEgressLeavesTheOfficerLockAsToday(t *testing.T) {
	cfg := officerEgressConfig(false)
	cfg.OfficerEgressCIDRs = nil
	want := provision.Network{Isolate: true, ControllerEgressTCPPorts: []int{18191}, NoClusterDNS: true}
	if got := mustBuild(t, ccAgent(), cfg, officerSession).Network; !reflect.DeepEqual(got, want) {
		t.Errorf("officer network = %+v, want %+v", got, want)
	}
}
