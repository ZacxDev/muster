package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE LLM ROUTE (CCD_LLM_ROUTE=muster): a routine instance of an officer that
// declares `llm` reaches its model through muster's LLM proxy, and nothing
// else. Two halves and the seam between them:
//
//   - the SPEC (Build with ClaudeCodeSession.LLMRoute): the pod is told
//     CCD_LLM_ROUTE=muster and carries no Claude subscription token;
//   - the ENTRYPOINT's credential check (images/claude-code-agent/
//     credentials.sh, sourced by entrypoint.sh): under the route it sets
//     ANTHROPIC_BASE_URL to exactly $MUSTER_CONNECT_URL/llm and
//     ANTHROPIC_AUTH_TOKEN to the pod's own muster token, and refuses every
//     other credential and endpoint; without the route it refuses all of them
//     as before.
//
// The check is run here with bash, on the file the image installs, so each
// condition has a positive and a negative case in the Go suite (the image
// smoke, cc-smoke step 7, runs the installed copy too).
// ---------------------------------------------------------------------------

const (
	llmConnectURL = "http://muster.example.test:18191"
	llmHookToken  = "fixture-llm-route-hook-token-5d2a"
)

// credentialsScript is the credential check the image installs.
func credentialsScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "images", "claude-code-agent", "credentials.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the entrypoint's credential check is missing: %v", err)
	}
	return p
}

// credResult is one run of the check: its exit status, what it exported,
// and its stderr.
type credResult struct {
	rc     int
	vars   map[string]string
	stderr string
}

// exported are the variables the check may set, printed after it ran.
var exported = []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "NO_PROXY", "no_proxy"}

// runCredCheck sources the check in a bash with EXACTLY env (plus PATH) and
// reports what it did. A variable it left unset prints as "<unset>".
func runCredCheck(t *testing.T, env map[string]string) credResult {
	t.Helper()
	script := `source "$1" || exit 99; cc_check_credentials || exit $?; for v in ` + strings.Join(exported, " ") +
		`; do if [[ -v $v ]]; then printf '%s=%s\n' "$v" "${!v}"; else printf '%s=<unset>\n' "$v"; fi; done`
	cmd := exec.Command("bash", "-c", script, "credcheck", credentialsScript(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	res := credResult{vars: map[string]string{}}
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("bash: %v", err)
		}
		res.rc = ee.ExitCode()
	}
	if res.rc == 99 {
		t.Fatalf("could not source the credential check: %s", errb.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			res.vars[k] = v
		}
	}
	res.stderr = errb.String()
	return res
}

