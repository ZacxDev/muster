package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/credentials"
)

// TestFocusModelIsReadWithTheOldDefault: MUSTER_FOCUS_MODEL reaches the
// config, trimmed; unset or blank it is scaledown/summarize — the model focus
// used before the variable existed, so nothing changes until an operator sets
// it.
func TestFocusModelIsReadWithTheOldDefault(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	for _, c := range []struct {
		v    string
		set  bool
		want string
	}{
		{"", false, "scaledown/summarize"},
		{"   ", true, "scaledown/summarize"},
		{" example/model-x ", true, "example/model-x"},
	} {
		cfg, err := loadConfig(func(k string) string {
			if k == envFocusModel && c.set {
				return c.v
			}
			return base[k]
		})
		if err != nil {
			t.Fatalf("%s=%q refused: %v", envFocusModel, c.v, err)
		}
		if cfg.FocusModel != c.want {
			t.Errorf("%s=%q (set %v) = %q, want %q", envFocusModel, c.v, c.set, cfg.FocusModel, c.want)
		}
	}
	if envFocusModel != "MUSTER_FOCUS_MODEL" {
		t.Errorf("the variable is spelled %q", envFocusModel)
	}
}

// TestTheFocusSummarizerUsesTheConfiguredModel: the wiring, not just the
// parse — the summarizer the server builds asks for cfg.FocusModel and spends
// the plane's key.
func TestTheFocusSummarizerUsesTheConfiguredModel(t *testing.T) {
	plane := &credentials.Plane{EnvOpenRouterKey: "a-synthetic-env-key"}
	sd := focusSummarizer(config{FocusModel: "example/model-x"}, plane)
	if sd.Model != "example/model-x" {
		t.Errorf("Model = %q, want example/model-x", sd.Model)
	}
	if sd.Key == nil || sd.Missing == nil {
		t.Fatal("the summarizer has no key source or no missing-key test")
	}
	if _, err := sd.Key(context.Background(), 1); err != nil && !sd.Missing(err) {
		t.Errorf("Key: %v", err)
	}
}
