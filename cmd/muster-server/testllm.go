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
// ATTACHES TO IT, TO THAT URL. The guard is that deploy/docker-compose.yml does
// not forward it: only the test override file sets it. It is refused unless
// the Compose chief runtime is on (nothing else reads it) and it is an http(s)
// URL, and the boot log says so loudly when it is in effect. (A second "this is
// a test" flag was deleted in round 0: whoever can set this variable can set
// that one too, so it added friction and no protection.)
// ---------------------------------------------------------------------------

func (c *config) applyTestLLMUpstream(getenv func(string) string) error {
	up := strings.TrimRight(strings.TrimSpace(getenv(envTestLLMUpstream)), "/")
	if up == "" {
		return nil
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
