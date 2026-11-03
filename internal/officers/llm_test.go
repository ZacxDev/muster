package officers_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

const llmTestKey = "sk-or-v1-officer-own-key-7a6b5c4d3e2f1a0b"

func llmDecl(llm string) string {
	return `[{"id":"triage:example-web","hq_url":"https://hq.example.com/api/v1","max_instances":2,"llm":` + llm + `}]`
}

// TestParseReadsTheLLMBlockAndItsOwnKey: the block is read, the routine
// agents are canonical, the officer's OWN key wins over the shared one, and
// neither JSON nor fmt prints it.
func TestParseReadsTheLLMBlockAndItsOwnKey(t *testing.T) {
	e := env(map[string]string{
		"MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB":          "hq-tok",
		"MUSTER_OFFICER_OPENROUTER_KEY_TRIAGE_EXAMPLE_WEB": llmTestKey,
		officers.LLMSharedKeyEnv:                           "sk-or-v1-shared-admin-key-should-lose",
	})
	list, err := officers.Parse(llmDecl(`{"model":"deepseek/deepseek-v4.1-flash-1017","daily_budget_usd":5,
		"routine_agents":["officer-triage-example-web-2","officer-triage-example-web-1","officer-triage-example-web-2"]}`), e, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	l := list[0].LLM
	if l == nil || l.Model != "deepseek/deepseek-v4.1-flash-1017" || l.DailyBudgetUSD != 5 || l.MaxTokensPerItem != officers.DefaultLLMMaxTokensPerItem ||
		strings.Join(l.RoutineAgents, ",") != "officer-triage-example-web-1,officer-triage-example-web-2" {
		t.Fatalf("llm = %+v", l)
	}
	if l.Key() != llmTestKey || l.KeySource != "MUSTER_OFFICER_OPENROUTER_KEY_TRIAGE_EXAMPLE_WEB" {
		t.Fatalf("key from %s, want the officer's own", l.KeySource)
	}
	b, _ := json.Marshal(list[0])
	lb, _ := json.Marshal(l)
	for _, s := range []string{string(b), string(lb), fmt.Sprintf("%v %+v %#v %s", l, l, l, l), fmt.Sprintf("%v %+v %#v", list[0], list[0], list[0])} {
		if strings.Contains(s, llmTestKey) {
			t.Fatalf("the key is printed: %s", s)
		}
	}
	if !officers.RoutineAgent(list[0], "officer-triage-example-web-1") || officers.RoutineAgent(list[0], "officer-triage-example-web-3") ||
		officers.RoutineAgent(list[0], "brave-otter") {
		t.Fatal("RoutineAgent disagrees with routine_agents")
	}
	noLLM := list[0]
	noLLM.LLM = nil
	if officers.RoutineAgent(noLLM, "officer-triage-example-web-1") {
		t.Fatal("an officer without llm admits an agent")
	}
}

// TestParseFallsBackToTheSharedKey, and names it as the source.
func TestParseFallsBackToTheSharedKey(t *testing.T) {
	list, err := officers.Parse(llmDecl(`{"model":"vendor/model","daily_budget_usd":1,"max_tokens_per_item":5000,"routine_agents":["officer-triage-example-web-1"]}`),
		env(map[string]string{"MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB": "hq-tok", officers.LLMSharedKeyEnv: "sk-shared"}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if l := list[0].LLM; l.Key() != "sk-shared" || l.KeySource != officers.LLMSharedKeyEnv || l.MaxTokensPerItem != 5000 {
		t.Fatalf("llm = %+v from %s", l, l.KeySource)
	}
}

// TestParseRefusesABadLLMBlock: each refusal names its reason.
func TestParseRefusesABadLLMBlock(t *testing.T) {
	withKey := env(map[string]string{"MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB": "hq-tok", officers.LLMSharedKeyEnv: "sk-shared"})
	noKey := env(map[string]string{"MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB": "hq-tok"})
	agent := `"routine_agents":["officer-triage-example-web-1"]`
	for _, c := range []struct {
		llm  string
		e    func(string) string
		want string
	}{
		{`{"model":"opus","daily_budget_usd":1,` + agent + `}`, withKey, "not a provider model id"},
		{`{"model":"vendor/model",` + agent + `}`, withKey, "daily_budget_usd is required"},
		{`{"model":"vendor/model","daily_budget_usd":0,` + agent + `}`, withKey, "not in (0, 1000]"},
		{`{"model":"vendor/model","daily_budget_usd":1001,` + agent + `}`, withKey, "not in (0, 1000]"},
		{`{"model":"vendor/model","daily_budget_usd":1,"max_tokens_per_item":10,` + agent + `}`, withKey, "max_tokens_per_item 10"},
		{`{"model":"vendor/model","daily_budget_usd":1}`, withKey, "routine_agents is empty"},
		{`{"model":"vendor/model","daily_budget_usd":1,"routine_agents":["officer-triage-example-web-3"]}`, withKey, "not an instance of triage:example-web"},
		{`{"model":"vendor/model","daily_budget_usd":1,"routine_agents":["officer-release-other-1"]}`, withKey, "not an instance"},
		{`{"model":"vendor/model","daily_budget_usd":1,"tier":"routine",` + agent + `}`, withKey, "unknown field"},
		{`{"model":"vendor/model","daily_budget_usd":1,` + agent + `}`, noKey, "no provider key"},
	} {
		_, err := officers.Parse(llmDecl(c.llm), c.e, "muster-agent-")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to say %q", c.llm, err, c.want)
		}
	}
}
