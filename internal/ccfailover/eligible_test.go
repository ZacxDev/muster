package ccfailover

import (
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestEligibleIsAClaudeCodeAgentOnANamedAccount pins the one predicate the
// failover and the officer chat's reset reply (internal/api withPoolReset)
// share: only a claude-code agent on a named account can be moved.
func TestEligibleIsAClaudeCodeAgentOnANamedAccount(t *testing.T) {
	for _, c := range []struct {
		name string
		ag   agents.Agent
		want bool
	}{
		{"claude-code on an account", agents.Agent{Kind: agents.KindClaudeCode, CCAccount: "acct-one"}, true},
		{"claude-code on no account", agents.Agent{Kind: agents.KindClaudeCode}, false},
		{"another kind", agents.Agent{Kind: agents.KindGateway}, false},
		{"another kind naming an account", agents.Agent{Kind: agents.KindGateway, CCAccount: "acct-one"}, false},
	} {
		if got := Eligible(c.ag); got != c.want {
			t.Errorf("%s: Eligible = %v; want %v", c.name, got, c.want)
		}
	}
}
