package main

import (
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/api"
)

// TestThePoolDeployKeysAreReadAndReachTheSpecConfig: MUSTER_AGENT_CC_POOL_DEPLOY_KEYS
// is read as pool -> {repository, Secret}, the pool lower-cased, and the
// claude-code spec config carries exactly that map, as a copy.
func TestThePoolDeployKeysAreReadAndReachTheSpecConfig(t *testing.T) {
	cfg, err := loadConfig(func(k string) string {
		return map[string]string{envAgentCCPoolDeployKeys: " Private-Review = acme/private-app : private-app-deploy-key , ,other=acme/second:k2"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]agentspec.PoolDeployKey{
		"private-review": {Repo: "acme/private-app", Secret: "private-app-deploy-key"},
		"other":          {Repo: "acme/second", Secret: "k2"},
	}
	if !reflect.DeepEqual(cfg.AgentCCPoolDeployKeys, want) || cfg.AgentCCPoolDeployKeysErr != nil {
		t.Fatalf("read %+v (%v), want %+v", cfg.AgentCCPoolDeployKeys, cfg.AgentCCPoolDeployKeysErr, want)
	}
	cc := ccTestConfig()
	cc.AgentCCPoolDeployKeys = want
	spec := claudeCodeSpecConfig(cc)
	if !reflect.DeepEqual(spec.PoolDeployKeys, want) {
		t.Fatalf("spec config PoolDeployKeys = %+v, want %+v", spec.PoolDeployKeys, want)
	}
	spec.PoolDeployKeys["other"] = agentspec.PoolDeployKey{Repo: "x/y", Secret: "z"}
	if cc.AgentCCPoolDeployKeys["other"].Repo != "acme/second" {
		t.Fatal("the spec config shares the configuration's map: a mutation downstream reaches it")
	}
	// Unset is none: no map at all, so every pool is anonymous.
	none, err := loadConfig(func(string) string { return "" })
	if err != nil || none.AgentCCPoolDeployKeys != nil || none.AgentCCPoolDeployKeysErr != nil {
		t.Fatalf("unset: %+v, %v, %v", none.AgentCCPoolDeployKeys, none.AgentCCPoolDeployKeysErr, err)
	}
}

// 🔴 EACH BOOT REFUSAL OF THE DEPLOY-KEY LIST, reached from the VALID
// claude-code config by one mutation and asserted by its own words.
func TestEveryPoolDeployKeyRefusalIsReachedOnItsOwn(t *testing.T) {
	ok := ccTestConfig()
	ok.AgentCCPoolDeployKeys, ok.AgentCCPoolDeployKeysErr = parsePoolDeployKeys("review=acme/private-app:deploy-key")
	if err := ok.validate(); err != nil {
		t.Fatalf("control: a valid deploy-key list is refused: %v", err)
	}
	for _, c := range []struct{ name, value, want string }{
		{"no secret", "review=acme/private-app", `invalid MUSTER_AGENT_CC_POOL_DEPLOY_KEYS entry "review=acme/private-app"`},
		{"no repository", "review", `invalid MUSTER_AGENT_CC_POOL_DEPLOY_KEYS entry "review"`},
		{"bad pool", "-review=acme/private-app:k", "a worker pool name is 1-63"},
		{"bad repository", "review=acme:k", "the repository must be owner/name"},
		{"bad secret", "review=acme/private-app:Deploy_Key", "the Secret's name must be a DNS subdomain"},
		{"pool twice", "review=acme/a:k,review=acme/b:k", `pool "review" is listed twice`},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := ccTestConfig()
			cfg.AgentCCPoolDeployKeys, cfg.AgentCCPoolDeployKeysErr = parsePoolDeployKeys(c.value)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
	t.Run("without the kind", func(t *testing.T) {
		cfg := ccTestConfig()
		cfg.AgentKinds, cfg.AgentCCImage, cfg.AgentCCAccountNames = nil, "", nil
		cfg.AgentCCPoolDeployKeys, cfg.AgentCCPoolDeployKeysErr = parsePoolDeployKeys("review=acme/private-app:k")
		want := "MUSTER_AGENT_CC_POOL_DEPLOY_KEYS is set but MUSTER_AGENT_KINDS does not include claude-code"
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want one containing %q", err, want)
		}
	})
}

// The claude-code arm of the `agent kinds:` line says which state the deploy
// keys are in, both ways: the pools, repositories and Secrets, or none.
func TestTheKindsBannerLineAnnouncesThePoolDeployKeysBothWays(t *testing.T) {
	render := func(keys map[string]agentspec.PoolDeployKey) string {
		logger := log.New(&strings.Builder{}, "", 0)
		cc := ccTestConfig()
		cc.AgentCCPoolDeployKeys = keys
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
	on := render(map[string]agentspec.PoolDeployKey{
		"zeta": {Repo: "acme/z", Secret: "kz"}, "alpha": {Repo: "acme/a", Secret: "ka"}})
	off := render(nil)
	if !strings.Contains(on, "Read-only deploy keys CONFIGURED for worker pools [alpha=acme/a (Secret ka), zeta=acme/z (Secret kz)] "+
		"(MUSTER_AGENT_CC_POOL_DEPLOY_KEYS)") {
		t.Fatalf("deploy keys on: %q", on)
	}
	if !strings.Contains(off, "No worker pool has a deploy key (MUSTER_AGENT_CC_POOL_DEPLOY_KEYS unset): every pool clones "+
		"and fetches anonymously, so pools serve public repositories only.") {
		t.Fatalf("deploy keys off: %q", off)
	}
	if strings.Contains(off, "deploy keys CONFIGURED") {
		t.Fatalf("deploy keys off announces one: %q", off)
	}
}
