package officers_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// The `llm` block and the tiers, together: `tiers.routine.model` is THE model
// the LLM proxy pins a routine instance to; routine membership is the routine
// pool's `-r<n>` names; the `llm` block holds only spend (the daily budget, the
// per-item token cap) and the key comes from the officer's key variable.

const llmTestKey = "sk-or-v1-officer-own-key-7a6b5c4d3e2f1a0b"

// llmTiers is a tiers block whose routine pool runs an OpenRouter id (allowed
// only beside `llm`); routine max 2, judgement max 3, so a pool reading the
// other's max cannot pass.
const llmTiers = `"tiers":{"routine":{"model":"deepseek/deepseek-v4.1-flash-1017","max_instances":2},"judgement":{"model":"opus","max_instances":3}}`

func llmDecl(fields string) string {
	return `[{"id":"triage:example-web","hq_url":"https://hq.example.com/api/v1"` + fields + `}]`
}

func llmEnv(extra map[string]string) func(string) string {
	m := map[string]string{"MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB": "hq-tok"}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

// TestParseReadsTheLLMBlockAndItsOwnKey: the block holds the spend settings,
// the routine pool's model is the pin, the officer's OWN key wins over the
// shared one, and neither JSON nor fmt prints it.
func TestParseReadsTheLLMBlockAndItsOwnKey(t *testing.T) {
	e := llmEnv(map[string]string{
		"MUSTER_OFFICER_OPENROUTER_KEY_TRIAGE_EXAMPLE_WEB": llmTestKey,
		officers.LLMSharedKeyEnv:                           "sk-or-v1-shared-admin-key-should-lose",
	})
	list, err := officers.Parse(llmDecl(`,`+llmTiers+`,"llm":{"daily_budget_usd":5}`), e, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	l := o.LLM
	if l == nil || l.DailyBudgetUSD != 5 || l.MaxTokensPerItem != officers.DefaultLLMMaxTokensPerItem {
		t.Fatalf("llm = %+v", l)
	}
	if m := o.Pool(officers.TierRoutine).Model; m != "deepseek/deepseek-v4.1-flash-1017" {
		t.Fatalf("the routine pool's model = %q, want the OpenRouter id from tiers.routine", m)
	}
	if m := o.Pool(officers.TierJudgement).Model; m != "opus" {
		t.Fatalf("the judgement pool's model = %q", m)
	}
	if l.Key() != llmTestKey || l.KeySource != "MUSTER_OFFICER_OPENROUTER_KEY_TRIAGE_EXAMPLE_WEB" {
		t.Fatalf("key from %s, want the officer's own", l.KeySource)
	}
	b, _ := json.Marshal(o)
	lb, _ := json.Marshal(l)
	for _, s := range []string{string(b), string(lb), fmt.Sprintf("%v %+v %#v %s", l, l, l, l), fmt.Sprintf("%v %+v %#v", o, o, o)} {
		if strings.Contains(s, llmTestKey) {
			t.Fatalf("the key is printed: %s", s)
		}
	}
}

// TestARoutineInstanceIsAnRNameOfTheRoutinePool: membership is the routine
// pool's own naming — `-r<n>` within the routine max — read through either of
// the officer's pools; a judgement instance, an `-r<n>` past the max, another
// officer's name and an untiered or llm-less officer are not routine.
func TestARoutineInstanceIsAnRNameOfTheRoutinePool(t *testing.T) {
	e := llmEnv(map[string]string{officers.LLMSharedKeyEnv: "sk-shared"})
	list, err := officers.Parse(llmDecl(`,`+llmTiers+`,"llm":{"daily_budget_usd":5}`), e, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	for _, view := range []officers.Officer{o, o.Pool(officers.TierRoutine), o.Pool(officers.TierJudgement)} {
		for name, want := range map[string]bool{
			"officer-triage-example-web-r1": true,
			"officer-triage-example-web-r2": true,
			"officer-triage-example-web-r3": false, // past the routine max (2)
			"officer-triage-example-web-r0": false,
			"officer-triage-example-web-1":  false, // a judgement instance
			"officer-triage-example-web-3":  false,
			"officer-other-area-r1":         false,
			"brave-otter":                   false,
		} {
			if got := officers.RoutineAgent(view, name); got != want {
				t.Errorf("RoutineAgent(%s pool, %q) = %v, want %v", view.Tier, name, got, want)
			}
		}
	}
	// The registry agrees: the -r1 name resolves to the routine pool.
	if p, n, ok := officers.NewRegistry(list).ForAgentName("officer-triage-example-web-r1"); !ok || n != 1 || p.Tier != officers.TierRoutine {
		t.Fatalf("ForAgentName(-r1) = %s %d %v", p.Tier, n, ok)
	}

	// A tiered officer without `llm`: nothing is routine for the proxy.
	plain, err := officers.Parse(llmDecl(`,"tiers":{"routine":{"model":"haiku","max_instances":2},"judgement":{"model":"opus"}}`), e, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if officers.RoutineAgent(plain[0], "officer-triage-example-web-r1") {
		t.Error("a tiered officer without llm admits its routine instance to the proxy")
	}
	// An untiered officer: no -r names at all.
	untiered, err := officers.Parse(llmDecl(``), e, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"officer-triage-example-web-r1", "officer-triage-example-web-1"} {
		if officers.RoutineAgent(untiered[0], name) {
			t.Errorf("an untiered officer admits %q", name)
		}
	}
}

// TestParseFallsBackToTheSharedKey, and names it as the source.
func TestParseFallsBackToTheSharedKey(t *testing.T) {
	list, err := officers.Parse(llmDecl(`,`+llmTiers+`,"llm":{"daily_budget_usd":1,"max_tokens_per_item":5000}`),
		llmEnv(map[string]string{officers.LLMSharedKeyEnv: "sk-shared"}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if l := list[0].LLM; l.Key() != "sk-shared" || l.KeySource != officers.LLMSharedKeyEnv || l.MaxTokensPerItem != 5000 {
		t.Fatalf("llm = %+v from %s", l, l.KeySource)
	}
}

// TestParseRefusesABadLLMBlock: each boot refusal names its reason. The
// control at the end is the same declaration, well formed, parsing — so each
// case is the one thing it changed.
func TestParseRefusesABadLLMBlock(t *testing.T) {
	withKey := llmEnv(map[string]string{officers.LLMSharedKeyEnv: "sk-shared"})
	noKey := llmEnv(nil)
	llm := `,"llm":{"daily_budget_usd":1}`
	routine := func(model string) string {
		return `,"tiers":{"routine":{"model":"` + model + `"},"judgement":{"model":"opus"}}`
	}
	for _, c := range []struct {
		name, fields string
		e            func(string) string
		want         string
	}{
		{"llm with no tiers", llm, withKey, "llm needs tiers.routine"},
		{"llm on a shipping officer", llm + `,"repo":"example-org/widget","ship":{}`, withKey, "llm is not supported on a shipping officer"},
		{"a routine OpenRouter id with no llm", routine("deepseek/deepseek-v4.1-flash"), withKey, "only through the LLM proxy"},
		{"llm with a Claude routine model", routine("haiku") + llm, withKey, "tiers.routine.model must be a provider model id"},
		{"a judgement OpenRouter id, even with llm", `,"tiers":{"routine":{"model":"vendor/model"},"judgement":{"model":"vendor/model"}}` + llm, withKey, "tiers.judgement"},
		{"the model is the tier's, not the block's", `,` + llmTiers + `,"llm":{"model":"vendor/model","daily_budget_usd":1}`, withKey, "unknown field"},
		{"no allowlist: membership is the pool", `,` + llmTiers + `,"llm":{"daily_budget_usd":1,"routine_agents":["officer-triage-example-web-r1"]}`, withKey, "unknown field"},
		{"no budget", `,` + llmTiers + `,"llm":{}`, withKey, "daily_budget_usd is required"},
		{"a zero budget", `,` + llmTiers + `,"llm":{"daily_budget_usd":0}`, withKey, "not in (0, 1000]"},
		{"an oversized budget", `,` + llmTiers + `,"llm":{"daily_budget_usd":1001}`, withKey, "not in (0, 1000]"},
		{"a tiny item cap", `,` + llmTiers + `,"llm":{"daily_budget_usd":1,"max_tokens_per_item":10}`, withKey, "max_tokens_per_item 10"},
		{"no key", `,` + llmTiers + llm, noKey, "no provider key"},
	} {
		_, err := officers.Parse(llmDecl(c.fields), c.e, "muster-agent-")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to say %q", c.name, err, c.want)
		}
	}
	if _, err := officers.Parse(llmDecl(`,`+llmTiers+llm), withKey, "muster-agent-"); err != nil {
		t.Fatalf("control: %v", err)
	}
}
