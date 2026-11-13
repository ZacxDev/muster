package main

import (
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/provision"
)

func officerEgressEnv(cidrs, dns string) func(string) string {
	return func(k string) string {
		return map[string]string{envOfficerEgressCIDRs: cidrs, envOfficerEgressDNS: dns}[k]
	}
}

// TestTheOfficerEgressSettingsAreReadAndReachTheSpecConfig: a valid list is
// parsed, trimmed, blank entries skipped and SORTED by block then port; the
// DNS value is read; both reach agentspec.Config as a copy. Unset is nothing.
func TestTheOfficerEgressSettingsAreReadAndReachTheSpecConfig(t *testing.T) {
	cfg, err := loadConfig(officerEgressEnv(" 160.79.104.0/23:8443 , ,153.61.224.0/22:443, 160.79.104.0/23:443,", "unfiltered"))
	if err != nil {
		t.Fatal(err)
	}
	want := []provision.CIDRPort{{CIDR: "153.61.224.0/22", Port: 443}, {CIDR: "160.79.104.0/23", Port: 443},
		{CIDR: "160.79.104.0/23", Port: 8443}}
	if !reflect.DeepEqual(cfg.OfficerEgressCIDRs, want) || !cfg.OfficerEgressDNS {
		t.Fatalf("read %#v dns=%v, want %#v dns=true", cfg.OfficerEgressCIDRs, cfg.OfficerEgressDNS, want)
	}
	k8s := ccK8sTestConfig()
	k8s.OfficerEgressCIDRs, k8s.OfficerEgressDNS = cfg.OfficerEgressCIDRs, true
	spec, err := agentSpecConfig(k8s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.OfficerEgressCIDRs, want) || !spec.OfficerEgressDNS {
		t.Fatalf("spec config = %#v dns=%v, want %#v dns=true", spec.OfficerEgressCIDRs, spec.OfficerEgressDNS, want)
	}
	spec.OfficerEgressCIDRs[0].CIDR = "mutated"
	if k8s.OfficerEgressCIDRs[0].CIDR != "153.61.224.0/22" {
		t.Fatal("the spec config shares the configuration's slice")
	}
	none, err := loadConfig(func(string) string { return "" })
	if err != nil || none.OfficerEgressCIDRs != nil || none.OfficerEgressDNS {
		t.Fatalf("unset: %#v dns=%v, %v", none.OfficerEgressCIDRs, none.OfficerEgressDNS, err)
	}
	if s, err := agentSpecConfig(ccK8sTestConfig()); err != nil || s.OfficerEgressCIDRs != nil || s.OfficerEgressDNS {
		t.Fatalf("unset reaches the spec config as %#v dns=%v (%v)", s.OfficerEgressCIDRs, s.OfficerEgressDNS, err)
	}
}

// TestEveryOfficerEgressRefusalNamesTheVariable: each malformed value refuses
// to load, naming its variable and the reason in its own words.
func TestEveryOfficerEgressRefusalNamesTheVariable(t *testing.T) {
	for name, c := range map[string]struct{ cidrs, dns, want string }{
		"no port":       {"160.79.104.0/23", "", `invalid MUSTER_OFFICER_EGRESS_CIDRS entry "160.79.104.0/23": want <ipv4-cidr>:<port>`},
		"port not num":  {"160.79.104.0/23:https", "", `the port "https" is not a number`},
		"port empty":    {"160.79.104.0/23:", "", `the port "" is not a number`},
		"port zero":     {"160.79.104.0/23:0", "", "MUSTER_OFFICER_EGRESS_CIDRS entry \"160.79.104.0/23:0\": port 0 out of range"},
		"port too big":  {"160.79.104.0/23:65536", "", "port 65536 out of range"},
		"bad cidr":      {"160.79.104/23:443", "", `"160.79.104/23" is not a CIDR block`},
		"bare address":  {"160.79.104.10:443", "", `"160.79.104.10" is not a CIDR block`},
		"ipv6":          {"2607:6bc0::/48:443", "", `MUSTER_OFFICER_EGRESS_CIDRS entry "2607:6bc0::/48:443": "2607:6bc0::/48" is not an IPv4 block`},
		"host bits":     {"160.79.104.10/23:443", "", "has host bits set"},
		"everything":    {"0.0.0.0/0:443", "", `"0.0.0.0/0" is wider than a /16`},
		"too wide":      {"160.0.0.0/8:443", "", `"160.0.0.0/8" is wider than a /16`},
		"private":       {"10.96.0.0/16:443", "", "overlaps the non-public range 10.0.0.0/8"},
		"lan":           {"192.168.0.0/16:443", "", "overlaps the non-public range 192.168.0.0/16"},
		"metadata":      {"169.254.169.254/32:80", "", "overlaps the non-public range 169.254.0.0/16"},
		"cgnat":         {"100.64.0.0/16:443", "", "overlaps the non-public range 100.64.0.0/10"},
		"loopback":      {"127.0.0.1/32:443", "", "overlaps the non-public range 127.0.0.0/8"},
		"duplicate":     {"160.79.104.0/23:443,160.79.104.0/23:443", "", "invalid MUSTER_OFFICER_EGRESS_CIDRS: 160.79.104.0/23:443 is listed twice"},
		"dns other":     {"", "on", `invalid MUSTER_OFFICER_EGRESS_DNS "on": want unfiltered`},
		"dns uppercase": {"", "Unfiltered", `invalid MUSTER_OFFICER_EGRESS_DNS "Unfiltered"`},
	} {
		_, err := loadConfig(officerEgressEnv(c.cidrs, c.dns))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
		}
	}
}

