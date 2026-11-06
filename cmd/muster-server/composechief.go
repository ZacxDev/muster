package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/compose"
)

// ---------------------------------------------------------------------------
// THE CHIEF ON THE DOCKER COMPOSE INSTALL (MUSTER_CHIEF_RUNTIME=compose)
//
// The Compose file runs a `chief` container (the claude-code-agent image,
// `--profile chief`) beside muster. muster configures it through a shared
// volume, using the compose driver (internal/provision/compose), and never
// starts a container itself: there is no Docker socket.
//
// 🔴 ONE VARIABLE TURNS IT ON, AND IT SETS THE AGENT PLANE ITSELF. The agent
// plane's own variables (MUSTER_AGENT_PROVISIONER, _GATEWAY, _KINDS,
// _CC_IMAGE, _CC_ACCOUNTS, …) describe a Kubernetes deployment that dispatches
// agents. On Compose they would be a puzzle with one right answer, so
// applyComposeChief fills them in and REFUSES any that the environment set:
// a Compose chief beside a hand-set provisioner is two claims about where
// agents run.
//
// What it sets:
//   - the provisioner is the compose driver, admitting exactly one instance:
//     the admin's chief (agents.ChiefName, user 1) — the container is one;
//   - the only agent kind is claude-code (the image the container runs);
//   - the chat wire is hooks-sha256 (agentgateway's), with a model sentinel
//     ccd never reads;
//   - the chief's Claude account is api.ComposeChiefAccount, holding
//     MUSTER_CHIEF_CLAUDE_TOKEN when that is set. Without it the chief runs on
//     its owner's OpenRouter key through muster's LLM proxy (the LLM route).
//
// What it refuses beside it, each by name: every agent-plane variable above,
// officers (they dispatch agents), the chief's dispatch owners (agent_dispatch
// has no target here), chief session runtimes (not part of this runtime yet),
// and tag auto-dispatch.
// ---------------------------------------------------------------------------

const (
	// chiefRuntimeCompose is the one value MUSTER_CHIEF_RUNTIME takes.
	chiefRuntimeCompose = compose.DriverName

	// provisionerCompose is agentProvisioner()'s answer on a Compose chief. It
	// is not a MUSTER_AGENT_PROVISIONER value (provisionerChoices does not list
	// it): only applyComposeChief sets it.
	provisionerCompose = compose.DriverName

	// defaultChiefSpecDir is where the Compose file mounts the shared volume
	// in muster's container.
	defaultChiefSpecDir = "/run/muster-spec"

	// composeChiefImage is the claude-code image reference recorded in the
	// chief's spec. The image that runs is the Compose file's (the driver
	// starts nothing), so this names where it comes from rather than a tag.
	composeChiefImage = "docker-compose-service:chief"

	// composeGatewayModel is the chat wire's model field. ccd ignores it; the
	// gateway requires one (agentgateway.Config.Model).
	composeGatewayModel = "claude-code"

	// composeSpecGroup and composeFilesGroup are the groups the compose driver
	// gives the chief's spec and its files (compose.Config.SpecGroup,
	// FilesGroup). They are the claude-code-agent image's own: 1001 is its
	// muster-keyholder group, the container supervisor's primary group in the
	// Compose file, and 1000 the agent's (images/claude-code-agent/Dockerfile).
	// The agent's session therefore cannot read spec.json, which holds its turn
	// bearer. muster holds both as supplementary groups (group_add).
	composeSpecGroup  = 1001
	composeFilesGroup = 1000
)

// chiefCompose reports whether this server runs the Compose chief.
func (c config) chiefCompose() bool { return c.ChiefRuntime == chiefRuntimeCompose }

