package main

import (
	"strings"
	"testing"
)

// TestTheTestLLMUpstreamNeedsTheComposeChief: MUSTER_TEST_LLM_UPSTREAM sends
// every chief request and the OpenRouter key to its URL, so it is refused
// unless the Compose chief runtime is on, and when it is not an http(s) URL.
// Control: beside the runtime it is accepted and reaches the config.
func TestTheTestLLMUpstreamNeedsTheComposeChief(t *testing.T) {
	base := func() map[string]string {
		e := composeEnv("/run/muster-spec")
		e[envTestLLMUpstream] = "http://fake-llm.example.invalid:8080/"
		return e
	}
	cfg, err := loadEnv(t, base())
	if err != nil || cfg.LLMTestUpstream != "http://fake-llm.example.invalid:8080" {
		t.Fatalf("control: beside the Compose chief it gave %q, %v", cfg.LLMTestUpstream, err)
	}
	for name, c := range map[string]struct {
		edit func(map[string]string)
		want string
	}{
		"not a URL": {func(e map[string]string) { e[envTestLLMUpstream] = "fake-llm:8080" }, "not an http(s) URL"},
		// A server with no Compose chief at all (no runtime, no spec dir), so no
		// other chief check can be the one that refuses.
		"no compose runtime": {func(e map[string]string) { delete(e, envChiefRuntime); delete(e, envChiefSpecDir) }, "nothing else reads it"},
	} {
		e := base()
		c.edit(e)
		if _, err := loadEnv(t, e); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal containing %q", name, err, c.want)
		}
	}
	// Unset is today's behaviour: no override.
	if cfg, err := loadEnv(t, composeEnv("/run/muster-spec")); err != nil || cfg.LLMTestUpstream != "" {
		t.Fatalf("unset: %q, %v", cfg.LLMTestUpstream, err)
	}
}
