package api

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/provision"
)

// noAccountGW is a gateway that takes the no-account rule
// (agentgateway.Gateway.UseNoAccount).
type noAccountGW struct {
	stubGateway
	rule func(agents.Agent) bool
}

func (g *noAccountGW) UseNoAccount(f func(agents.Agent) bool) { g.rule = f }

const (
	poolKeySecurity = "sk-ant-api03-fixture-security-judgement-key-AAAAAAAAAAAAAAAAAAAAAAAA"
	poolKeyEnv      = "MUSTER_OFFICER_ANTHROPIC_KEY_SECURITY_EXAMPLE_JUDGEMENT"
	poolSetupToken  = "sk-ant-oat01-fixture-shared-account-setup-token-ZZZZZZZZZZZZZZZZZZZZ"
)

// TestAPoolCredentialReachesOnlyThatPoolsPods is tiers.<pool>.credential at its
// seam, from the configuration: MUSTER_OFFICERS (officers.Parse) → the
// registry → the provisioner's session source (OfficerClaudeCodeSession) →
// agentspec.Build → the pod's Secret. With the shared account's setup-token
// configured for every agent:
//
//   - the security officer's JUDGEMENT pod carries exactly its own key, and no
//     setup-token;
//   - its ROUTINE pod (no credential) and another officer's pods carry the
//     account's setup-token and no key — other officers are unaffected;
//   - the agent gateway is given the no-account rule, which answers true for
//     the judgement instance only, so a spent or revoked key marks and moves
//     no shared account.
func TestAPoolCredentialReachesOnlyThatPoolsPods(t *testing.T) {
	list, err := officers.Parse(`[{"id":"security:example","hq_url":"http://hq.invalid/api/v1",`+
		`"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus","credential":{"secret_env":"`+poolKeyEnv+`"}}}},`+
		`{"id":"triage:example","hq_url":"http://hq.invalid/api/v1","tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}}]`,
		func(k string) string {
			switch {
			case k == poolKeyEnv:
				return poolKeySecurity
			case strings.HasPrefix(k, officers.TokenEnvPrefix):
				return "fixture-" + k
			}
			return ""
		}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	gw := &noAccountGW{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{},
		Kinds: stubKinds{pick: "acct-one"}, Connectors: presenceStore{}, Gateway: gw})
	var installed func(agents.Agent) (agentspec.ClaudeCodeSession, bool)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list,
		InstallSession: func(f func(agents.Agent) (agentspec.ClaudeCodeSession, bool)) { installed = f }}); err != nil {
		t.Fatal(err)
	}
	if gw.rule == nil {
		t.Fatal("the gateway was not given the no-account rule")
	}
	cfg := agentspec.Config{APIBaseURL: "http://muster.example.test", ClaudeCode: &agentspec.ClaudeCodeConfig{
		Image: "ghcr.io/example-org/claude-code-agent:0a1b2c3", Accounts: map[string]string{"acct-one": poolSetupToken}}}
	for _, c := range []struct {
		name    string
		wantKey bool
	}{
		{"officer-security-example-1", true},
		{"officer-security-example-r1", false},
		{"officer-triage-example-1", false},
		{"officer-triage-example-r1", false},
	} {
		a := agents.Agent{ID: 5, Name: c.name, Kind: agents.KindClaudeCode, CCAccount: "acct-one", HooksToken: "fixture-hooks",
			GatewaySecret: "fixture-turn-credential", OwnerID: 1}
		sess, ok := installed(a)
		if !ok {
			t.Fatalf("%s: no officer session", c.name)
		}
		spec, err := agentspec.Build(a, cfg, agentspec.Options{ClaudeCodeSession: sess})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		secrets := map[string]string{}
		for _, e := range spec.Secrets {
			secrets[e.Name] = e.Value
		}
		key, hasKey := secrets[agentspec.EnvAnthropicAPIKey]
		tok, hasTok := secrets[agentspec.EnvClaudeOAuthToken]
		if c.wantKey && (!hasKey || key != poolKeySecurity || hasTok) {
			t.Errorf("%s: key %v (ours %v), setup-token %v; want only its own key", c.name, hasKey, key == poolKeySecurity, hasTok)
		}
		if !c.wantKey && (hasKey || !hasTok || tok != poolSetupToken) {
			t.Errorf("%s: key %v, setup-token %v; want the account's setup-token and no key", c.name, hasKey, hasTok)
		}
		for _, e := range append(append([]provision.EnvVar{}, spec.Env...), provision.EnvVar{Name: "session", Value: sessString(sess)}) {
			if strings.Contains(e.Value, poolKeySecurity[:20]) {
				t.Errorf("%s: %s carries the key outside the Secret", c.name, e.Name)
			}
		}
		if got := gw.rule(a); got != c.wantKey {
			t.Errorf("%s: no-account rule %v, want %v", c.name, got, c.wantKey)
		}
		// The account's usage and attribution follow the same rule: a pool on
		// its own key is credited to no shared account.
		if _, _, onNone, _ := s.officerPoolOf(c.name); onNone != c.wantKey {
			t.Errorf("%s: attribution's no-account answer %v, want %v", c.name, onNone, c.wantKey)
		}
		p, _, _ := s.officerRT.reg.ForAgentName(c.name)
		if got := instanceUsageAccount(p, a); (got == "") != c.wantKey {
			t.Errorf("%s: usage reported on account %q, want none only for the pool on its own key", c.name, got)
		}
	}
	if gw.rule(agents.Agent{Name: "brave-otter", CCAccount: "acct-one"}) {
		t.Error("an ordinary agent is on no account")
	}
}

func sessString(s agentspec.ClaudeCodeSession) string {
	var b bytes.Buffer
	log.New(&b, "", 0).Printf("%v %+v %#v", s, s, s)
	return b.String()
}
