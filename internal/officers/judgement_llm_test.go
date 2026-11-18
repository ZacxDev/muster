package officers_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// tiers.judgement.llm: the judgement pool on the LLM proxy, with its own
// budget, beside (or without) the routine pool's top-level `llm`.

// judgementLLMTiers: judgement on a provider model with its own block, the
// routine pool on Claude (no top-level llm). Budgets and caps differ from the
// routine fixture's and from the defaults, so a value read from the wrong
// block cannot pass.
const judgementLLMTiers = `"tiers":{"routine":{"model":"haiku","max_instances":2},` +
	`"judgement":{"model":"deepseek/deepseek-v4-pro","max_instances":3,"max_sessions_per_day":30,"max_item_minutes":45,` +
	`"llm":{"daily_budget_usd":7.5,"max_tokens_per_item":900000}}}`

func ownKey() func(string) string {
	return llmEnv(map[string]string{ownKeyEnv: llmTestKey})
}

// TestAnExistingRoutineLLMConfigParsesUnchanged pins what a config written
// before tiers.judgement.llm parses to: the routine block as before, no
// judgement block, and only the routine pool on the proxy.
func TestAnExistingRoutineLLMConfigParsesUnchanged(t *testing.T) {
	list, err := officers.Parse(llmDecl(`,`+llmTiers+`,"llm":{"daily_budget_usd":5,"max_tokens_per_item":300000}`), ownKey(), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if o.LLM == nil || o.LLM.DailyBudgetUSD != 5 || o.LLM.MaxTokensPerItem != 300000 || o.LLM.Key() != llmTestKey {
		t.Fatalf("routine llm = %v", o.LLM)
	}
	if o.Tiers.Judgement.LLM != nil {
		t.Fatalf("judgement llm = %v, want none", o.Tiers.Judgement.LLM)
	}
	if got := o.Pool(officers.TierRoutine).PoolLLM(); got != o.LLM {
		t.Fatalf("routine PoolLLM = %v, want the top-level block", got)
	}
	if got := o.Pool(officers.TierJudgement).PoolLLM(); got != nil {
		t.Fatalf("judgement PoolLLM = %v, want nil", got)
	}
	if _, _, ok := officers.ProxyAgent(o, "officer-triage-example-web-1"); ok {
		t.Fatal("a judgement instance of an officer without tiers.judgement.llm is on the proxy")
	}
	if p, n, ok := officers.ProxyAgent(o, "officer-triage-example-web-r2"); !ok || p.Tier != officers.TierRoutine || n != 2 {
		t.Fatalf("routine r2: pool %q n %d ok %v", p.Tier, n, ok)
	}
}

// TestAJudgementLLMBlockIsAccepted: the judgement pool's model is a provider
// id, its block is its own (budget, cap, the officer's key), the session and
// wall-time caps are kept, and the routine pool stays on Claude.
func TestAJudgementLLMBlockIsAccepted(t *testing.T) {
	list, err := officers.Parse(llmDecl(`,`+judgementLLMTiers), ownKey(), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	j := o.Pool(officers.TierJudgement)
	l := j.PoolLLM()
	if l == nil || l.DailyBudgetUSD != 7.5 || l.MaxTokensPerItem != 900000 || l.Key() != llmTestKey {
		t.Fatalf("judgement llm = %v", l)
	}
	if j.Model != "deepseek/deepseek-v4-pro" || o.Tiers.Judgement.MaxSessionsPerDay != 30 || j.ItemCap().Minutes() != 45 {
		t.Fatalf("judgement pool: model %q sessions %d cap %v", j.Model, o.Tiers.Judgement.MaxSessionsPerDay, j.ItemCap())
	}
	if o.LLM != nil || o.Pool(officers.TierRoutine).PoolLLM() != nil {
		t.Fatal("the routine pool is on the proxy, but only the judgement pool declared a block")
	}
	if p, n, ok := officers.ProxyAgent(o, "officer-triage-example-web-3"); !ok || p.Tier != officers.TierJudgement || n != 3 || p.Model != j.Model {
		t.Fatalf("judgement #3: pool %q n %d ok %v", p.Tier, n, ok)
	}
	for _, name := range []string{"officer-triage-example-web-r1", "officer-triage-example-web-4", "other-agent"} {
		if _, _, ok := officers.ProxyAgent(o, name); ok {
			t.Errorf("%s is on the proxy", name)
		}
	}
	if officers.RoutineAgent(o, "officer-triage-example-web-1") {
		t.Error("a judgement instance reads as a routine one")
	}
	// Default cap when left out.
	def := strings.Replace(judgementLLMTiers, `,"max_tokens_per_item":900000`, ``, 1)
	list, err = officers.Parse(llmDecl(`,`+def), ownKey(), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].Tiers.Judgement.LLM.MaxTokensPerItem; got != officers.DefaultLLMMaxTokensPerItem {
		t.Fatalf("default cap = %d", got)
	}
	b, _ := json.Marshal(list[0])
	if s := string(b) + fmt.Sprintf("%v %+v %#v", list[0], *list[0].Tiers, list[0].Tiers.Judgement); strings.Contains(s, llmTestKey) {
		t.Fatalf("the key is printed: %s", s)
	}
}

// TestBothPoolsMayBeOnTheProxy: routine beside `llm` and judgement with its
// own block, each with its own budget.
func TestBothPoolsMayBeOnTheProxy(t *testing.T) {
	tiers := `"tiers":{"routine":{"model":"deepseek/deepseek-v4.1-flash-1017","max_instances":2},` +
		`"judgement":{"model":"deepseek/deepseek-v4-pro","llm":{"daily_budget_usd":20}}}`
	list, err := officers.Parse(llmDecl(`,`+tiers+`,"llm":{"daily_budget_usd":5}`), ownKey(), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if r, j := o.Pool(officers.TierRoutine).PoolLLM(), o.Pool(officers.TierJudgement).PoolLLM(); r == nil || j == nil ||
		r.DailyBudgetUSD != 5 || j.DailyBudgetUSD != 20 {
		t.Fatalf("routine %v judgement %v", r, j)
	}
}

// TestParseRefusesABadJudgementLLM: each refusal names its fault.
func TestParseRefusesABadJudgementLLM(t *testing.T) {
	jl := func(model, llm string) string {
		return `,"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"` + model + `"` + llm + `}}`
	}
	for _, c := range []struct {
		name, fields string
		env          func(string) string
		want         string
	}{
		{"no key", jl("deepseek/deepseek-v4-pro", `,"llm":{"daily_budget_usd":5}`), llmEnv(map[string]string{sharedKeyEnv: "sk-or-v1-shared"}),
			"tiers.judgement.llm: " + ownKeyEnv + " is not set"},
		{"provider model without llm", jl("deepseek/deepseek-v4-pro", ``), ownKey(),
			"a judgement instance reaches only through the LLM proxy: declare tiers.judgement.llm"},
		{"llm beside a Claude model", jl("opus", `,"llm":{"daily_budget_usd":5}`), ownKey(),
			`with llm, model must be a provider model id`},
		{"routine llm", `,"tiers":{"routine":{"model":"deepseek/deepseek-v4.1-flash-1017","llm":{"daily_budget_usd":5}},"judgement":{"model":"opus"}}`, ownKey(),
			"the routine pool's LLM proxy block is the officer's top-level `llm`"},
		{"no budget", jl("deepseek/deepseek-v4-pro", `,"llm":{}`), ownKey(), "tiers.judgement.llm.daily_budget_usd is required"},
		{"budget zero", jl("deepseek/deepseek-v4-pro", `,"llm":{"daily_budget_usd":0}`), ownKey(), "tiers.judgement.llm.daily_budget_usd 0 is not in (0, 1000]"},
		{"budget over", jl("deepseek/deepseek-v4-pro", `,"llm":{"daily_budget_usd":1001}`), ownKey(), "tiers.judgement.llm.daily_budget_usd 1001 is not in (0, 1000]"},
		{"cap low", jl("deepseek/deepseek-v4-pro", `,"llm":{"daily_budget_usd":5,"max_tokens_per_item":10}`), ownKey(), "tiers.judgement.llm.max_tokens_per_item 10 is not"},
		{"unknown key", jl("deepseek/deepseek-v4-pro", `,"llm":{"daily_budget_usd":5,"model":"x/y"}`), ownKey(), "tiers.judgement.llm must be"},
	} {
		_, err := officers.Parse(llmDecl(c.fields), c.env, "muster-agent-")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
}