// routed is the environment a routine instance's pod gives the check.
func routed(extra map[string]string) map[string]string {
	env := map[string]string{"CCD_LLM_ROUTE": "muster", "MUSTER_CONNECT_URL": llmConnectURL, "MUSTER_HOOK_TOKEN": llmHookToken}
	for k, v := range extra {
		if v == "<unset>" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	return env
}

// TestTheCredentialCheckWithoutTheRouteRefusesEveryOtherCredential: the
// pre-route behaviour, unchanged. A pod without CCD_LLM_ROUTE runs on its
// subscription token only; each of the five variables is refused alone,
// naming it, and the check exports nothing. Control: a pod with just the
// subscription token passes and exports nothing.
// ⚠ Partly an INVARIANT guard (the five refusals held before the route
// existed); the file it runs is new, so the test as a whole is red at base.
func TestTheCredentialCheckWithoutTheRouteRefusesEveryOtherCredential(t *testing.T) {
	ok := runCredCheck(t, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-fixture-not-real"})
	if ok.rc != 0 {
		t.Fatalf("control: a pod with only its subscription token was refused (%d): %s", ok.rc, ok.stderr)
	}
	for _, v := range exported {
		if ok.vars[v] != "<unset>" {
			t.Errorf("control: without the route the check exported %s=%q", v, ok.vars[v])
		}
	}
	for _, v := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_BASE_URL"} {
		r := runCredCheck(t, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-fixture-not-real", v: "fake-value"})
		if r.rc != 1 || !strings.Contains(r.stderr, v+" is set") || !strings.Contains(r.stderr, "Refusing.") {
			t.Errorf("without the route, %s set: rc %d, stderr %q; want 1 naming %s", v, r.rc, r.stderr, v)
		}
	}
}

// TestTheLLMRouteSetsExactlyTheProxyAndThePodsOwnToken: under
// CCD_LLM_ROUTE=muster the check points Claude Code at exactly
// $MUSTER_CONNECT_URL/llm with the pod's own muster token as its bearer, and
// keeps the muster host off any proxy (NO_PROXY / no_proxy). A base URL or
// auth token the pod already carries is refused, naming the variable — even
// one equal to the value the check sets (round 0 D1: nothing produces one).
func TestTheLLMRouteSetsExactlyTheProxyAndThePodsOwnToken(t *testing.T) {
	r := runCredCheck(t, routed(nil))
	if r.rc != 0 {
		t.Fatalf("a routed pod was refused (%d): %s", r.rc, r.stderr)
	}
	if got := r.vars["ANTHROPIC_BASE_URL"]; got != llmConnectURL+"/llm" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want exactly %s/llm", got, llmConnectURL)
	}
	if got := r.vars["ANTHROPIC_AUTH_TOKEN"]; got != llmHookToken {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want the pod's own muster token", got)
	}
	for _, v := range []string{"NO_PROXY", "no_proxy"} {
		if got := r.vars[v]; got != "muster.example.test" {
			t.Errorf("%s = %q, want the muster host", v, got)
		}
	}
	// An existing NO_PROXY is kept, the host appended.
	if r := runCredCheck(t, routed(map[string]string{"NO_PROXY": "10.0.0.0/8"})); r.rc != 0 || r.vars["NO_PROXY"] != "10.0.0.0/8,muster.example.test" {
		t.Errorf("NO_PROXY with a prior value: rc %d, %q", r.rc, r.vars["NO_PROXY"])
	}

	for _, c := range []struct {
		name string
		env  map[string]string
		ok   bool
		says string
	}{
		// Round 0 D1: ANY preset is refused under the route, even the exact
		// value the check would set — nothing produces one.
		{"base URL preset to exactly the proxy", map[string]string{"ANTHROPIC_BASE_URL": llmConnectURL + "/llm"}, false, "ANTHROPIC_BASE_URL"},
		{"base URL preset elsewhere", map[string]string{"ANTHROPIC_BASE_URL": "https://api.example.test"}, false, "ANTHROPIC_BASE_URL"},
		{"base URL preset to the connect URL without /llm", map[string]string{"ANTHROPIC_BASE_URL": llmConnectURL}, false, "ANTHROPIC_BASE_URL"},
		{"auth token preset to the pod's token", map[string]string{"ANTHROPIC_AUTH_TOKEN": llmHookToken}, false, "ANTHROPIC_AUTH_TOKEN"},
		{"auth token preset to another", map[string]string{"ANTHROPIC_AUTH_TOKEN": "someone-elses-token"}, false, "ANTHROPIC_AUTH_TOKEN"},
		{"a subscription token beside the route", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-fixture-not-real"}, false, "CLAUDE_CODE_OAUTH_TOKEN"},
		{"an API key beside the route", map[string]string{"ANTHROPIC_API_KEY": "fake-value"}, false, "ANTHROPIC_API_KEY"},
		{"Bedrock beside the route", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, false, "CLAUDE_CODE_USE_BEDROCK"},
		{"Vertex beside the route", map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}, false, "CLAUDE_CODE_USE_VERTEX"},
		{"no connect URL", map[string]string{"MUSTER_CONNECT_URL": "<unset>"}, false, "MUSTER_CONNECT_URL"},
		{"no muster token", map[string]string{"MUSTER_HOOK_TOKEN": "<unset>"}, false, "MUSTER_HOOK_TOKEN"},
		{"another route", map[string]string{"CCD_LLM_ROUTE": "openrouter"}, false, "CCD_LLM_ROUTE"},
	} {
		r := runCredCheck(t, routed(c.env))
		switch {
		case c.ok && r.rc != 0:
			t.Errorf("%s: refused (%d): %s", c.name, r.rc, r.stderr)
		case c.ok && (r.vars["ANTHROPIC_BASE_URL"] != llmConnectURL+"/llm" || r.vars["ANTHROPIC_AUTH_TOKEN"] != llmHookToken):
			t.Errorf("%s: exported %v", c.name, r.vars)
		case !c.ok && (r.rc != 1 || !strings.Contains(r.stderr, c.says) || !strings.Contains(r.stderr, "Refusing.")):
			t.Errorf("%s: rc %d, stderr %q; want 1 naming %s", c.name, r.rc, r.stderr, c.says)
		}
		if !c.ok && strings.Contains(r.stderr, llmHookToken) {
			t.Errorf("%s: the refusal printed the pod's token", c.name)
		}
	}
}

// TestTheImageInstallsTheCredentialCheckTheEntrypointSources pins the wiring
// the Go suite cannot run: the Dockerfile installs credentials.sh at the path
// entrypoint.sh sources, entrypoint.sh calls the check before it seeds, and
// it no longer carries its own refusal loop (one rule, one place).
func TestTheImageInstallsTheCredentialCheckTheEntrypointSources(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "images", "claude-code-agent", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const installed = "/usr/local/lib/cc/credentials.sh"
	if df := read("Dockerfile"); !strings.Contains(df, "COPY images/claude-code-agent/credentials.sh "+installed) {
		t.Errorf("the Dockerfile does not install credentials.sh at %s", installed)
	}
	ep := read("entrypoint.sh")
	src, call, seed := strings.Index(ep, "source "+installed), strings.Index(ep, "cc_check_credentials || exit 1"), strings.Index(ep, "ccd seed")
	if src < 0 || call < 0 || seed < 0 || !(src < call && call < seed) {
		t.Errorf("entrypoint.sh must source %s and run cc_check_credentials before ccd seed (source %d, call %d, seed %d)", installed, src, call, seed)
	}
	if strings.Contains(ep, "for v in ANTHROPIC_API_KEY") {
		t.Error("entrypoint.sh still carries its own refusal loop beside the check")
	}
}
