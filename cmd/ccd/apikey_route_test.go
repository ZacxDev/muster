package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
)

// THE API-KEY ROUTE (CCD_LLM_ROUTE=api_key): an instance of an officer pool
// with its own credential runs on that pool's Anthropic API key and nothing
// else. The entrypoint's check (images/claude-code-agent/credentials.sh) and
// ccd seed's approval of the key, which the interactive CLI would otherwise
// stop on.

const fixtureAPIKey = "sk-ant-api03-fixture-pool-own-key-not-real-0123456789abcdefghij"

// keyed is the environment a pool-key pod gives the check.
func keyed(extra map[string]string) map[string]string {
	env := map[string]string{"CCD_LLM_ROUTE": "api_key", "ANTHROPIC_API_KEY": fixtureAPIKey,
		"MUSTER_CONNECT_URL": llmConnectURL, "MUSTER_HOOK_TOKEN": llmHookToken}
	for k, v := range extra {
		if v == "<unset>" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	return env
}

// TestTheAPIKeyRouteAdmitsOnlyThePoolsOwnKey: under CCD_LLM_ROUTE=api_key the
// check passes a pod holding just its key and exports nothing; it refuses a
// pod without the key (fail closed: there is no other credential to fall back
// to), and one holding the key beside a subscription token, an auth token, a
// base URL, Bedrock or Vertex — each naming the variable, never printing the
// key. Control: without the route the same key is refused as before.
func TestTheAPIKeyRouteAdmitsOnlyThePoolsOwnKey(t *testing.T) {
	r := runCredCheck(t, keyed(nil))
	if r.rc != 0 {
		t.Fatalf("a pool-key pod was refused (%d): %s", r.rc, r.stderr)
	}
	for _, v := range exported {
		if r.vars[v] != "<unset>" {
			t.Errorf("the API-key route exported %s=%q", v, r.vars[v])
		}
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		says string
	}{
		{"no key", map[string]string{"ANTHROPIC_API_KEY": "<unset>"}, "ANTHROPIC_API_KEY is not set"},
		{"an empty key", map[string]string{"ANTHROPIC_API_KEY": ""}, "ANTHROPIC_API_KEY is not set"},
		{"a subscription token beside the key", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-fixture-not-real"}, "CLAUDE_CODE_OAUTH_TOKEN"},
		{"an auth token beside the key", map[string]string{"ANTHROPIC_AUTH_TOKEN": "x"}, "ANTHROPIC_AUTH_TOKEN"},
		{"a base URL beside the key", map[string]string{"ANTHROPIC_BASE_URL": "https://api.example.test"}, "ANTHROPIC_BASE_URL"},
		{"Bedrock beside the key", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, "CLAUDE_CODE_USE_BEDROCK"},
		{"Vertex beside the key", map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}, "CLAUDE_CODE_USE_VERTEX"},
	} {
		r := runCredCheck(t, keyed(c.env))
		if r.rc != 1 || !strings.Contains(r.stderr, c.says) || !strings.Contains(r.stderr, "Refusing.") {
			t.Errorf("%s: rc %d, stderr %q; want 1 naming %s", c.name, r.rc, r.stderr, c.says)
		}
		if strings.Contains(r.stderr, fixtureAPIKey[:20]) {
			t.Errorf("%s: the refusal printed the key", c.name)
		}
	}
	if r := runCredCheck(t, map[string]string{"ANTHROPIC_API_KEY": fixtureAPIKey}); r.rc != 1 || !strings.Contains(r.stderr, "ANTHROPIC_API_KEY is set") {
		t.Errorf("control: the key without the route: rc %d, %q; want refused", r.rc, r.stderr)
	}
}

// TestSeedApprovesThePoolKeyOnlyOnTheAPIKeyRoute: on the route, seed records
// the key's last 20 characters as approved in .claude.json (the CLI's own
// customApiKeyResponses rule) and drops a rejection of it, keeping other
// entries; it never writes the whole key. Control: a pod holding
// ANTHROPIC_API_KEY without the route approves nothing.
func TestSeedApprovesThePoolKeyOnlyOnTheAPIKeyRoute(t *testing.T) {
	suffix := fixtureAPIKey[len(fixtureAPIKey)-20:]
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	prior := map[string]any{"customApiKeyResponses": map[string]any{"approved": []any{"someone-elses-suffix"}, "rejected": []any{suffix, "kept-rejection"}}}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCD_LLM_ROUTE", "api_key")
	t.Setenv("ANTHROPIC_API_KEY", fixtureAPIKey)
	var out, errb bytes.Buffer
	if rc := runSeed([]string{"--config-dir", cfg, "--workspace", ws, "--settings-template", imageSettingsTemplate}, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	raw, err := os.ReadFile(filepath.Join(cfg, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fixtureAPIKey[:24]) {
		t.Fatal(".claude.json carries more of the key than its 20-character suffix")
	}
	resp, _ := readObj(t, filepath.Join(cfg, ".claude.json"))["customApiKeyResponses"].(map[string]any)
	gotA, _ := json.Marshal(resp["approved"])
	gotR, _ := json.Marshal(resp["rejected"])
	if string(gotA) != `["someone-elses-suffix","`+suffix+`"]` || string(gotR) != `["kept-rejection"]` {
		t.Fatalf("customApiKeyResponses = approved %s, rejected %s", gotA, gotR)
	}

	cfg2 := t.TempDir()
	t.Setenv("CCD_LLM_ROUTE", "")
	if rc := runSeed([]string{"--config-dir", cfg2, "--workspace", ws, "--settings-template", imageSettingsTemplate}, &out, &errb); rc != 0 {
		t.Fatalf("control rc %d: %s", rc, errb.String())
	}
	if _, has := readObj(t, filepath.Join(cfg2, ".claude.json"))["customApiKeyResponses"]; has {
		t.Fatal("control: seed approved a key without the API-key route")
	}
}

// TestCcdReadsTheAPIKeyRouteAgentspecWrites pins the spellings on both sides.
func TestCcdReadsTheAPIKeyRouteAgentspecWrites(t *testing.T) {
	if envLLMRoute != agentspec.EnvLLMRoute || llmRouteAPIKey != agentspec.LLMRouteAPIKey || envAnthropicAPIKey != agentspec.EnvAnthropicAPIKey {
		t.Fatalf("ccd reads %s=%s and %s; agentspec writes %s=%s and %s", envLLMRoute, llmRouteAPIKey, envAnthropicAPIKey,
			agentspec.EnvLLMRoute, agentspec.LLMRouteAPIKey, agentspec.EnvAnthropicAPIKey)
	}
}
