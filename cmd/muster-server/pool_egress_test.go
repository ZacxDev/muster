package main

import (
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/api"
)

// TestTheEgressLockedPoolsAreReadAndReachTheSpecConfig:
// MUSTER_AGENT_CC_EGRESS_LOCKED_POOLS is read as a lower-cased pool list, and
// the claude-code spec config carries exactly that list, as a copy.
func TestTheEgressLockedPoolsAreReadAndReachTheSpecConfig(t *testing.T) {
	cfg, err := loadConfig(func(k string) string {
		return map[string]string{envAgentCCEgressLockedPools: " Locked-Review , ,other "}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"locked-review", "other"}
	if !reflect.DeepEqual(cfg.AgentCCEgressLockedPools, want) {
		t.Fatalf("read %#v, want %#v", cfg.AgentCCEgressLockedPools, want)
	}
	cc := ccTestConfig()
	cc.AgentCCEgressLockedPools = want
	spec := claudeCodeSpecConfig(cc)
	if !reflect.DeepEqual(spec.EgressLockedPools, want) {
		t.Fatalf("spec config EgressLockedPools = %#v, want %#v", spec.EgressLockedPools, want)
	}
	spec.EgressLockedPools[0] = "mutated"
	if cc.AgentCCEgressLockedPools[0] != "locked-review" {
		t.Fatal("the spec config shares the configuration's slice")
	}
	none, err := loadConfig(func(string) string { return "" })
	if err != nil || len(none.AgentCCEgressLockedPools) != 0 {
		t.Fatalf("unset: %#v, %v", none.AgentCCEgressLockedPools, err)
	}
}

// TestEveryEgressLockedPoolsRefusalIsReachedOnItsOwn: from the VALID config by
// one mutation, each boot refusal by its own words.
func TestEveryEgressLockedPoolsRefusalIsReachedOnItsOwn(t *testing.T) {
	ok := ccK8sTestConfig()
	ok.AgentCCEgressLockedPools = []string{"review"}
	if err := ok.validate(); err != nil {
		t.Fatalf("control: a valid config is refused: %v", err)
	}
	bad := ok
	bad.AgentCCEgressLockedPools = []string{"-review"}
	if err := bad.validate(); err == nil || !strings.Contains(err.Error(), `invalid MUSTER_AGENT_CC_EGRESS_LOCKED_POOLS entry "-review"`) {
		t.Fatalf("bad pool: %v", err)
	}
	off := provisionerTestConfig(provisionerNoop)
	off.AgentCCEgressLockedPools = []string{"review"}
	if err := off.validate(); err == nil ||
		!strings.Contains(err.Error(), "MUSTER_AGENT_CC_EGRESS_LOCKED_POOLS is set but MUSTER_AGENT_KINDS does not include claude-code") {
		t.Fatalf("without the kind: %v", err)
	}
}

// TestTheDriverCopiesKeyHolderSecretsFromMustersNamespace: with the kubernetes
// claude-code config the driver copies key-holder Secrets from the
// NETPOL_FROM namespace (muster's own); without the kind, from nowhere.
func TestTheDriverCopiesKeyHolderSecretsFromMustersNamespace(t *testing.T) {
	if got := k8sDriverConfig(ccK8sTestConfig(), nil).SecretSourceNamespace; got != "control-plane-7" {
		t.Errorf("SecretSourceNamespace = %q, want muster's namespace control-plane-7", got)
	}
	if got := k8sDriverConfig(provisionerTestConfig(provisionerK8s), nil).SecretSourceNamespace; got != "" {
		t.Errorf("a deployment without the claude-code kind copies Secrets from %q", got)
	}
}

// TestTheKindsBannerLineAnnouncesTheEgressLock: the locked pools (listed and
// deploy-key, sorted, once each) and the label the cluster policy selects are
// named; nothing is said when no pool is locked.
func TestTheKindsBannerLineAnnouncesTheEgressLock(t *testing.T) {
	render := func(listed []string, keys map[string]agentspec.PoolDeployKey) string {
		logger := log.New(&strings.Builder{}, "", 0)
		cc := ccTestConfig()
		cc.AgentCCEgressLockedPools, cc.AgentCCPoolDeployKeys = listed, keys
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
	on := render([]string{"zeta", "keyed"}, keys)
	if !strings.Contains(on, "Egress LOCKED for worker pools [keyed, zeta] (MUSTER_AGENT_CC_EGRESS_LOCKED_POOLS, and every "+
		"deploy-key pool)") || !strings.Contains(on, "muster.agent/egress=locked and muster.agent/pool=<pool>") {
		t.Fatalf("lock on: %q", on)
	}
	if only := render(nil, keys); !strings.Contains(only, "Egress LOCKED for worker pools [keyed]") {
		t.Fatalf("a deploy-key pool alone is not announced locked: %q", only)
	}
	if off := render(nil, nil); strings.Contains(off, "Egress LOCKED") {
		t.Fatalf("no lock announces one: %q", off)
	}
}
