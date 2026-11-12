package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/officers"
)

// pullSecretTestConfig is the VALID configuration with
// MUSTER_OFFICER_IMAGE_PULL_SECRET: claude-code on the kubernetes driver with
// muster's own namespace, an officer image allowlist, and one officer whose
// pool runs its own image (with the connector door officers need).
func pullSecretTestConfig() config {
	c := ccK8sTestConfig()
	c.OfficerImagePullSecret = "officer-image-pull"
	c.OfficerImageRegistries = true
	c.ConnectorEncKey = bytes.Repeat([]byte{7}, 32)
	c.ConnectPort, c.AgentConnectURL = 18191, "http://muster.example.test:18191"
	c.Officers = []officers.Officer{{ID: "review:example-web", Image: "ghcr.io/example-org/officer-runner@sha256:" + strings.Repeat("c", 64)}}
	return c
}

func TestTheValidOfficerImagePullSecretConfigBoots(t *testing.T) {
	if err := pullSecretTestConfig().validate(); err != nil {
		t.Fatalf("control: the valid pull-Secret config is refused: %v", err)
	}
	// Only a TIERED pool's image: accepted too.
	c := pullSecretTestConfig()
	c.Officers[0].Image = ""
	c.Officers[0].Tiers = &officers.Tiers{Routine: officers.TierPool{Image: "ghcr.io/example-org/officer-routine@sha256:" + strings.Repeat("d", 64)}}
	if err := c.validate(); err != nil {
		t.Fatalf("a tiered pool's image only: refused: %v", err)
	}
	// A deploy-key pool on ANOTHER Secret: accepted.
	c = pullSecretTestConfig()
	c.AgentCCPoolDeployKeys = map[string]agentspec.PoolDeployKey{"private-review": {Repo: "example-org/private-app", Secret: "private-app-deploy-key"}}
	if err := c.validate(); err != nil {
		t.Fatalf("a deploy-key pool on another Secret: refused: %v", err)
	}
}

// TestEveryOfficerImagePullSecretRefusalIsReachedOnItsOwn: each boot refusal,
// reached from the VALID config by one mutation and asserted by its own words
// (each naming the variable).
func TestEveryOfficerImagePullSecretRefusalIsReachedOnItsOwn(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config)
		want string
	}{
		{"not a Secret name", func(c *config) { c.OfficerImagePullSecret = "Officer_Pull" },
			`invalid MUSTER_OFFICER_IMAGE_PULL_SECRET "Officer_Pull": it names a Secret`},
		// Only the provisioner moves: muster's namespace stays set, so this
		// case reaches the provisioner half of the check and not the
		// namespace half.
		{"not the kubernetes provisioner", func(c *config) { c.AgentProvisioner = provisionerNoop },
			"MUSTER_OFFICER_IMAGE_PULL_SECRET is set but nothing would copy the Secret"},
		{"no muster namespace", func(c *config) { c.AgentNetpolFromNS = "" },
			"MUSTER_OFFICER_IMAGE_PULL_SECRET is set but nothing would copy the Secret"},
		{"no officer image allowlist", func(c *config) { c.OfficerImageRegistries = false },
			"MUSTER_OFFICER_IMAGE_PULL_SECRET is set but MUSTER_OFFICER_IMAGE_REGISTRIES is not"},
		{"no officer declares an image", func(c *config) { c.Officers[0].Image = "" },
			"MUSTER_OFFICER_IMAGE_PULL_SECRET is set but no officer in MUSTER_OFFICERS declares an `image`"},
		{"the pull Secret is also a deploy-key pool's", func(c *config) {
			c.AgentCCPoolDeployKeys = map[string]agentspec.PoolDeployKey{"private-review": {Repo: "example-org/private-app", Secret: "officer-image-pull"}}
		}, "MUSTER_OFFICER_IMAGE_PULL_SECRET=officer-image-pull is also pool private-review's deploy-key Secret in MUSTER_AGENT_CC_POOL_DEPLOY_KEYS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := pullSecretTestConfig()
			c.mut(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// TestTheOfficerImagePullSecretIsReadAndReachesTheDriversSourceNamespace: the
// variable is read trimmed, the allowlist's presence is read, and the driver
// the Secret is copied by gets muster's namespace as SecretSourceNamespace.
// Unset, the config carries none.
func TestTheOfficerImagePullSecretIsReadAndReachesTheDriversSourceNamespace(t *testing.T) {
	env := map[string]string{
		envAgentProvisioner:            "kubernetes",
		envAgentKinds:                  "gateway,claude-code",
		envAgentNetpolFromNS:           "control-plane-7",
		envAgentNetpolFromLabels:       "app=Muster-Server",
		envAgentCCImage:                "ghcr.io/example-org/cc:1",
		envAgentCCAccounts:             "work",
		envAgentCCTokenPrefix + "WORK": fakeTokWork,
		envOfficerImagePullSecret:      " officer-image-pull ",
		officers.ImageRegistriesEnv:    "ghcr.io/example-org",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OfficerImagePullSecret != "officer-image-pull" || !cfg.OfficerImageRegistries {
		t.Fatalf("read %q / registries %v", cfg.OfficerImagePullSecret, cfg.OfficerImageRegistries)
	}
	if got := k8sDriverConfig(cfg, log.New(&strings.Builder{}, "", 0)).SecretSourceNamespace; got != "control-plane-7" {
		t.Fatalf("SecretSourceNamespace = %q, want muster's namespace", got)
	}
	if got := officersConfig(cfg, nil).ImagePullSecret; got != "officer-image-pull" {
		t.Fatalf("the officer runtime's ImagePullSecret = %q, want the variable's", got)
	}
	delete(env, envOfficerImagePullSecret)
	delete(env, officers.ImageRegistriesEnv)
	cfg, err = loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OfficerImagePullSecret != "" || cfg.OfficerImageRegistries {
		t.Fatalf("unset: read %q / registries %v", cfg.OfficerImagePullSecret, cfg.OfficerImageRegistries)
	}
}