// applyComposeChief reads the Compose chief's variables into c and, when it is
// on, fills in the agent plane (see the header). It runs at the end of
// loadConfig, after every agent-plane variable was read, so it can refuse the
// ones that were set.
func (c *config) applyComposeChief(getenv func(string) string) error {
	c.ChiefRuntime = strings.ToLower(strings.TrimSpace(getenv(envChiefRuntime)))
	c.ChiefSpecDir = strings.TrimSpace(getenv(envChiefSpecDir))
	c.ChiefClaudeToken = strings.TrimSpace(getenv(envChiefClaudeToken))
	c.ChiefLLMModel = strings.TrimSpace(getenv(envChiefLLMModel))
	switch c.ChiefRuntime {
	case "":
		if c.ChiefSpecDir != "" || c.ChiefClaudeToken != "" || c.ChiefLLMModel != "" {
			return fmt.Errorf("%s, %s or %s is set but %s is not %s: they configure the chief's Compose "+
				"container and nothing else would read them. Set %s=%s, or unset them",
				envChiefSpecDir, envChiefClaudeToken, envChiefLLMModel, envChiefRuntime, chiefRuntimeCompose, envChiefRuntime, chiefRuntimeCompose)
		}
		return nil
	case chiefRuntimeCompose:
	default:
		return fmt.Errorf("invalid %s %q: the one value it takes is %s (the chief in the docker compose "+
			"install's `chief` container)", envChiefRuntime, c.ChiefRuntime, chiefRuntimeCompose)
	}
	if c.ChiefSpecDir == "" {
		c.ChiefSpecDir = defaultChiefSpecDir
	}
	if c.ChiefLLMModel == "" {
		c.ChiefLLMModel = api.DefaultChiefLLMModel
	}
	if !filepath.IsAbs(c.ChiefSpecDir) {
		return fmt.Errorf("invalid %s %q: it must be an absolute path (the shared volume's mount point in "+
			"this container)", envChiefSpecDir, c.ChiefSpecDir)
	}
	var set []string
	for _, v := range []struct {
		env string
		on  bool
	}{
		{envAgentProvisioner, c.AgentProvisioner != "" && c.AgentProvisioner != provisionerNone},
		{envAgentGateway, c.AgentGateway != "" && c.AgentGateway != gatewayNone},
		{envAgentGatewayModel, c.AgentGatewayModel != ""},
		{envAgentKinds, len(c.AgentKinds) > 0},
		{envAgentImageRepo, c.AgentImageRepo != ""},
		{envAgentRuntimeConfig, c.AgentRuntimeConfig != "" || c.AgentRuntimeInstall != ""},
		{envAgentCCImage, c.AgentCCImage != ""},
		{envAgentCCAccounts, len(c.AgentCCAccountNames) > 0},
		{envAgentNSShared, c.AgentNamespaceShared},
		{envAgentNSClusterRole, c.AgentNamespaceClusterRole != ""},
		{envOfficers, len(c.Officers) > 0},
		{envChiefDispatchOwners, len(c.ChiefDispatchOwners) > 0},
		{envChiefSessionRuntimes, len(c.ChiefSessionRuntimes) > 0},
		{envTagAutoDispatch, c.TagAutoDispatch},
		// The hosted store's credential lands in the chief's spec as a 0600
		// secret file, which the compose driver refuses (it can only write a
		// file the agent's group reads): refused here, at boot, instead of at
		// the chief's first start.
		{envAgentCairnURL, c.AgentCairnURL != ""},
		{envAgentCairnToken, c.AgentCairnToken != ""},
	} {
		if v.on {
			set = append(set, v.env)
		}
	}
	if len(set) > 0 {
		return fmt.Errorf("%s=%s runs the chief in its Compose container and sets the agent plane itself, "+
			"but %s %s also set: those describe agents muster dispatches (on Kubernetes), which this "+
			"install does not do. Unset %s", envChiefRuntime, chiefRuntimeCompose, strings.Join(set, ", "),
			map[bool]string{true: "is", false: "are"}[len(set) == 1], strings.Join(set, ", "))
	}
	c.AgentProvisioner = provisionerCompose
	c.AgentGateway = gatewayHooksSHA256
	c.AgentGatewayModel = composeGatewayModel
	c.AgentKinds = []string{agents.KindClaudeCode}
	c.AgentCCImage = composeChiefImage
	if c.ChiefClaudeToken != "" {
		c.AgentCCAccountNames = []string{api.ComposeChiefAccount}
		c.AgentCCTokens = map[string]string{api.ComposeChiefAccount: c.ChiefClaudeToken}
	}
	return nil
}

// validateComposeChief is validateProvisioner for a Compose chief: what the
// chief needs that the Compose file does not set by itself.
func (c config) validateComposeChief() error {
	var missing []string
	if c.Database == "" {
		missing = append(missing, envDatabase)
	}
	if len(c.ConnectorEncKey) == 0 {
		missing = append(missing, envConnectorEncKey)
	}
	if c.ConnectPort == 0 {
		missing = append(missing, envConnectPort)
	}
	if c.AgentConnectURL == "" {
		missing = append(missing, envAgentConnectURL)
	}
	if c.AgentAPIURL == "" {
		missing = append(missing, envAgentAPIURL)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s=%s but %s %s not set: the chief reaches muster's tools only over the MCP "+
			"server on the connect port (%s, %s), which needs the connector store (%s), and it calls "+
			"back on %s. The Compose file sets all but %s when %s is set",
			envChiefRuntime, chiefRuntimeCompose, strings.Join(missing, ", "),
			map[bool]string{true: "is", false: "are"}[len(missing) == 1],
			envConnectPort, envAgentConnectURL, envConnectorEncKey, envAgentAPIURL, envConnectorEncKey, envChiefRuntime)
	}
	return nil
}