// TestOfficerEgressIsRefusedWhereNoAgentPolicyIsWritten: from the VALID
// kubernetes claude-code config, each setting boots; the same setting on a
// deployment that writes no agent NetworkPolicy (noop driver, or no
// claude-code kind) is refused naming both variables.
func TestOfficerEgressIsRefusedWhereNoAgentPolicyIsWritten(t *testing.T) {
	blocks := []provision.CIDRPort{{CIDR: "160.79.104.0/23", Port: 443}}
	for name, set := range map[string]func(*config){
		"cidrs": func(c *config) { c.OfficerEgressCIDRs = blocks },
		"dns":   func(c *config) { c.OfficerEgressDNS = true },
	} {
		ok := ccK8sTestConfig()
		set(&ok)
		if err := ok.validate(); err != nil {
			t.Fatalf("%s: control: a valid config is refused: %v", name, err)
		}
		for arm, base := range map[string]config{
			"noop driver":    ccTestConfig(),
			"no claude-code": provisionerTestConfig(provisionerK8s),
		} {
			bad := base
			set(&bad)
			err := bad.validate()
			if err == nil || !strings.Contains(err.Error(), "MUSTER_OFFICER_EGRESS_CIDRS or MUSTER_OFFICER_EGRESS_DNS is set but "+
				"this deployment writes no agent NetworkPolicy") {
				t.Errorf("%s on %s: err = %v", name, arm, err)
			}
		}
	}
}

// TestTheOfficerEgressBannerLineSaysBothStates: the line names the blocks and
// DNS when set and says "unset" for each when not, and it is printed under the
// kubernetes arm of the network line.
func TestTheOfficerEgressBannerLineSaysBothStates(t *testing.T) {
	on := ccK8sTestConfig()
	on.OfficerEgressCIDRs = []provision.CIDRPort{{CIDR: "153.61.224.0/22", Port: 443}, {CIDR: "160.79.104.0/23", Port: 8443}}
	on.OfficerEgressDNS = true
	onLine := officerEgressBannerLine(on)
	for _, want := range []string{"TCP to [153.61.224.0/22:443, 160.79.104.0/23:8443] (MUSTER_OFFICER_EGRESS_CIDRS)",
		"cluster DNS UNFILTERED, any name (MUSTER_OFFICER_EGRESS_DNS=unfiltered)", "officers only"} {
		if !strings.Contains(onLine, want) {
			t.Errorf("on: line does not say %q:\n  %s", want, onLine)
		}
	}
	offLine := officerEgressBannerLine(ccK8sTestConfig())
	for _, want := range []string{"no address blocks (MUSTER_OFFICER_EGRESS_CIDRS unset)",
		"no cluster DNS (MUSTER_OFFICER_EGRESS_DNS unset)"} {
		if !strings.Contains(offLine, want) {
			t.Errorf("off: line does not say %q:\n  %s", want, offLine)
		}
	}
	// It is in the banner: rendered as TestTheNetworkBannerLineSaysWhetherAgentsAreConfined does.
	build := on
	build.AgentProvisioner = provisionerNoop
	build.AgentNetpolFromNS, build.AgentNetpolFromLabels = "", ""
	build.OfficerEgressCIDRs, build.OfficerEgressDNS = nil, false
	pool, err := buildClaudePool(build, envPlane(t, build), memMarks{})
	if err != nil {
		t.Fatal(err)
	}
	prov, _, err := buildAgentPlaneWith(build, stubStore{}, pool, nil, log.New(&strings.Builder{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if got := lineNaming(renderBanner(t, on, api.Extensions{Provisioner: prov}), "officer egress:"); got != onLine {
		t.Errorf("the banner's officer egress line = %q, want %q", got, onLine)
	}
}
