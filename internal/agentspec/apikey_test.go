package agentspec

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// The API-key route's spec half (ClaudeCodeSession.APIKey) and its seam with
// the entrypoint's credential check: a pod of an officer pool with its own
// credential carries that key and NO other model credential.

const poolAPIKey = "sk-ant-api03-fixture-pool-own-key-not-real-0123456789abcdefghij"

// podEnvOf is the pod's whole environment: its plain variables and its
// secrets, as the container gets them.
func podEnvOf(s provision.Spec) map[string]string {
	m := map[string]string{}
	for _, e := range append(append([]provision.EnvVar{}, s.Env...), s.Secrets...) {
		m[e.Name] = e.Value
	}
	return m
}

// TestAPoolKeyPodCarriesOnlyItsKey: Build for a session with APIKey puts the
// key in the agent's Secret under ANTHROPIC_API_KEY and nowhere else, carries
// NO subscription token although the agent's account has one configured,
// tells the entrypoint CCD_LLM_ROUTE=api_key, and its environment passes the
// entrypoint's check. Control: the same agent without APIKey carries its
// account's token and no key, and its environment with the key bolted on is
// refused.
func TestAPoolKeyPodCarriesOnlyItsKey(t *testing.T) {
	cfg := ccConfig()
	a := ccAgent()
	spec := mustBuild(t, a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{APIKey: &ClaudeCodeAPIKey{Value: poolAPIKey},
		BypassPermissions: true, Model: "opus"}})
	if got, ok := secretValue(spec, EnvAnthropicAPIKey); !ok || got != poolAPIKey {
		t.Fatalf("the pod's Secret has %s = %q (set %v), want the pool's key", EnvAnthropicAPIKey, got, ok)
	}
	if _, ok := secretValue(spec, EnvClaudeOAuthToken); ok {
		t.Fatal("a pool-key pod carries the account's subscription token")
	}
	names := map[string]bool{}
	for _, e := range spec.Secrets {
		names[e.Name] = true
	}
	if len(names) != 3 || !names[EnvToken] || !names[EnvGatewayBearer] || !names[EnvAnthropicAPIKey] {
		t.Fatalf("the pod's secrets are %v, want exactly its muster token, its turn credential and the key", names)
	}
	for _, e := range spec.Env {
		if strings.Contains(e.Value, poolAPIKey[:20]) || e.Name == EnvAnthropicAPIKey || e.Name == EnvClaudeOAuthToken {
			t.Errorf("the plain environment carries the credential: %s", e.Name)
		}
	}
	if v, ok := envValue(spec, EnvLLMRoute); !ok || v != LLMRouteAPIKey {
		t.Fatalf("%s = %q (set %v), want %s", EnvLLMRoute, v, ok, LLMRouteAPIKey)
	}
	if rc, base, tok, stderr := runCredCheck(t, podEnvOf(spec)); rc != 0 || base != "<unset>" || tok != "<unset>" {
		t.Fatalf("the pool-key spec's own environment: rc %d, base %q, token %q, stderr %s", rc, base, tok, stderr)
	}

	plain := mustBuild(t, a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{BypassPermissions: true}})
	if tok, _ := secretValue(plain, EnvClaudeOAuthToken); tok != ccTokenBeta {
		t.Fatal("control: a pod without a pool key lost its subscription token")
	}
	if _, ok := secretValue(plain, EnvAnthropicAPIKey); ok {
		t.Fatal("control: a pod without a pool key carries an API key")
	}
	env := podEnvOf(plain)
	env[EnvAnthropicAPIKey], env[EnvLLMRoute] = poolAPIKey, LLMRouteAPIKey
	if rc, _, _, stderr := runCredCheck(t, env); rc != 1 || !strings.Contains(stderr, EnvClaudeOAuthToken) {
		t.Fatalf("control: a subscription pod with the key bolted on: rc %d, %s; want refused naming the token", rc, stderr)
	}
}

// TestAPoolKeyPodFailsClosed: an empty key is a refusal, never a pod on the
// account's token; a key beside the LLM route is refused (one model
// credential); and an agent whose account has no token still builds on its
// key (it needs none), while the same agent without the key is refused.
func TestAPoolKeyPodFailsClosed(t *testing.T) {
	cfg := ccConfig()
	cfg.ConnectURL, cfg.ConnectPort = llmConnectURL, 18191
	a := ccAgent()
	for _, v := range []string{"", "   "} {
		if _, err := Build(a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{APIKey: &ClaudeCodeAPIKey{Value: v}}}); err == nil ||
			!strings.Contains(err.Error(), "never falls back to the shared account pool") {
			t.Errorf("an empty key %q: %v, want a refusal", v, err)
		}
	}
	if _, err := Build(a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{APIKey: &ClaudeCodeAPIKey{Value: poolAPIKey}, LLMRoute: true}}); err == nil ||
		!strings.Contains(err.Error(), "one model credential") {
		t.Errorf("a key beside the LLM route: %v, want a refusal", err)
	}
	a.CCAccount = "unconfigured"
	if _, err := Build(a, cfg, Options{ClaudeCodeSession: ClaudeCodeSession{APIKey: &ClaudeCodeAPIKey{Value: poolAPIKey}}}); err != nil {
		t.Errorf("a pool-key pod on an account with no token: %v, want it built", err)
	}
	if _, err := Build(a, cfg, Options{}); err == nil {
		t.Error("control: the same agent without the key built with no account token")
	}
}

// TestAPoolKeyIsNeverPrinted: a session holding the key prints without it.
func TestAPoolKeyIsNeverPrinted(t *testing.T) {
	sess := ClaudeCodeSession{APIKey: &ClaudeCodeAPIKey{Value: poolAPIKey}, Model: "opus"}
	if s := fmt.Sprintf("%v %+v %#v %s", sess, sess, sess, sess.APIKey); strings.Contains(s, poolAPIKey[:20]) {
		t.Fatalf("printed the key: %s", s)
	}
}
