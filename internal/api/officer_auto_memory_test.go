package api

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestEveryOfficerInstanceSessionDisablesAutoMemory: the session settings the
// officer runtime hands the provisioner turn the CLI's auto-memory OFF for
// every officer instance — a routine instance on the LLM route, a judgement
// instance, an untiered officer's — whatever fresh_session_per_item says
// (none of these declare it): an instance's memory of earlier items is HQ's to
// give, never a file on its volume. A name that is not an instance (past the
// routine max) and the chief get no officer session at all (⚠ INVARIANT half).
func TestEveryOfficerInstanceSessionDisablesAutoMemory(t *testing.T) {
	const llmOff, flatOff = "triage:example-web", "release:example-api"
	list, err := officers.Parse(`[{"id":"`+llmOff+`","hq_url":"http://hq.invalid/api/v1",`+
		`"tiers":{"routine":{"model":"vendor/pinned-model","max_instances":2},"judgement":{"model":"opus","max_instances":1}},`+
		`"llm":{"daily_budget_usd":5}},{"id":"`+flatOff+`","hq_url":"http://hq.invalid/api/v1","model":"sonnet"}]`, func(k string) string {
		if strings.HasPrefix(k, officers.LLMKeyEnvPrefix) || strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "fixture-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{},
		Kinds: stubKinds{pick: "acct-one"}, Connectors: presenceStore{}})
	var installed func(agents.Agent) (agentspec.ClaudeCodeSession, bool)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, LLMStore: nopLLMStore{},
		InstallSession: func(f func(agents.Agent) (agentspec.ClaudeCodeSession, bool)) { installed = f }}); err != nil {
		t.Fatal(err)
	}
	stem := "officer-triage-example-web-"
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{stem + "r1", true}, // routine, on the LLM route
		{stem + "1", true},  // judgement
		{officers.InstanceName(flatOff, 1), true},
		{stem + "r3", false}, // past the routine max: not an instance
		{agents.ChiefName, false},
	} {
		sess, ok := installed(agents.Agent{Name: c.name, Kind: agents.KindClaudeCode})
		if ok != c.ok || sess.DisableAutoMemory != c.ok {
			t.Errorf("%s: session %+v (ok %v), want ok %v with DisableAutoMemory %v", c.name, sess, ok, c.ok, c.ok)
		}
	}
}
