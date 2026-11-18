package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/credentials"
)

// ---------------------------------------------------------------------------
// THE CHIEF ON THE LLM PROXY, ON EITHER RUNTIME
//
// A claude-code chief can run on OpenRouter instead of a Claude account: its
// pod is put on the LLM route (CCD_LLM_ROUTE=muster: no subscription token, its
// Claude Code calls $MUSTER_CONNECT_URL/llm with its own muster token) and
// api.EnableChiefLLM serves it there on the server's OpenRouter key, the model
// pinned (MUSTER_CHIEF_LLM_MODEL).
//
//   - On the docker compose install (MUSTER_CHIEF_RUNTIME=compose) the
//     credential decides: no MUSTER_CHIEF_CLAUDE_TOKEN means OpenRouter
//     (composeChiefCredential).
//   - On Kubernetes it is an explicit opt-in, MUSTER_CHIEF_LLM=openrouter, and
//     it covers the admin's chief alone (api.ChiefLLMOwner: the proxy serves no
//     other user's chief, so a member's chief keeps its Claude account). Unset
//     is today's behaviour exactly: the chief runs on its Claude account and
//     the proxy serves no chief.
//
// 🔴 ONE RULE: chiefLLMRoute is the only place that decides which owner's chief
// is on the route, and agentprovision.ChiefLLMRouted the only predicate that
// applies it to an agent (a claude-code agent with the reserved chief slug).
// The spec builder asks that predicate over agentprovision.Config.ChiefLLMRoute
// (provisioner.go); off Compose the LLM proxy asks the same predicate over the
// same rule on every request (api.ChiefLLM.Routed, main.go), so it serves
// exactly the chief whose pod is built on the route. On Compose the proxy asks
// the credential rule (api.ComposeChief.Credential) that chiefLLMRoute wraps.
// ---------------------------------------------------------------------------

// chiefLLMOpenRouter is the one value MUSTER_CHIEF_LLM takes.
const chiefLLMOpenRouter = "openrouter"

// applyChiefLLM reads MUSTER_CHIEF_LLM. It runs before applyComposeChief,
// which reads MUSTER_CHIEF_LLM_MODEL and refuses it unless one of the two puts
// the chief on the proxy.
func (c *config) applyChiefLLM(getenv func(string) string) error {
	c.ChiefLLM = strings.ToLower(strings.TrimSpace(getenv(envChiefLLM)))
	switch c.ChiefLLM {
	case "", chiefLLMOpenRouter:
		return nil
	}
	return fmt.Errorf("invalid %s %q: the one value it takes is %s (the admin's claude-code chief on the server's "+
		"OpenRouter key, through muster's LLM proxy); unset it to keep the chief on its Claude account",
		envChiefLLM, c.ChiefLLM, chiefLLMOpenRouter)
}

// chiefLLMOptIn reports whether MUSTER_CHIEF_LLM=openrouter (the Kubernetes
// chief's switch).
func (c config) chiefLLMOptIn() bool { return c.ChiefLLM == chiefLLMOpenRouter }

// chiefOnLLMProxy reports whether this server runs a chief on the LLM proxy
// (calls api.EnableChiefLLM).
func (c config) chiefOnLLMProxy() bool { return c.chiefCompose() || c.chiefLLMOptIn() }

// validateChiefLLM is the opt-in's boot refusals: what a Kubernetes chief on
// the route needs. Each is a combination that would otherwise start a chief
// whose every turn fails.
func (c config) validateChiefLLM() error {
	if !c.chiefLLMOptIn() {
		return nil
	}
	if c.chiefCompose() {
		return fmt.Errorf("%s=%s is the Kubernetes chief's switch; with %s=%s the chief's credential decides "+
			"(no %s means OpenRouter). Unset %s", envChiefLLM, chiefLLMOpenRouter, envChiefRuntime, chiefRuntimeCompose,
			envChiefClaudeToken, envChiefLLM)
	}
	var missing []string
	if c.agentProvisioner() == provisionerNone {
		missing = append(missing, envAgentProvisioner)
	}
	if !c.claudeCodeEnabled() {
		missing = append(missing, envAgentKinds+" (with claude-code)")
	}
	if c.AgentConnectURL == "" {
		missing = append(missing, envAgentConnectURL)
	}
	if c.AgentOpenRouterKey == "" {
		missing = append(missing, envAgentOpenRouterKey)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s=%s puts the admin's claude-code chief on the LLM proxy, which needs %s: set %s, "+
			"or unset %s to keep the chief on its Claude account", envChiefLLM, chiefLLMOpenRouter,
			strings.Join(missing, ", "), strings.Join(missing, ", "), envChiefLLM)
	}
	return nil
}

// chiefLLMRoute is THE rule for whether owner's claude-code chief runs on the
// LLM route, handed to the spec builder (agentprovision.Config.ChiefLLMRoute).
// nil when no chief is: every chief is built on its Claude account, as before.
func chiefLLMRoute(cfg config, plane *credentials.Plane) func(ctx context.Context, owner int64) (bool, error) {
	switch {
	case cfg.chiefCompose():
		return composeChiefRoute(composeChiefCredential(cfg, plane))
	case cfg.chiefLLMOptIn():
		return func(_ context.Context, owner int64) (bool, error) { return api.ChiefLLMOwner(owner), nil }
	}
	return nil
}
