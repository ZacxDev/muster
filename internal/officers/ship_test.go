package officers_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

var (
	shipKeyOnce sync.Once
	shipKeyPEM  string
)

// testAppKey is a throwaway RSA key generated for this test run.
func testAppKey(t *testing.T) string {
	t.Helper()
	shipKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		shipKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	})
	return shipKeyPEM
}

const shipID = "engineering:example-web"

func shipEnv(t *testing.T) map[string]string {
	return map[string]string{
		"MUSTER_OFFICER_TOKEN_ENGINEERING_EXAMPLE_WEB":            "tok-ship",
		"MUSTER_OFFICER_GITHUB_APP_KEY_ENGINEERING_EXAMPLE_WEB":   testAppKey(t),
		"MUSTER_OFFICER_ESCALATE_WEBHOOK_ENGINEERING_EXAMPLE_WEB": "https://hooks.example.test/escalate/fixture",
	}
}

// fullShip is a valid release-stage declaration; tests edit the map.
func fullShip() map[string]any {
	return map[string]any{
		"stage": "release", "github_app": map[string]any{"id": 9001, "account": "example-org:4242"},
		"required_checks": []string{"ci / build", "ci / e2e"}, "review_check": "pr-review", "review_app": "reviewer",
		"migrations": "db/migrations/", "sensitive_paths": []string{"internal/auth/", "*token*"},
		"notice":      map[string]any{"channel": "ch/example-alerts"},
		"quiet_hours": map[string]any{"tz": "America/New_York", "start": "22:00", "end": "08:00", "weekends": true},
		"rollout":     map[string]any{"health_url": "https://svc.example.test/healthz", "rollback": "auto"},
	}
}

func parseShip(t *testing.T, ship map[string]any, envm map[string]string) ([]officers.Officer, error) {
	t.Helper()
	decl := map[string]any{"id": shipID, "hq_url": "https://hq.example.com/api/v1", "repo": "example-org/example-web", "ship": ship}
	raw, _ := json.Marshal([]any{decl})
	return officers.Parse(string(raw), env(envm), "muster-agent-")
}

