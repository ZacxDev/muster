package main

import (
	"fmt"
	"net/url"
	"strings"
)

// ---------------------------------------------------------------------------
// MUSTER_TEST_LLM_UPSTREAM — TEST ONLY
//
// It replaces the Compose chief's LLM proxy upstream (llmproxy.DefaultUpstream,
// OpenRouter) with another server: the Compose smoke test points it at a fake
// Anthropic-Messages server (tests/compose-chief.override.yml), so the chief's
// real Claude Code CLI runs real turns with no key.
//
// 🔴 IT SENDS EVERY CHIEF REQUEST, AND THE OWNER'S OPENROUTER KEY MUSTER
// ATTACHES TO IT, TO THAT URL. So it is refused unless MUSTER_TEST_MODE=1 is
// set beside it — a second, explicit statement that this is a test — and
// unless the chief runtime is on (nothing else reads it); a set
// MUSTER_TEST_MODE with no override is refused too, as an armed switch.
// Neither is forwarded by deploy/docker-compose.yml: only the test override
// file sets them. The boot log says so loudly when it is in effect.
// ---------------------------------------------------------------------------

func (c *config) applyTestLLMUpstream(getenv func(string) string) error {
	up := strings.TrimRight(strings.TrimSpace(getenv(envTestLLMUpstream)), "/")
	mode := strings.TrimSpace(getenv(envTestMode))
	switch {
	case up == "" && mode == "":
		return nil
	case up == "":
		return fmt.Errorf("%s=%q is set but %s is not: it arms nothing else. Unset it", envTestMode, mode, envTestLLMUpstream)
	case mode != "1":
		return fmt.Errorf("%s is set but %s is not 1: it sends every chief request, and the OpenRouter key muster "+
			"attaches, to %q. It is TEST ONLY; set %s=1 beside it in a test, and never in a real deployment",
			envTestLLMUpstream, envTestMode, up, envTestMode)
	}
	if u, err := url.Parse(up); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s=%q is not an http(s) URL", envTestLLMUpstream, up)
	}
	if strings.ToLower(strings.TrimSpace(getenv(envChiefRuntime))) != chiefRuntimeCompose {
		return fmt.Errorf("%s is set but %s is not %s: it is the Compose chief's LLM proxy upstream and nothing else reads it",
			envTestLLMUpstream, envChiefRuntime, chiefRuntimeCompose)
	}
	c.LLMTestUpstream = up
	return nil
}
