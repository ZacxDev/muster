package main

import (
	"encoding/json"
	"testing"
)

// TestShipReadReposWithoutTheDeploymentAppRefuseToBoot: a SHIPPING officer's
// read_repos are read with the DEPLOYMENT's GitHub App (PushGate.ReadCred =
// deploymentReadCred), which exists only when MUSTER_GITHUB_APP_ID is set.
// Without it those reads would go anonymously while the officer's kickoff
// promises a read-only token for each, so that configuration refuses to boot
// with the full text pinned below. Controls: the same officer WITH the
// deployment App loads; the same shipping officer WITHOUT read_repos and
// without the App loads (the refusal is not a blanket one on `ship`).
func TestShipReadReposWithoutTheDeploymentAppRefuseToBoot(t *testing.T) {
	keyPEM := testAppKeyPEM(t)
	ship := map[string]any{
		"stage": "release", "github_app": map[string]any{"id": 9001, "account": "example-org:4242"},
		"required_checks": []string{"ci / build", "ci / e2e"}, "review_check": "pr-review", "review_app": "reviewer",
		"migrations": "db/migrations/", "sensitive_paths": []string{"internal/auth/", "*token*"},
		"notice":      map[string]any{"channel": "ch/example-alerts"},
		"quiet_hours": map[string]any{"tz": "America/New_York", "start": "22:00", "end": "08:00", "weekends": true},
		"rollout":     map[string]any{"health_url": "https://svc.example.test/healthz", "rollback": "auto"},
	}
	load := func(readRepos []string, withApp bool) error {
		t.Helper()
		decl := map[string]any{"id": "engineering:example-web", "hq_url": "https://hq.example.invalid/api/v1",
			"repo": "example-org/example-web", "ship": ship}
		if readRepos != nil {
			decl["read_repos"] = readRepos
		}
		raw, err := json.Marshal([]any{decl})
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{
			envDatabase: "postgres://unused", envStandalone: "1",
			envOfficers: string(raw),
			"MUSTER_OFFICER_TOKEN_ENGINEERING_EXAMPLE_WEB":            "fixture-officer-token-cfg-3",
			"MUSTER_OFFICER_GITHUB_APP_KEY_ENGINEERING_EXAMPLE_WEB":   keyPEM,
			"MUSTER_OFFICER_ESCALATE_WEBHOOK_ENGINEERING_EXAMPLE_WEB": "https://hooks.example.test/escalate/fixture",
			envConnectorEncKey: connectorKey(3), envConnectPort: "18191",
			envAgentConnectURL: "http://muster.example.test:18191",
		}
		if withApp {
			env[envGitHubAppID], env[envGitHubAppKey], env[envGitHubAppAccounts] = "12345", keyPEM, "example-org:4242"
		}
		cfg, err := loadConfig(func(n string) string { return env[n] })
		if err != nil {
			return err
		}
		return cfg.validate()
	}
	const want = "officer engineering:example-web declares `read_repos` beside a `ship` block, but MUSTER_GITHUB_APP_ID is not set: " +
		"a shipping officer reads its read_repos with the deployment's own GitHub App (a read-only token for each), and without one " +
		"those reads would go anonymously. Set MUSTER_GITHUB_APP_ID (with MUSTER_GITHUB_APP_PRIVATE_KEY and MUSTER_GITHUB_APP_ACCOUNTS), " +
		"or remove read_repos"
	rr := []string{"example-org/flags-state", "example-org/docs"}
	if err := load(rr, false); err == nil || err.Error() != want {
		t.Fatalf("ship + read_repos with no deployment App: err = %v,\nwant %q", err, want)
	}
	if err := load(rr, true); err != nil {
		t.Fatalf("control: ship + read_repos WITH the deployment App is refused: %v", err)
	}
	if err := load(nil, false); err != nil {
		t.Fatalf("control: ship WITHOUT read_repos and no deployment App is refused: %v", err)
	}
}