// composeChiefHint is what an unreachable chief container is reported with.
const composeChiefHint = "start it: add chief to COMPOSE_PROFILES in .env and run `docker compose up -d --wait` (docs/features.md#chief)"

// composeAdmit is the compose driver's admission rule: the admin's chief, and
// nothing else. The Compose file runs one chief container.
func composeAdmit(r provision.Ref) error {
	if r.Name != agents.ChiefName {
		return fmt.Errorf("this install runs one chief container, for the admin's chief (%q); %q has none",
			agents.ChiefName, r.Name)
	}
	return nil
}

// composeChown replaces the compose driver's group change. nil (always, outside
// tests) is the driver's own os.Lchown; a test, which cannot give a file
// another user's group, records the calls instead.
var composeChown func(path string, gid int) error

// chiefLLMUpstream replaces the chief proxy's provider API base. "" (always,
// outside tests) is llmproxy.DefaultUpstream; a test points it at a stub
// provider so nothing reaches OpenRouter.
var chiefLLMUpstream string

// buildComposeDriver is buildDriver's compose arm.
func buildComposeDriver(cfg config, logger *log.Logger) (provision.Provisioner, error) {
	d, err := compose.New(compose.Config{Root: cfg.ChiefSpecDir, Admit: composeAdmit, Hint: composeChiefHint,
		SpecGroup: composeSpecGroup, FilesGroup: composeFilesGroup, Chown: composeChown})
	if err != nil {
		return nil, err
	}
	// The port is agentspec.DefaultGatewayPort, the one the claude-code spec
	// always declares (ccd's), not cfg.agentGatewayPort(): MUSTER_AGENT_GATEWAY_PORT
	// moves the gateway kind's port and never this container's.
	logger.Printf("chief runtime: COMPOSE (%s=%s) — spec volume %s, container %s:%d, MCP door %s",
		envChiefRuntime, chiefRuntimeCompose, cfg.ChiefSpecDir, agents.ChiefName, agentspec.DefaultGatewayPort, cfg.AgentConnectURL)
	return d, nil
}

// composeSpec is api.ComposeChief.Spec: the generation (the compose driver's
// restart signal) and the replicas (0 stopped, 1 live) of the chief's
// spec.json under root.
func composeSpec(root string) func() (int64, int, error) {
	return func() (int64, int, error) {
		raw, err := os.ReadFile(filepath.Join(root, agents.ChiefName, "spec.json"))
		if err != nil {
			return 0, 0, err
		}
		var cs compose.ContainerSpec
		if err := json.Unmarshal(raw, &cs); err != nil {
			return 0, 0, err
		}
		return cs.Generation, cs.Replicas, nil
	}
}

// composeChiefCredential is THE rule for which credential the Compose chief
// runs on, handed to both of its readers (the api's Account page and refusal,
// and agentprovision's spec builder) so they cannot disagree:
//
//   - MUSTER_CHIEF_CLAUDE_TOKEN set: the Claude subscription (api.ComposeChiefAccount);
//   - otherwise the owner's OpenRouter key (the credential plane: the admin's
//     is MUSTER_AGENT_OPENROUTER_KEY) through muster's LLM proxy;
//   - neither: a refusal saying what to set.
func composeChiefCredential(cfg config, plane *credentials.Plane) func(ctx context.Context, owner int64) (api.ComposeChiefCred, error) {
	token := cfg.ChiefClaudeToken != ""
	return func(ctx context.Context, owner int64) (api.ComposeChiefCred, error) {
		if token {
			return api.ComposeChiefClaude, nil
		}
		key, err := plane.OpenRouterKey(ctx, owner)
		if err != nil && !credentials.IsNoCredential(err) {
			return "", err
		}
		if strings.TrimSpace(key) != "" {
			return api.ComposeChiefOpenRouter, nil
		}
		return "", fmt.Errorf("%w: %s", api.ErrComposeChiefNoCredential, api.ComposeChiefNeedsCredential)
	}
}

// composeChiefRoute is agentprovision.Config.ComposeChiefRoute over the same
// rule: the LLM route exactly when the chief runs on OpenRouter.
func composeChiefRoute(cred func(context.Context, int64) (api.ComposeChiefCred, error)) func(context.Context, int64) (bool, error) {
	return func(ctx context.Context, owner int64) (bool, error) {
		c, err := cred(ctx, owner)
		if err != nil {
			return false, err
		}
		return c == api.ComposeChiefOpenRouter, nil
	}
}
