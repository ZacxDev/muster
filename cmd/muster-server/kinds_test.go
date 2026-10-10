package main

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// Fake setup-tokens, obviously not real.
const (
	fakeTokWork     = "sk-ant-oat01-FAKE-boot-work-3e7a"
	fakeTokPersonal = "sk-ant-oat01-FAKE-boot-personal-58cd"
)

// ccTestConfig is a VALID claude-code deployment: every refusal case below
// mutates exactly one field of it, so no earlier check can reject the case
// first.
func ccTestConfig() config {
	c := provisionerTestConfig(provisionerNoop)
	c.AgentKinds = []string{agents.KindGateway, agents.KindClaudeCode}
	c.AgentCCImage = "ghcr.io/example-org/claude-code-agent:0a1b2c3"
	c.AgentCCAccountNames = []string{"work", "personal-2"}
	c.AgentCCTokens = map[string]string{"work": fakeTokWork, "personal-2": fakeTokPersonal}
	return c
}

func TestTheValidClaudeCodeConfigBoots(t *testing.T) {
	if err := ccTestConfig().validate(); err != nil {
		t.Fatalf("control: the valid claude-code config is refused: %v", err)
	}
	if err := provisionerTestConfig(provisionerNoop).validate(); err != nil {
		t.Fatalf("control: the gateway-only config is refused: %v", err)
	}
}

// TestEveryKindsRefusalIsReachedOnItsOwn: each fail-closed boot refusal, reached
// from the VALID config by one mutation, asserted by its own words, and never
// carrying a token.
func TestEveryKindsRefusalIsReachedOnItsOwn(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config)
		want string
	}{
		{"unknown kind", func(c *config) { c.AgentKinds = []string{"gateway", "teleport"} }, `entry "teleport"`},
		{"kind twice", func(c *config) { c.AgentKinds = []string{"gateway", "claude-code", "gateway"} }, "listed twice"},
		{"gateway missing", func(c *config) { c.AgentKinds = []string{"claude-code"} }, "must include gateway"},
		{"cc image without the kind", func(c *config) { c.AgentKinds = nil; c.AgentCCAccountNames = nil }, "armed switch"},
		{"cc accounts without the kind", func(c *config) { c.AgentKinds = nil; c.AgentCCImage = "" }, "armed switch"},
		{"cc storage without the kind", func(c *config) {
			c.AgentKinds, c.AgentCCImage, c.AgentCCAccountNames, c.AgentCCStorage = nil, "", nil, "2Gi"
		}, "armed switch"},
		{"no provisioner", func(c *config) { c.AgentProvisioner = provisionerNone }, "nothing to build it with"},
		{"no image", func(c *config) { c.AgentCCImage = "" }, "MUSTER_AGENT_CC_IMAGE is not set"},
		{"unpinned image", func(c *config) { c.AgentCCImage = "registry.example.test:5000/team/claude-code-agent" }, "must pin a tag or a digest"},
		{"bad storage size", func(c *config) { c.AgentCCStorage = "lots" }, "invalid MUSTER_AGENT_CC_STORAGE_SIZE"},
		{"empty pool", func(c *config) { c.AgentCCAccountNames = nil; c.AgentCCTokens = nil }, "pool is EMPTY"},
		{"bad account name", func(c *config) { c.AgentCCAccountNames = []string{"work", "-bad"} }, `entry "-bad"`},
		{"account twice", func(c *config) { c.AgentCCAccountNames = []string{"work", "work"} }, "listed twice"},
		{"account with no token", func(c *config) { c.AgentCCTokens["personal-2"] = "" }, "MUSTER_AGENT_CC_TOKEN_PERSONAL_2 is not set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := ccTestConfig()
			c.mut(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			for _, tok := range []string{fakeTokWork, fakeTokPersonal} {
				if strings.Contains(err.Error(), tok) {
					t.Fatalf("the refusal carries a token: %v", err)
				}
			}
		})
	}
}

// TestTheAccountListAndTokensAreReadFromTheEnvironment: names from
// MUSTER_AGENT_CC_ACCOUNTS, each token from MUSTER_AGENT_CC_TOKEN_<NAME> with '-'
// as '_'; a token variable no list names is NOT an account.
func TestTheAccountListAndTokensAreReadFromTheEnvironment(t *testing.T) {
	env := map[string]string{
		envAgentKinds:                        " Gateway , claude-code ",
		envAgentCCImage:                      "ghcr.io/example-org/cc:1",
		envAgentCCAccounts:                   "work, personal-2",
		envAgentCCTokenPrefix + "WORK":       fakeTokWork,
		envAgentCCTokenPrefix + "PERSONAL_2": fakeTokPersonal,
		envAgentCCTokenPrefix + "STRAY":      "sk-ant-oat01-FAKE-stray",
		envAgentCCStorage:                    "2Gi",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.AgentKinds, ",") != "gateway,claude-code" || !cfg.claudeCodeEnabled() {
		t.Fatalf("kinds = %v", cfg.AgentKinds)
	}
	if strings.Join(cfg.AgentCCAccountNames, ",") != "work,personal-2" || len(cfg.AgentCCTokens) != 2 ||
		cfg.AgentCCTokens["work"] != fakeTokWork || cfg.AgentCCTokens["personal-2"] != fakeTokPersonal {
		t.Fatalf("accounts %v, %d token(s)", cfg.AgentCCAccountNames, len(cfg.AgentCCTokens))
	}
	if cfg.AgentCCStorage != "2Gi" || cfg.AgentCCImage != "ghcr.io/example-org/cc:1" {
		t.Fatal("storage/image not read")
	}
	none, _ := loadConfig(func(string) string { return "" })
	if none.claudeCodeEnabled() || strings.Join(none.agentKinds(), ",") != "gateway" {
		t.Fatalf("unset kinds resolve to %v", none.agentKinds())
	}
}

