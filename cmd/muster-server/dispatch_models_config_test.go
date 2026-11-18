package main

import (
	"slices"
	"strings"
	"testing"
)

// TestTheDispatchModelListIsReadOrTheServerStops: MUSTER_CHIEF_DISPATCH_MODELS
// (docs/design/chief-dispatch.md 2.8, decision 15). Unset or blank is the
// default list; "none" lists no model; a list is read in order, trimmed. A
// malformed entry, a repeat, or more than fit on the card STOPS the server,
// naming the entry — never dropped silently.
func TestTheDispatchModelListIsReadOrTheServerStops(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	load := func(v string) (config, error) {
		return loadConfig(func(k string) string {
			if k == envChiefDispatchModels {
				return v
			}
			return base[k]
		})
	}
	for _, c := range []struct {
		v    string
		want []string
	}{
		{"", []string{"qwen/qwen3-coder", "moonshotai/kimi-k2-0905", "z-ai/glm-4.6", "deepseek/deepseek-v3.2"}},
		{"  ", []string{"qwen/qwen3-coder", "moonshotai/kimi-k2-0905", "z-ai/glm-4.6", "deepseek/deepseek-v3.2"}},
		{"none", []string{}},
		{" example/model-b , example/model-a:free ", []string{"example/model-b", "example/model-a:free"}},
	} {
		cfg, err := load(c.v)
		if err != nil {
			t.Fatalf("%q refused: %v", c.v, err)
		}
		if !slices.Equal(cfg.ChiefDispatchModels, c.want) || cfg.ChiefDispatchModels == nil {
			t.Errorf("%q = %q, want %q", c.v, cfg.ChiefDispatchModels, c.want)
		}
	}
	for v, want := range map[string]string{
		"example/model-a,Model B":               `invalid MUSTER_CHIEF_DISPATCH_MODELS: "Model B" is not an OpenRouter model id (<vendor>/<model>)`,
		"openrouter/example/model-a":            `invalid MUSTER_CHIEF_DISPATCH_MODELS: "openrouter/example/model-a" is not an OpenRouter model id (<vendor>/<model>)`,
		"example/model-a,,example/model-b":      `invalid MUSTER_CHIEF_DISPATCH_MODELS: "" is not an OpenRouter model id (<vendor>/<model>)`,
		"example/model-a,example/model-a":       `invalid MUSTER_CHIEF_DISPATCH_MODELS: "example/model-a" is listed twice`,
		strings.Repeat("example/m,", 8) + "x/y": `invalid MUSTER_CHIEF_DISPATCH_MODELS: "example/m" is listed twice`,
		"a/1,a/2,a/3,a/4,a/5,a/6,a/7,a/8,a/9":   "invalid MUSTER_CHIEF_DISPATCH_MODELS: 9 models; at most 8 fit on the card",
	} {
		if _, err := load(v); err == nil || err.Error() != want {
			t.Errorf("%q: %v; want %q", v, err, want)
		}
	}
	if envChiefDispatchModels != "MUSTER_CHIEF_DISPATCH_MODELS" {
		t.Errorf("the variable is spelled %q", envChiefDispatchModels)
	}
}
