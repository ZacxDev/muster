package api

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestOnlyARoutineLLMInstanceIsRoutedToTheProxy: the session settings the
// officer runtime hands the provisioner put a ROUTINE instance of an officer
// that declares `llm` on the LLM route (agentspec.ClaudeCodeSession.LLMRoute:
// the pod gets CCD_LLM_ROUTE=muster and no subscription token) with no model —
// the proxy pins tiers.routine.model, and the pod spec refuses a provider id.
// The same officer's judgement instance, an `-r<n>` past the routine max and
// an untiered officer's instance are not routed, and keep their models.
//
// The route is read from the settings' JSON (field LLMRoute), so this test
// compiles on a tree without it and is red there by assertion.
func TestOnlyARoutineLLMInstanceIsRoutedToTheProxy(t *testing.T) {
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
	routed := func(sess agentspec.ClaudeCodeSession) bool {
		var m map[string]any
		b, _ := json.Marshal(sess)
		_ = json.Unmarshal(b, &m)
		v, _ := m["LLMRoute"].(bool)
		return v
	}
	stem := "officer-triage-example-web-"
	for _, c := range []struct {
		name   string
		ok     bool
		routed bool
		model  string
	}{
		{stem + "r1", true, true, ""},
		{stem + "r2", true, true, ""},
		{stem + "1", true, false, "opus"},
		{stem + "r3", false, false, ""}, // past the routine max: not an instance
		{officers.InstanceName(flatOff, 1), true, false, "sonnet"},
	} {
		sess, ok := installed(agents.Agent{Name: c.name, Kind: agents.KindClaudeCode})
		if ok != c.ok || routed(sess) != c.routed || sess.Model != c.model {
			t.Errorf("%s: session %+v (ok %v, routed %v), want ok %v, routed %v, model %q", c.name, sess, ok, routed(sess), c.ok, c.routed, c.model)
		}
		if c.ok && !sess.BypassPermissions {
			t.Errorf("%s: an officer instance lost its posture: %+v", c.name, sess)
		}
	}
}
