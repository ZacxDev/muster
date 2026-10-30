package main

import (
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/api"
)

// TestThePoolEgressListIsReadAndReachesTheSpecConfig: MUSTER_AGENT_CC_POOL_EGRESS
// is read as pool -> extra hosts (pool and hosts lower-cased, `;` between
// pools, `<pool>=` alone an empty list), and the claude-code spec config
// carries exactly that map, as a copy. MUSTER_AGENT_NETPOL_FQDN is read
// lower-cased.
func TestThePoolEgressListIsReadAndReachesTheSpecConfig(t *testing.T) {
	cfg, err := loadConfig(func(k string) string {
		return map[string]string{
			envAgentCCPoolEgress: " Locked-Review = HQ.example.test , extra.example.test ; ; bare= ",
			envAgentNetpolFQDN:   " Cilium ",
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"locked-review": {"hq.example.test", "extra.example.test"}, "bare": {}}
	if !reflect.DeepEqual(cfg.AgentCCPoolEgress, want) || cfg.AgentCCPoolEgressErr != nil {
		t.Fatalf("read %#v (%v), want %#v", cfg.AgentCCPoolEgress, cfg.AgentCCPoolEgressErr, want)
	}
	if cfg.AgentNetpolFQDN != "cilium" {
		t.Errorf("AgentNetpolFQDN = %q, want cilium", cfg.AgentNetpolFQDN)
	}
	cc := ccTestConfig()
	cc.AgentCCPoolEgress = want
	spec := claudeCodeSpecConfig(cc)
	if !reflect.DeepEqual(spec.PoolEgress, want) {
		t.Fatalf("spec config PoolEgress = %#v, want %#v", spec.PoolEgress, want)
	}
	spec.PoolEgress["bare"] = []string{"x.example.test"}
	if len(cc.AgentCCPoolEgress["bare"]) != 0 {
		t.Fatal("the spec config shares the configuration's map")
	}
	none, err := loadConfig(func(string) string { return "" })
	if err != nil || none.AgentCCPoolEgress != nil || none.AgentCCPoolEgressErr != nil || none.AgentNetpolFQDN != "" {
		t.Fatalf("unset: %#v, %v, %q, %v", none.AgentCCPoolEgress, none.AgentCCPoolEgressErr, none.AgentNetpolFQDN, err)
	}
}

// TestEveryPoolEgressRefusalIsReachedOnItsOwn: each boot refusal of the two
// variables, from the VALID kubernetes claude-code config by one mutation.
func TestEveryPoolEgressRefusalIsReachedOnItsOwn(t *testing.T) {
	ok := ccK8sTestConfig()
	ok.AgentCCPoolEgress, ok.AgentCCPoolEgressErr = parsePoolEgress("review=hq.example.test")
	ok.AgentNetpolFQDN = "cilium"
	if err := ok.validate(); err != nil {
		t.Fatalf("control: a valid config is refused: %v", err)
	}
	for _, c := range []struct {
		name string
		mut  func(*config)
		want string
	}{
		{"no =", func(c *config) { c.AgentCCPoolEgress, c.AgentCCPoolEgressErr = parsePoolEgress("review") }, `invalid MUSTER_AGENT_CC_POOL_EGRESS entry "review"`},
		{"bad pool", func(c *config) { c.AgentCCPoolEgress, c.AgentCCPoolEgressErr = parsePoolEgress("-r=a.example.test") }, "a worker pool name is 1-63"},
		{"pool twice", func(c *config) { c.AgentCCPoolEgress, c.AgentCCPoolEgressErr = parsePoolEgress("r=;r=") }, `pool "r" is listed twice`},
		{"url", func(c *config) {
			c.AgentCCPoolEgress, c.AgentCCPoolEgressErr = parsePoolEgress("r=https://a.example.test")
		}, `"https://a.example.test" is not a host name`},
		{"wildcard", func(c *config) { c.AgentCCPoolEgress, c.AgentCCPoolEgressErr = parsePoolEgress("r=*.example.test") }, "is not a host name"},
		{"host twice", func(c *config) {
			c.AgentCCPoolEgress, c.AgentCCPoolEgressErr = parsePoolEgress("r=a.example.test,A.example.test")
		}, `host "a.example.test" is listed twice`},
		{"fqdn value", func(c *config) { c.AgentNetpolFQDN = "calico" }, `invalid MUSTER_AGENT_NETPOL_FQDN "calico"`},
		{"fqdn off k8s", func(c *config) {
			c.AgentProvisioner = provisionerNoop
			c.AgentNetpolFromNS, c.AgentNetpolFromLabels = "", ""
		},
			"MUSTER_AGENT_NETPOL_FQDN is set but nothing would render"},
		{"egress without the kind", func(c *config) {
			*c = provisionerTestConfig(provisionerNoop)
			c.AgentCCPoolEgress, c.AgentCCPoolEgressErr = parsePoolEgress("r=a.example.test")
		}, "MUSTER_AGENT_CC_POOL_EGRESS is set but MUSTER_AGENT_KINDS does not include claude-code"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := ok
			c.mut(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestTheDriverIsToldToFilterByNameAndWhereSecretsLive: with the kubernetes
// claude-code config, the driver copies key-holder Secrets from muster's own
// namespace (the NETPOL_FROM namespace), and writes Cilium policies exactly
// when MUSTER_AGENT_NETPOL_FQDN=cilium.
func TestTheDriverIsToldToFilterByNameAndWhereSecretsLive(t *testing.T) {
	cfg := ccK8sTestConfig()
	off := k8sDriverConfig(cfg, nil)
	if off.SecretSourceNamespace != "control-plane-7" {
		t.Errorf("SecretSourceNamespace = %q, want muster's namespace control-plane-7", off.SecretSourceNamespace)
	}
	if off.NetworkPolicy == nil || off.NetworkPolicy.CiliumFQDN {
		t.Errorf("FQDN unset: CiliumFQDN must be false, got %+v", off.NetworkPolicy)
	}
	cfg.AgentNetpolFQDN = "cilium"
	if on := k8sDriverConfig(cfg, nil); on.NetworkPolicy == nil || !on.NetworkPolicy.CiliumFQDN {
		t.Errorf("FQDN=cilium: CiliumFQDN must be true, got %+v", on.NetworkPolicy)
	}
	if gw := k8sDriverConfig(provisionerTestConfig(provisionerK8s), nil); gw.SecretSourceNamespace != "" {
		t.Errorf("a deployment without the claude-code kind copies Secrets from %q", gw.SecretSourceNamespace)
	}
}

// TestTheClusterDomainIsReadFromThisPodsSearchList: the domain is what follows
// `<ns>.svc.` in the entry for muster's own namespace — not an operator's extra
// search domain listed first, nor another namespace's entry; with no such entry
// it is a refusal, not a guess.
func TestTheClusterDomainIsReadFromThisPodsSearchList(t *testing.T) {
	conf := "nameserver 192.0.2.10\nsearch svc.corp.example ops.svc.other.example muster.svc.k8s.example svc.k8s.example k8s.example\noptions ndots:5\n"
	got, err := clusterDomainFromResolvConf(conf, "muster")
	if err != nil || got != "k8s.example" {
		t.Fatalf("domain = %q, %v; want k8s.example", got, err)
	}
	if got, err := clusterDomainFromResolvConf("search muster.svc.k8s.example.\n", "muster"); err != nil || got != "k8s.example" {
		t.Errorf("a trailing dot: %q, %v", got, err)
	}
	for _, bad := range []string{"", "search svc.k8s.example k8s.example\n", "search ops.svc.k8s.example\n", "# search muster.svc.k8s.example\n"} {
		if got, err := clusterDomainFromResolvConf(bad, "muster"); err == nil {
			t.Errorf("%q: read %q, want a refusal", bad, got)
		}
	}
}

// TestTheKindsBannerLineAnnouncesTheEgressLock: locked pools are named, with
// the hosts when the cluster filters by name, and with REFUSED when it does
// not; nothing is said when no pool is locked.
func TestTheKindsBannerLineAnnouncesTheEgressLock(t *testing.T) {
	render := func(egress map[string][]string, keys map[string]agentspec.PoolDeployKey, fqdn string) string {
		logger := log.New(&strings.Builder{}, "", 0)
		cc := ccTestConfig()
		cc.AgentCCPoolEgress, cc.AgentCCPoolDeployKeys, cc.AgentNetpolFQDN = egress, keys, fqdn
		pool, err := buildClaudePool(cc, envPlane(t, cc), memMarks{})
		if err != nil {
			t.Fatal(err)
		}
		prov, _, err := buildAgentPlaneWith(cc, stubStore{}, pool, nil, logger)
		if err != nil {
			t.Fatal(err)
		}
		return lineNaming(renderBanner(t, cc, api.Extensions{Provisioner: prov}), envAgentKinds)
	}
	keys := map[string]agentspec.PoolDeployKey{"keyed": {Repo: "acme/k", Secret: "kk"}}
	on := render(map[string][]string{"zeta": {"hq.example.test"}}, keys, "cilium")
	if !strings.Contains(on, "Egress LOCKED for worker pools [keyed, zeta] (MUSTER_AGENT_CC_POOL_EGRESS, and every deploy-key pool): "+
		"a CiliumNetworkPolicy per worker") || !strings.Contains(on, "api.anthropic.com") {
		t.Fatalf("lock on: %q", on)
	}
	refused := render(nil, keys, "")
	if !strings.Contains(refused, "Egress LOCKED for worker pools [keyed]") || !strings.Contains(refused, "REFUSED at dispatch and Start") {
		t.Fatalf("lock without FQDN: %q", refused)
	}
	if off := render(nil, nil, ""); strings.Contains(off, "Egress LOCKED") {
		t.Fatalf("no lock announces one: %q", off)
	}
}
