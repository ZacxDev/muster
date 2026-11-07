package officers

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// An officer's `llm` block: the settings muster's LLM proxy (internal/llmproxy,
// served at POST /llm/v1/messages on the connect listener) applies to the
// officer's ROUTINE pool (tiers.go). See docs/officers.md "Tiers and the LLM
// proxy".
//
// 🔴 THE PROVIDER KEY NEVER LEAVES muster-server, exactly as the HQ token does
// not: it is read from the server's own environment, held behind an
// unexported field that encoding/json and fmt never print, and attached by the
// proxy on its way to the provider. An instance authenticates with its own
// muster token and holds no provider credential at all.

// Bounds on an `llm` block, so a typo is a boot refusal rather than an
// unbounded bill.
const (
	// MaxLLMDailyBudgetUSD bounds daily_budget_usd.
	MaxLLMDailyBudgetUSD = 1000.0
	// DefaultLLMMaxTokensPerItem is max_tokens_per_item when it is left out.
	DefaultLLMMaxTokensPerItem = 400_000
	// MinLLMMaxTokensPerItem and MaxLLMMaxTokensPerItem bound it.
	MinLLMMaxTokensPerItem = 1_000
	MaxLLMMaxTokensPerItem = 20_000_000
)

// LLMKeyEnvPrefix is the prefix of the variable an officer's own provider key
// is read from: MUSTER_OFFICER_OPENROUTER_KEY_<NAME>, NAME as for the HQ
// token ([TokenEnvSuffix]).
const LLMKeyEnvPrefix = "MUSTER_OFFICER_OPENROUTER_KEY_"

// 🔴 THERE IS NO FALLBACK KEY. The deployment's shared OpenRouter key
// (MUSTER_AGENT_OPENROUTER_KEY) never stands in for an officer's own: the
// officer's key is the one that carries a provider-side credit limit, the
// budget wall that does not depend on muster.

// LLM is an officer's `llm` block: what the LLM proxy may SPEND for the
// officer. The model it pins a routine instance to is the routine pool's
// (tiers.routine.model), and who may use it is the routine pool's instances
// (RoutineAgent) — one source each, so neither is repeated here.
type LLM struct {
	// DailyBudgetUSD is the officer's spend cap per UTC day, summed over its
	// routine instances and metered from the provider's own reported cost.
	DailyBudgetUSD float64 `json:"daily_budget_usd"`
	// MaxTokensPerItem ends a runaway turn: once an instance has used this
	// many tokens (input, cache and output together) on the item it holds,
	// its next request is refused until it holds a different item. It does
	// not apply while the instance holds no item: DailyBudgetUSD bounds that.
	MaxTokensPerItem int64 `json:"max_tokens_per_item"`
	key              string
}

// Key returns the provider key. Its only caller is the proxy, on the way to
// the provider, and tests asserting it appears nowhere else.
func (l *LLM) Key() string {
	if l == nil {
		return ""
	}
	return l.key
}

// WithKey returns a copy of l carrying key (tests, and wiring outside Parse).
func (l LLM) WithKey(key string) *LLM { l.key = key; return &l }

// String describes l without its key.
func (l LLM) String() string {
	return fmt.Sprintf("llm budget $%.2f/day, %d tokens/item", l.DailyBudgetUSD, l.MaxTokensPerItem)
}

// GoString is String, so %#v cannot print the key either.
func (l LLM) GoString() string { return l.String() }

// RoutineAgent is THE rule for whether agent name may use officer o's LLM
// proxy: o declares `llm`, and name is an instance of o's ROUTINE pool — an
// `-r<n>` within tiers.routine.max_instances, by the registry's own naming
// rule (parseInstance). o may be either of the officer's pools. A judgement
// instance, an untiered officer's instance, or any other name is not.
func RoutineAgent(o Officer, name string) bool {
	if o.LLM == nil || o.Tiers == nil {
		return false
	}
	p, _, ok := parseInstance(o, name)
	return ok && p.Tier == TierRoutine
}

// ProviderModel reports whether m is a provider model id (`<vendor>/<model>`,
// optionally `:variant`): the only kind of model a routine pool may run when
// its officer declares `llm`, and a kind it may run ONLY then.
func ProviderModel(m string) bool { return llmModelRE.MatchString(m) }

// llmModelRE is an OpenRouter model id: `<vendor>/<model>`, optionally with a
// `:variant` suffix.
var llmModelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[a-z0-9][a-z0-9._-]{0,99}(:[a-z0-9-]{1,32})?$`)

type declaredLLM struct {
	DailyBudgetUSD   *float64 `json:"daily_budget_usd"`
	MaxTokensPerItem int64    `json:"max_tokens_per_item"`
}

// parseLLM reads officer o's `llm` block strictly, and its key through getenv.
// o's tiers are already parsed: `llm` needs a routine pool, and that pool's
// model must be a provider model id (parseTiers admits one only beside `llm`).
func parseLLM(o Officer, raw json.RawMessage, getenv func(string) string) (*LLM, error) {
	if o.Tiers == nil {
		return nil, errors.New("llm needs tiers.routine: the proxy serves only a routine pool's instances, and pins them to " +
			"tiers.routine.model; declare tiers")
	}
	if m := o.Tiers.Routine.Model; !ProviderModel(m) {
		return nil, fmt.Errorf("with llm, tiers.routine.model must be a provider model id (<vendor>/<model>, e.g. "+
			"deepseek/deepseek-v4.1-flash-1017): %q is a Claude model, which the proxy cannot serve", m)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var d declaredLLM
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("llm must be {\"daily_budget_usd\", \"max_tokens_per_item\"} (the model is tiers.routine.model, "+
			"and the routine instances are the routine pool's): %v", err)
	}
	l := &LLM{MaxTokensPerItem: d.MaxTokensPerItem}
	if d.DailyBudgetUSD == nil {
		return nil, errors.New("llm.daily_budget_usd is required: the proxy refuses to spend without a cap")
	}
	b := *d.DailyBudgetUSD
	if math.IsNaN(b) || b <= 0 || b > MaxLLMDailyBudgetUSD {
		return nil, fmt.Errorf("llm.daily_budget_usd %v is not in (0, %v]", b, MaxLLMDailyBudgetUSD)
	}
	l.DailyBudgetUSD = b
	if l.MaxTokensPerItem == 0 {
		l.MaxTokensPerItem = DefaultLLMMaxTokensPerItem
	}
	if l.MaxTokensPerItem < MinLLMMaxTokensPerItem || l.MaxTokensPerItem > MaxLLMMaxTokensPerItem {
		return nil, fmt.Errorf("llm.max_tokens_per_item %d is not %d..%d", d.MaxTokensPerItem, MinLLMMaxTokensPerItem, MaxLLMMaxTokensPerItem)
	}
	k := strings.TrimSpace(getenv(LLMKeyEnvPrefix + TokenEnvSuffix(o.ID)))
	if k == "" {
		return nil, fmt.Errorf("llm: %s%s is not set: an officer with llm needs its own OpenRouter key (give it a "+
			"provider-side credit limit); the shared key is never used for it", LLMKeyEnvPrefix, TokenEnvSuffix(o.ID))
	}
	l.key = k
	return l, nil
}