// TestEnablingClaudeCodeRaisesTheDriversPersistenceAndNothingElse: the
// claude-code PVC needs the driver's persistence capability, so enabling the
// kind sets the storage-class pointer even when the gateway kind's workspaces
// stay ephemeral — and the gateway kind's spec still asks for no persistence.
func TestEnablingClaudeCodeRaisesTheDriversPersistenceAndNothingElse(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	off := k8sDriverConfig(provisionerTestConfig(provisionerK8s), logger)
	if off.WorkspaceStorageClass != nil {
		t.Fatal("control: a gateway-only deployment without persistence got a storage class")
	}
	cc := ccTestConfig()
	cc.AgentProvisioner = provisionerK8s
	cc.AgentStorageClass = "local-path"
	on := k8sDriverConfig(cc, logger)
	if on.WorkspaceStorageClass == nil || *on.WorkspaceStorageClass != "local-path" {
		t.Fatalf("claude-code enabled: storage class = %v", on.WorkspaceStorageClass)
	}
	spec, err := agentSpecConfig(cc)
	if err != nil {
		t.Fatal(err)
	}
	if spec.WorkspacePersist {
		t.Fatal("enabling claude-code turned on persistence for gateway-kind agents")
	}
	if spec.ClaudeCode == nil || spec.ClaudeCode.Image != cc.AgentCCImage || spec.ClaudeCode.Accounts["work"] != fakeTokWork {
		t.Fatalf("ClaudeCode spec config = %+v", spec.ClaudeCode)
	}
	if gw, _ := agentSpecConfig(provisionerTestConfig(provisionerK8s)); gw.ClaudeCode != nil {
		t.Fatal("a gateway-only deployment has a ClaudeCode profile")
	}
}

type memMarks struct{}

func (memMarks) Marks(context.Context) (map[string]ccpool.Mark, error) { return nil, nil }
func (memMarks) LiveCounts(context.Context) (map[string]int, error)    { return nil, nil }
func (memMarks) MarkRateLimited(context.Context, string, string, time.Time) error {
	return nil
}
func (memMarks) MarkAuthFailed(context.Context, string, string, string, time.Time) error {
	return nil
}

func TestAClaudeCodeDeploymentWithNoDatabaseIsRefused(t *testing.T) {
	if _, err := buildClaudePool(ccTestConfig(), nil); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v", err)
	}
	if p, err := buildClaudePool(provisionerTestConfig(provisionerNoop), nil); p != nil || err != nil {
		t.Fatalf("gateway-only: pool %v err %v, want neither", p, err)
	}
	if _, _, _, err := buildAgentPlaneWith(ccTestConfig(), stubStore{}, nil, log.New(&strings.Builder{}, "", 0)); err == nil {
		t.Fatal("a claude-code agent plane was built with no account pool")
	}
}

// TestTheKindsBannerLineNamesAccountsNeverTokens renders both arms of the
// `agent kinds:` line and pins that the claude-code arm names the accounts and
// the variables — and contains no token.
func TestTheKindsBannerLineNamesAccountsNeverTokens(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	cc := ccTestConfig()
	pool, err := buildClaudePool(cc, memMarks{})
	if err != nil {
		t.Fatal(err)
	}
	prov, _, _, err := buildAgentPlaneWith(cc, stubStore{}, pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	onOut := renderBanner(t, cc, api.Extensions{Provisioner: prov})
	off := provisionerTestConfig(provisionerNoop)
	offOut := renderBanner(t, off, api.Extensions{Provisioner: bannerProvisioner(t, off)})

	on := lineNaming(onOut, envAgentKinds)
	offLine := lineNaming(offOut, envAgentKinds)
	if !strings.Contains(on, "gateway, claude-code") || !strings.Contains(on, "[work, personal-2]") ||
		!strings.Contains(on, envAgentCCTokenPrefix) || !strings.Contains(on, cc.AgentCCImage) {
		t.Fatalf("claude-code arm: %q", on)
	}
	if !strings.Contains(offLine, "gateway only") {
		t.Fatalf("gateway-only arm: %q", offLine)
	}
	for _, out := range []string{onOut, offOut} {
		for _, tok := range []string{fakeTokWork, fakeTokPersonal} {
			if strings.Contains(out, tok) {
				t.Fatalf("the banner prints a token:\n%s", out)
			}
		}
	}
}

// TestThePickerDefaultIsGatewayWhateverTheEnvOrder: the picker checks its first
// option, and the default must be the gateway kind even when
// MUSTER_AGENT_KINDS lists claude-code first.
func TestThePickerDefaultIsGatewayWhateverTheEnvOrder(t *testing.T) {
	got := kindSet{kinds: []string{agents.KindClaudeCode, agents.KindGateway}}.Enabled()
	if strings.Join(got, ",") != "gateway,claude-code" {
		t.Fatalf("Enabled = %v, want gateway first", got)
	}
}
