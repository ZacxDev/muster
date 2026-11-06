package main

import (
	"strings"
	"testing"
)

// TestTheTestLLMUpstreamNeedsTheTestFlag: MUSTER_TEST_LLM_UPSTREAM sends every
// chief request and the OpenRouter key to its URL, so it is refused unless
// MUSTER_TEST_MODE=1 is set beside it and the Compose chief runtime is on; the
// flag alone, a non-URL and any other flag value are refused too. Control:
// the full pair is accepted and reaches the config.
func TestTheTestLLMUpstreamNeedsTheTestFlag(t *testing.T) {
	base := func() map[string]string {
		e := composeEnv("/run/muster-spec")
		e[envTestLLMUpstream] = "http://fake-llm.example.invalid:8080/"
		e[envTestMode] = "1"
		return e
	}
	cfg, err := loadEnv(t, base())
	if err != nil || cfg.LLMTestUpstream != "http://fake-llm.example.invalid:8080" {
		t.Fatalf("control: the full pair gave %q, %v", cfg.LLMTestUpstream, err)
	}
	for name, c := range map[string]struct {
		edit func(map[string]string)
		want string
	}{
		"no test flag":       {func(e map[string]string) { delete(e, envTestMode) }, "TEST ONLY"},
		"flag not 1":         {func(e map[string]string) { e[envTestMode] = "yes" }, "TEST ONLY"},
		"flag alone":         {func(e map[string]string) { delete(e, envTestLLMUpstream) }, "arms nothing else"},
		"not a URL":          {func(e map[string]string) { e[envTestLLMUpstream] = "fake-llm:8080" }, "not an http(s) URL"},
		"no compose runtime": {func(e map[string]string) { delete(e, envChiefRuntime) }, "nothing else reads it"},
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