// TestParseReadsAShipBlockWithItsDefaultsAndSecrets.
func TestParseReadsAShipBlockWithItsDefaultsAndSecrets(t *testing.T) {
	list, err := parseShip(t, fullShip(), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	s := list[0].Ship
	if s == nil || s.Stage != officers.StageRelease || s.Base != "main" || s.BranchPrefix != "officer/" || s.AppID != 9001 ||
		s.AppAccount.ID != 4242 || s.ReleasesPerDay != officers.DefaultReleasesPerDay || s.MinReleaseGap != 30*time.Minute ||
		s.NoticeURLPath(shipID) != "/officers/engineering:example-web/notices" || s.NoticeChannel != "ch/example-alerts" ||
		s.Rollout.LiveReads != officers.DefaultLiveReads || s.Rollout.Rollback != officers.RollbackAuto || s.AppKey() == nil ||
		s.EscalateWebhook() == "" || !s.MayMerge() || !s.MayRelease() {
		t.Fatalf("parsed %#v", s)
	}
	// Neither secret is printed.
	for _, out := range []string{s.String(), fmt.Sprintf("%v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%+v", list[0])} {
		if strings.Contains(out, "hooks.example.test") || strings.Contains(out, testAppKey(t)[40:80]) {
			t.Fatalf("a secret printed: %s", out)
		}
	}
	// 22:00-08:00 America/New_York wraps midnight; weekends are quiet.
	ny, _ := time.LoadLocation("America/New_York")
	for when, quiet := range map[time.Time]bool{
		time.Date(2000, 1, 5, 23, 0, 0, 0, ny):  true,
		time.Date(2000, 1, 5, 7, 59, 0, 0, ny):  true,
		time.Date(2000, 1, 5, 8, 0, 0, 0, ny):   false,
		time.Date(2000, 1, 5, 21, 59, 0, 0, ny): false,
		time.Date(2000, 1, 8, 12, 0, 0, 0, ny):  true, // a Saturday
	} {
		if s.Quiet.Quiet(when) != quiet {
			t.Errorf("Quiet(%s) = %v, want %v", when, !quiet, quiet)
		}
	}
}

// TestParseRefusesAShipBlockThatWouldWidenOrBlind: each refusal names what
// is wrong; none falls back to a wider default.
func TestParseRefusesAShipBlockThatWouldWidenOrBlind(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(m map[string]any, e map[string]string)
		want string
	}{
		"no stage":            {func(m map[string]any, _ map[string]string) { delete(m, "stage") }, "stage is required"},
		"unknown stage":       {func(m map[string]any, _ map[string]string) { m["stage"] = "yolo" }, "is not"},
		"release, no rollout": {func(m map[string]any, _ map[string]string) { delete(m, "rollout") }, "needs a rollout"},
		"no checks":           {func(m map[string]any, _ map[string]string) { m["required_checks"] = []string{} }, "required_checks"},
		"no review check":     {func(m map[string]any, _ map[string]string) { delete(m, "review_check") }, "review_check"},
		"no app":              {func(m map[string]any, _ map[string]string) { delete(m, "github_app") }, "github_app.id"},
		"app account by login": {func(m map[string]any, _ map[string]string) {
			m["github_app"] = map[string]any{"id": 9001, "account": "example-org"}
		}, "<login>:<numeric id>"},
		"app on another owner": {func(m map[string]any, _ map[string]string) {
			m["github_app"] = map[string]any{"id": 9001, "account": "someone-else:77"}
		}, "not the owner"},
		"prefix is the base":   {func(m map[string]any, _ map[string]string) { m["branch_prefix"] = "main/" }, "overlap"},
		"prefix without slash": {func(m map[string]any, _ map[string]string) { m["branch_prefix"] = "officer" }, "ending in '/'"},
		"paths, no notice":     {func(m map[string]any, _ map[string]string) { delete(m, "notice") }, "needs a notice"},
		"bad channel":          {func(m map[string]any, _ map[string]string) { m["notice"] = map[string]any{"channel": "nope"} }, "notice.channel"},
		"no rollback policy": {func(m map[string]any, _ map[string]string) {
			m["rollout"] = map[string]any{"health_url": "https://h.example.test/"}
		}, "rollout.rollback"},
		"no app key": {func(_ map[string]any, e map[string]string) {
			delete(e, "MUSTER_OFFICER_GITHUB_APP_KEY_ENGINEERING_EXAMPLE_WEB")
		}, "MUSTER_OFFICER_GITHUB_APP_KEY_ENGINEERING_EXAMPLE_WEB"},
		"bad app key": {func(_ map[string]any, e map[string]string) {
			e["MUSTER_OFFICER_GITHUB_APP_KEY_ENGINEERING_EXAMPLE_WEB"] = "not a pem"
		}, "PEM"},
		"no escalation webhook": {func(_ map[string]any, e map[string]string) {
			delete(e, "MUSTER_OFFICER_ESCALATE_WEBHOOK_ENGINEERING_EXAMPLE_WEB")
		}, "MUSTER_OFFICER_ESCALATE_WEBHOOK_ENGINEERING_EXAMPLE_WEB"},
		"plain-http webhook": {func(_ map[string]any, e map[string]string) {
			e["MUSTER_OFFICER_ESCALATE_WEBHOOK_ENGINEERING_EXAMPLE_WEB"] = "http://hooks.example.test/x"
		}, "https webhook"},
		"bad quiet hours": {func(m map[string]any, _ map[string]string) {
			m["quiet_hours"] = map[string]any{"tz": "America/New_York", "start": "10pm", "end": "08:00"}
		}, "HH:MM"},
		"unknown field": {func(m map[string]any, _ map[string]string) { m["protected_paths"] = []string{"x"} }, "unknown field"},
	} {
		m, e := fullShip(), shipEnv(t)
		c.edit(m, e)
		if _, err := parseShip(t, m, e); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to name %q", name, err, c.want)
		}
	}
	// A ship block needs the officer's repo.
	raw := `[{"id":"` + shipID + `","hq_url":"https://hq.example.com/api/v1","ship":{"stage":"shadow"}}]`
	if _, err := officers.Parse(raw, env(shipEnv(t)), "muster-agent-"); err == nil || !strings.Contains(err.Error(), "needs `repo`") {
		t.Errorf("a ship block with no repo: %v", err)
	}
}

// TestAShadowShipNeedsNoRolloutAndMayDoNothingGated.
func TestAShadowShipNeedsNoRolloutAndMayDoNothingGated(t *testing.T) {
	m := fullShip()
	m["stage"] = "shadow"
	delete(m, "rollout")
	e := shipEnv(t)
	delete(e, "MUSTER_OFFICER_ESCALATE_WEBHOOK_ENGINEERING_EXAMPLE_WEB")
	list, err := parseShip(t, m, e)
	if err != nil {
		t.Fatal(err)
	}
	if s := list[0].Ship; s.MayMerge() || s.MayRelease() {
		t.Fatalf("shadow may merge %v, release %v", s.MayMerge(), s.MayRelease())
	}
	if !officers.ToolOffered(list[0], officers.ToolMergePR) {
		t.Fatal("a shipping officer is not offered officer_merge_pr (it is offered, and refuses by stage)")
	}
	plain := officers.Officer{ID: "triage:x"}
	for _, tool := range []string{officers.ToolMergePR, officers.ToolTagRelease, officers.ToolRolloutStatus, officers.ToolMarkReady} {
		if officers.ToolOffered(plain, tool) {
			t.Errorf("an officer with no ship block is offered %s", tool)
		}
	}
}

// TestSensitiveMatch pins the three pattern shapes.
func TestSensitiveMatch(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{"internal/auth/", "internal/auth/session.go", true},
		{"internal/auth/", "internal/authz/x.go", false},
		{"internal/store/officer*", "internal/store/officer_release.go", true},
		{"internal/store/officer*", "internal/store/sub/officer.go", false},
		{"*token*", "internal/api/api_tokens.go", true},
		{"*token*", "internal/api/api.go", false},
		{"Dockerfile*", "deploy/Dockerfile.prod", true},
		{"*auth*", "internal/oauth/x.go", true},
	} {
		if got := officers.SensitiveMatch(c.pat, c.path); got != c.want {
			t.Errorf("SensitiveMatch(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}
