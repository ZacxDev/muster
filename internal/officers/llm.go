package officers

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// An officer's `llm` block: the settings muster's LLM proxy (internal/llmproxy,
// served at POST /llm/v1/messages on the connect listener) applies to the
// officer's instances. See docs/officers.md "The LLM proxy".
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

// LLMSharedKeyEnv is the deployment's admin OpenRouter key, used for an
// officer that has no key of its own. It is the same variable the agents'
// OpenRouter credential reads (cmd/muster-server).
const LLMSharedKeyEnv = "MUSTER_AGENT_OPENROUTER_KEY"

// LLM is an officer's `llm` block.
type LLM struct {
	// Model is the provider model id EVERY proxied request is rewritten to,
	// whatever the client asked for (e.g. `deepseek/deepseek-v4.1-flash-1017`).
	Model string `json:"model"`
	// DailyBudgetUSD is the officer's spend cap per UTC day, summed over its
	// instances and metered from the provider's own reported cost.
	DailyBudgetUSD float64 `json:"daily_budget_usd"`
	// MaxTokensPerItem ends a runaway turn: once an instance has used this
	// many tokens (input, cache and output together) on the item it holds,
	// its next request is refused until it holds a different item.
	MaxTokensPerItem int64 `json:"max_tokens_per_item"`
	// RoutineAgents are the instance agent names allowed to use the proxy.
	// 🔴 THIS IS AN ALLOWLIST STANDING IN FOR THE ROUTINE TIER, which is not
	// in muster yet: when instances carry a tier, "a routine-tier instance of
	// this officer" replaces it (RoutineAgent is the one predicate to change).
	// Sorted and unique; every entry is an instance name of this officer.
	RoutineAgents []string `json:"routine_agents"`
	// KeySource names where the key came from (LLMKeyEnvPrefix<NAME> or
	// LLMSharedKeyEnv), never the key.
	KeySource string `json:"-"`

	key string
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
	return fmt.Sprintf("llm %s (budget $%.2f/day, %d tokens/item, %d agents, key from %s)",
		l.Model, l.DailyBudgetUSD, l.MaxTokensPerItem, len(l.RoutineAgents), l.KeySource)
}

// GoString is String, so %#v cannot print the key either.
func (l LLM) GoString() string { return l.String() }

// RoutineAgent is THE rule for whether agent name may use officer o's LLM
// proxy: o declares `llm` and lists name in its routine_agents.
func RoutineAgent(o Officer, name string) bool {
	if o.LLM == nil {
		return false
	}
	for _, a := range o.LLM.RoutineAgents {
		if a == name {
			return true
		}
	}
	return false
}

// llmModelRE is an OpenRouter model id: `<vendor>/<model>`, optionally with a
// `:variant` suffix.
var llmModelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[a-z0-9][a-z0-9._-]{0,99}(:[a-z0-9-]{1,32})?$`)

type declaredLLM struct {
	Model            string   `json:"model"`
	DailyBudgetUSD   *float64 `json:"daily_budget_usd"`
	MaxTokensPerItem int64    `json:"max_tokens_per_item"`
	RoutineAgents    []string `json:"routine_agents"`
}

// parseLLM reads officer o's `llm` block strictly, and its key through getenv.
func parseLLM(o Officer, raw json.RawMessage, getenv func(string) string) (*LLM, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var d declaredLLM
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("llm must be {\"model\", \"daily_budget_usd\", \"max_tokens_per_item\", \"routine_agents\"}: %v", err)
	}
	l := &LLM{Model: strings.TrimSpace(d.Model), MaxTokensPerItem: d.MaxTokensPerItem}
	if !llmModelRE.MatchString(l.Model) {
		return nil, fmt.Errorf("llm.model %q is not a provider model id (<vendor>/<model>)", d.Model)
	}
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
	if len(d.RoutineAgents) == 0 {
		return nil, errors.New("llm.routine_agents is empty: name the instance agents (e.g. " + InstanceName(o.ID, 1) +
			") that may use the proxy")
	}
	seen := map[string]bool{}
	for _, a := range d.RoutineAgents {
		a = strings.TrimSpace(a)
		if _, _, ok := parseInstance(o, a); !ok {
			return nil, fmt.Errorf("llm.routine_agents entry %q is not an instance of %s (%s … %s)", a, o.ID,
				InstanceName(o.ID, 1), InstanceName(o.ID, o.MaxInstances))
		}
		if !seen[a] {
			seen[a] = true
			l.RoutineAgents = append(l.RoutineAgents, a)
		}
	}
	sort.Strings(l.RoutineAgents)
	own := LLMKeyEnvPrefix + TokenEnvSuffix(o.ID)
	if k := strings.TrimSpace(getenv(LLMKeyEnvPrefix + TokenEnvSuffix(o.ID))); k != "" {
		l.key, l.KeySource = k, own
	} else if k := strings.TrimSpace(getenv(LLMSharedKeyEnv)); k != "" {
		l.key, l.KeySource = k, LLMSharedKeyEnv
	} else {
		return nil, fmt.Errorf("llm: no provider key: set %s (recommended: a key with its own provider-side credit limit) or %s",
			own, LLMSharedKeyEnv)
	}
	return l, nil
}
