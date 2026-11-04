package agentspec

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// The LLM route's spec half and its seam with the entrypoint's credential
// check (images/claude-code-agent/credentials.sh). The check's own positive
// and negative cases are cmd/ccd's llmroute_credentials_test.go; this file
// runs the same script on the environment Build produces.

const (
	llmConnectURL = "http://muster.example.test:18191"
	llmHookToken  = "fixture-llm-route-hook-token-5d2a"
)

// runCredCheck sources the image's credential check in a bash with EXACTLY
// env (plus PATH) and returns its exit status, ANTHROPIC_BASE_URL and
// ANTHROPIC_AUTH_TOKEN as it left them ("<unset>" when unset), and stderr.
func runCredCheck(t *testing.T, env map[string]string) (int, string, string, string) {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "images", "claude-code-agent", "credentials.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the entrypoint's credential check is missing: %v", err)
	}
	script := `source "$1" || exit 99; cc_check_credentials || exit $?; printf '%s\n%s\n' "${ANTHROPIC_BASE_URL-<unset>}" "${ANTHROPIC_AUTH_TOKEN-<unset>}"`
	cmd := exec.Command("bash", "-c", script, "credcheck", p)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	rc := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("bash: %v", err)
		}
		rc = ee.ExitCode()
	}
	if rc == 99 {
		t.Fatalf("could not source the credential check: %s", errb.String())
	}
	base, tok, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
	return rc, base, tok, errb.String()
}

// TestARoutineLLMInstancesPodReachesOnlyTheProxy is the seam: the spec Build
// makes for a session with LLMRoute — its plain environment and its secrets,
// exactly as a pod would get them — passes the entrypoint's check, which then
// points Claude Code at the proxy with the pod's own token. The spec carries
// the route, no subscription token, no model and no ANTHROPIC_* of its own.
// Control: the same agent without the route carries its subscription token
// and no route, and its environment with the route bolted on is refused.
func TestARoutineLLMInstancesPodReachesOnlyTheProxy(t *testing.T) {
	cfg := ccConfig()
	cfg.ConnectURL, cfg.ConnectPort = llmConnectURL, 18191
	a := ccAgent()
	a.HooksToken = llmHookToken
	spec := mustBuild(t, a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{LLMRoute: true, BypassPermissions: true, AllowMCPTools: []string{"officer_claim"}}})
	if v, ok := envValue(spec, "CCD_LLM_ROUTE"); !ok || v != "muster" {
		t.Fatalf("CCD_LLM_ROUTE = %q (set %v), want muster", v, ok)
	}
	if _, ok := secretValue(spec, EnvClaudeOAuthToken); ok {
		t.Fatal("a routed pod carries a Claude subscription token")
	}
	for _, e := range append(append([]provision.EnvVar{}, spec.Env...), spec.Secrets...) {
		if strings.HasPrefix(e.Name, "ANTHROPIC_") || e.Name == "CLAUDE_CODE_USE_BEDROCK" || e.Name == "CLAUDE_CODE_USE_VERTEX" {
			t.Errorf("the spec sets %s itself; only the entrypoint's check may", e.Name)
		}
	}
	podEnv := func(s provision.Spec) map[string]string {
		m := map[string]string{}
		for _, e := range append(append([]provision.EnvVar{}, s.Env...), s.Secrets...) {
			m[e.Name] = e.Value
		}
		return m
	}
	if rc, base, tok, stderr := runCredCheck(t, podEnv(spec)); rc != 0 || base != llmConnectURL+"/llm" || tok != llmHookToken {
		t.Fatalf("the routed spec's own environment: rc %d, base %q, token matches %v, stderr %s", rc, base, tok == llmHookToken, stderr)
	}

	plain := mustBuild(t, a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{BypassPermissions: true}})
	if _, ok := envValue(plain, "CCD_LLM_ROUTE"); ok {
		t.Fatal("control: a pod without the route is told CCD_LLM_ROUTE")
	}
	if tok, _ := secretValue(plain, EnvClaudeOAuthToken); tok != ccTokenBeta {
		t.Fatal("control: a pod without the route lost its subscription token")
	}
	env := podEnv(plain)
	env["CCD_LLM_ROUTE"] = "muster"
	if rc, _, _, stderr := runCredCheck(t, env); rc != 1 || !strings.Contains(stderr, EnvClaudeOAuthToken) {
		t.Fatalf("control: a subscription pod with the route bolted on: rc %d, %s; want refused", rc, stderr)
	}
}

// TestTheLLMRouteNeedsTheConnectorDoorAndNoModel: Build refuses a routed
// session with no connect URL (there is no proxy to route to) and one that
// names a model (the proxy pins the model; the pod spec would otherwise carry
// a second, ignored source).
func TestTheLLMRouteNeedsTheConnectorDoorAndNoModel(t *testing.T) {
	a := ccAgent()
	a.HooksToken = llmHookToken
	if _, err := Build(a, ccConfig(), Options{ClaudeCodeSession: ClaudeCodeSession{LLMRoute: true}}); err == nil || !strings.Contains(err.Error(), "ConnectURL") {
		t.Fatalf("routed with no connect URL: err = %v", err)
	}
	cfg := ccConfig()
	cfg.ConnectURL, cfg.ConnectPort = llmConnectURL, 18191
	if _, err := Build(a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{LLMRoute: true, Model: "haiku"}}); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("routed with a model: err = %v", err)
	}
}
