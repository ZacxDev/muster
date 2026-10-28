package main

import (
	"encoding/base64"
	"testing"
)

// TestTheRemovedGitHubOAuthVariablesDoNotStopBoot: nothing reads the removed
// GitHub OAuth connection's variables any more, and loadConfig refuses no
// variable it does not read — so a deployment that still sets all four boots
// through the real loader, including on an encryption key that does not decode
// (the old loader refused it) and on one equal to the connector key (the old
// validation refused it as shared). The names are spelled here, not taken from
// constants, because the binary no longer has any.
func TestTheRemovedGitHubOAuthVariablesDoNotStopBoot(t *testing.T) {
	load := func(env map[string]string) {
		t.Helper()
		full := map[string]string{envDatabase: "postgres://unused", envStandalone: "1"}
		for k, v := range env {
			full[k] = v
		}
		cfg, err := loadConfig(func(n string) string { return full[n] })
		if err != nil {
			t.Fatalf("loadConfig refused the removed OAuth variables: %v", err)
		}
		if err := cfg.validate(); err != nil {
			t.Fatalf("validate refused the removed OAuth variables: %v", err)
		}
	}
	load(map[string]string{
		"MUSTER_GITHUB_ENCRYPTION_KEY": "not-base64-and-not-32-bytes!",
		"MUSTER_GITHUB_TOKEN":          "ghp_example_not_a_real_token",
		"GITHUB_CLIENT_ID":             "example-client-id",
		"GITHUB_CLIENT_SECRET":         "example-client-secret",
	})
	shared := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	load(map[string]string{
		"MUSTER_GITHUB_ENCRYPTION_KEY": shared,
		envConnectorEncKey:             shared,
		envAgentConnectURL:             "http://muster.example.test:18191/",
		envConnectPort:                 "18191",
	})
}
