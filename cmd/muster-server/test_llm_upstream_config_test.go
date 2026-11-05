package main

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// TestTheTestLLMUpstreamIsParsedRefusedUnlessHTTPAndAnnouncedLoudly:
// MUSTER_TEST_LLM_UPSTREAM reaches config.LLMUpstreamOverride (trailing slash
// trimmed), anything but an http(s) URL with a host refuses to boot naming the
// variable, and a set value is announced on the banner as an override that
// receives every request and its key.
func TestTheTestLLMUpstreamIsParsedRefusedUnlessHTTPAndAnnouncedLoudly(t *testing.T) {
	load := func(v string) (config, error) {
		c, err := loadConfig(func(k string) string {
			if k == envTestLLMUpstream {
				return v
			}
			return ""
		})
		return c, err
	}
	c, err := load("http://fake-llm.example.invalid:8080/")
	if err != nil {
		t.Fatal(err)
	}
	if c.LLMUpstreamOverride != "http://fake-llm.example.invalid:8080" {
		t.Fatalf("LLMUpstreamOverride = %q, want the value with its trailing slash trimmed", c.LLMUpstreamOverride)
	}
	// Control: with a valid value the rest of this minimal config validates,
	// so each refusal below is this variable's own.
	valid := config{Database: "postgres://muster@db.example.invalid/muster", LLMUpstreamOverride: "https://fake-llm.example.invalid"}
	if err := valid.validate(); err != nil {
		t.Fatalf("control: a valid override is refused: %v", err)
	}
	for _, bad := range []string{"ftp://fake-llm.example.invalid", "fake-llm.example.invalid:8080", "http://"} {
		cfg := valid
		cfg.LLMUpstreamOverride = bad
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), envTestLLMUpstream) || !strings.Contains(err.Error(), "TEST-ONLY") {
			t.Errorf("%q: validate = %v, want a refusal naming %s as test only", bad, err, envTestLLMUpstream)
		}
	}
	if c, _ := load(""); c.LLMUpstreamOverride != "" {
		t.Fatalf("unset: LLMUpstreamOverride = %q, want empty (the provider)", c.LLMUpstreamOverride)
	}
	on := renderBanner(t, config{LLMUpstreamOverride: "http://fake-llm.example.invalid:8080"}, api.Extensions{})
	if !strings.Contains(on, "llm proxy: 🔴 upstream OVERRIDDEN to http://fake-llm.example.invalid:8080 by "+envTestLLMUpstream+
		" — test only, every request and its key go there") {
		t.Errorf("set: no loud override line:\n%s", on)
	}
	off := renderBanner(t, config{}, api.Extensions{})
	if strings.Contains(off, "OVERRIDDEN") || !strings.Contains(off, "llm proxy: upstream is the provider (https://openrouter.ai/api") {
		t.Errorf("unset: want the provider line and no override:\n%s", off)
	}
}
