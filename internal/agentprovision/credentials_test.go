package agentprovision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// ownerCreds is a CredentialResolver over fixed per-owner maps.
type ownerCreds struct {
	claude map[int64]map[string]string
	or     map[int64]string
	asked  []string
}

func (c *ownerCreds) ClaudeToken(_ context.Context, owner int64, account string) (string, error) {
	c.asked = append(c.asked, fmt.Sprintf("claude:%d:%s", owner, account))
	if tok, ok := c.claude[owner][account]; ok {
		return tok, nil
	}
	return "", errors.New("no such account")
}

func (c *ownerCreds) OpenRouterKey(_ context.Context, owner int64) (string, error) {
	c.asked = append(c.asked, fmt.Sprintf("openrouter:%d", owner))
	if k, ok := c.or[owner]; ok {
		return k, nil
	}
	return "", errors.New("no credential: add your OpenRouter key")
}

const (
	envTok    = "sk-ant-oat01-FAKE-provision-ENV-0000"
	memberTok = "sk-ant-oat01-FAKE-provision-MEMBER-1"
	envKey    = "sk-or-v1-FAKE-provision-ENV-key-2222"
	memberKey = "sk-or-v1-FAKE-provision-MEMBER-key-3"
)

func secretsOf(spec provision.Spec) string {
	var b strings.Builder
	for _, e := range spec.Secrets {
		b.WriteString(e.Name + "=" + e.Value + "\n")
	}
	for _, e := range spec.Env {
		b.WriteString(e.Name + "=" + e.Value + "\n")
	}
	return b.String()
}

// TestASpecIsBuiltFromItsOwnersCredentialsOnly: the deployment-wide config
// carries the ENVIRONMENT's token and key (the admin's). A member's claude-code
// and gateway agents are built with the member's own — and no spec of the
// member's carries an environment value; a member with no credential is
// refused, never built from the environment. The admin's agent gets the
// environment's (the control).
func TestASpecIsBuiltFromItsOwnersCredentialsOnly(t *testing.T) {
	creds := &ownerCreds{
		claude: map[int64]map[string]string{1: {"work": envTok}, 41: {"work": memberTok}},
		or:     map[int64]string{1: envKey, 41: memberKey},
	}
	cfg := fixtureSpecConfig()
	cfg.OpenRouterAPIKey = envKey
	cfg.ClaudeCode = &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:1", Accounts: map[string]string{"work": envTok, "spare": envTok + "-spare"}}
	a, err := New(Config{Driver: provision.MustNewNoop(), Store: &recordingStore{}, Spec: cfg, Credentials: creds, KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	row := func(owner int64, kind string) agents.Agent {
		r := fixtureAgent()
		r.OwnerID, r.Kind, r.HooksToken = owner, kind, "fixture-hooks-owner"
		if kind == agents.KindClaudeCode {
			r.CCAccount = "work"
		}
		return r
	}
	build := func(owner int64, kind string) (string, error) {
		spec, err := a.buildSpec(ctx, row(owner, kind))
		return secretsOf(spec), err
	}

	for _, kind := range []string{agents.KindClaudeCode, agents.KindGateway} {
		got, err := build(41, kind)
		if err != nil {
			t.Fatalf("member %s: %v", kind, err)
		}
		if strings.Contains(got, envTok) || strings.Contains(got, envKey) {
			t.Fatalf("a MEMBER's %s spec carries an environment credential:\n%s", kind, got)
		}
		want := map[string]string{agents.KindClaudeCode: agentspec.EnvClaudeOAuthToken + "=" + memberTok,
			agents.KindGateway: agentspec.EnvOpenRouterKey + "=" + memberKey}[kind]
		if !strings.Contains(got, want) {
			t.Fatalf("member %s spec lacks its own credential (%s):\n%s", kind, want, got)
		}
	}
	// No credential: refused, and never the environment's.
	for _, kind := range []string{agents.KindClaudeCode, agents.KindGateway} {
		got, err := build(42, kind)
		if err == nil {
			t.Fatalf("member 42 (no credential) %s: built a spec:\n%s", kind, got)
		}
		if strings.Contains(err.Error(), envTok) || strings.Contains(err.Error(), envKey) {
			t.Fatalf("the refusal carries a credential: %v", err)
		}
	}
	// The control: the admin's agents get the environment's.
	if got, err := build(1, agents.KindClaudeCode); err != nil || !strings.Contains(got, envTok) {
		t.Fatalf("admin claude-code: %v\n%s", err, got)
	}
	if got, err := build(1, agents.KindGateway); err != nil || !strings.Contains(got, envKey) {
		t.Fatalf("admin gateway: %v\n%s", err, got)
	}
	// The CONFIG a member's spec is built from carries none of the
	// environment's credentials either — replaced, not merged — so a kind that
	// read both (or another account name) still could not reach the admin's.
	for _, kind := range []string{agents.KindClaudeCode, agents.KindGateway} {
		c, err := a.specFor(ctx, row(41, kind))
		if err != nil {
			t.Fatal(err)
		}
		dump := fmt.Sprintf("%v %q", c.ClaudeCode.Accounts, c.OpenRouterAPIKey)
		if strings.Contains(dump, envTok) || strings.Contains(dump, envKey) {
			t.Fatalf("member %s spec config carries an environment credential: %s", kind, dump)
		}
	}
	if c, _ := a.specFor(ctx, row(41, agents.KindClaudeCode)); len(c.ClaudeCode.Accounts) != 1 || c.OpenRouterAPIKey != "" {
		t.Fatalf("member claude-code config: accounts %d, openrouter key set %v", len(c.ClaudeCode.Accounts), c.OpenRouterAPIKey != "")
	}
}
