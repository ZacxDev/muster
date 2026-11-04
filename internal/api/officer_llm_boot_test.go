package api

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/officers"
)

// nopLLMStore is an llmproxy.Store that holds nothing.
type nopLLMStore struct{}

func (nopLLMStore) SpentToday(context.Context, string, time.Time) (float64, error) { return 0, nil }
func (nopLLMStore) Book(context.Context, llmproxy.Usage) error                     { return nil }

// TestEnableOfficersRefusesLLMWithoutAConnectorStore: the proxy's door is
// requireConnectorAgent, which answers 503 to every request on a server with
// no connector store, and the proxy's audit rows go to that store. An `llm`
// officer there would be accepted and then fail every request, so
// EnableOfficers refuses it, naming the officer. (The enabled case, with a
// connector store, is officer_llm_pg_test.go's llmOfficerServer.)
func TestEnableOfficersRefusesLLMWithoutAConnectorStore(t *testing.T) {
	list, err := officers.Parse(`[{"id":"triage:example-web","hq_url":"http://hq.invalid/api/v1",`+
		`"tiers":{"routine":{"model":"vendor/pinned-model","max_instances":1},"judgement":{"model":"opus","max_instances":1}},`+
		`"llm":{"daily_budget_usd":5}}]`, func(k string) string {
		if strings.HasPrefix(k, officers.LLMKeyEnvPrefix) || strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "fixture-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	_, err = s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, LLMStore: nopLLMStore{}})
	if err == nil || !strings.Contains(err.Error(), "triage:example-web") || !strings.Contains(err.Error(), "connector store") {
		t.Fatalf("an llm officer with no connector store: err = %v, want a refusal naming the officer and the connector store", err)
	}
}
