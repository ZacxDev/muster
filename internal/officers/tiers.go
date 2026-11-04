package officers

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/podagent"
)

// ---------------------------------------------------------------------------
// OFFICER TIERS
//
// An officer with a `tiers` block runs two pools of instances:
//
//   - judgement: `officer-<slug>-<n>`, the names an untiered officer's
//     instances have always had;
//   - routine:   `officer-<slug>-r<n>`.
//
// Each pool has its own model and max instances, and each is woken only for
// the items of its tier as HQ reads them (ItemTier). Each instance's claim
// names its pool's tier, so HQ refuses it another tier's item (409
// wrong_tier, civ-hq docs/OFFICERS.md "Tiers and due dates").
//
// 🔴 AN OFFICER WITHOUT `tiers` IS EXACTLY TODAY'S OFFICER. It has one pool
// (Pools() is the officer itself, Tier ""), the same instance names, a claim
// that names no tier and an items list with no tier filter. Every rule below
// branches on Tiers == nil / Tier == "" first.
// ---------------------------------------------------------------------------

// Instance tiers, as HQ names an item's `tier`.
const (
	TierRoutine   = "routine"
	TierJudgement = "judgement"
)

// TierPool is one tier's pool of instances.
type TierPool struct {
	// Model is the model the pool's Claude Code sessions run (podagent.ValidModel).
	Model string `json:"model"`
	// MaxInstances is how many of the pool's instances may run at once.
	MaxInstances int `json:"max_instances"`
}

// Tiers is an officer's `tiers` block: both pools.
type Tiers struct {
	Routine   TierPool `json:"routine"`
	Judgement TierPool `json:"judgement"`
}

// ItemTier is the tier item it is worked by: routine when HQ says routine,
// and judgement otherwise — an HQ without tiers sends none, and a value muster
// does not know fails toward judgement, as HQ's own default does.
func ItemTier(it Item) string {
	if it.Tier == TierRoutine {
		return TierRoutine
	}
	return TierJudgement
}

// Pools is the officer's instance pools: the officer itself when it is
// untiered, else its judgement pool then its routine pool.
func (o Officer) Pools() []Officer {
	if o.Tiers == nil {
		return []Officer{o}
	}
	return []Officer{o.Pool(TierJudgement), o.Pool(TierRoutine)}
}

// Pool is o as tier's pool: Tier, Model and MaxInstances are the pool's,
// everything else the officer's. An untiered officer is its only pool,
// whatever tier says.
func (o Officer) Pool(tier string) Officer {
	if o.Tiers == nil {
		return o
	}
	p, src := o, o.Tiers.Judgement
	p.Tier = TierJudgement
	if tier == TierRoutine {
		p.Tier, src = TierRoutine, o.Tiers.Routine
	}
	p.Model, p.MaxInstances = src.Model, src.MaxInstances
	return p
}

// InstanceName is the agent name of instance n of this pool: `-r<n>` for the
// routine pool, `-<n>` otherwise (InstanceName).
func (o Officer) InstanceName(n int) string {
	if o.Tier == TierRoutine {
		return InstanceNamePrefix + Slug(o.ID) + "-" + routineMark + strconv.Itoa(n)
	}
	return InstanceName(o.ID, n)
}

// InstanceLabel is how instance n of this pool is named in text: "r<n>" in
// the routine pool, "<n>" otherwise.
func (o Officer) InstanceLabel(n int) string {
	if o.Tier == TierRoutine {
		return routineMark + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// routineMark is what a routine instance's number follows in its name.
const routineMark = "r"

// worksItem reports whether this pool works item it: every item for an
// untiered officer, else the items of its own tier.
func (o Officer) worksItem(it Item) bool { return o.Tier == "" || ItemTier(it) == o.Tier }

// label names the pool in a log line: the officer id, plus the pool for a
// tiered officer.
func (o Officer) label() string {
	if o.Tier == "" {
		return o.ID
	}
	return o.ID + " (" + o.Tier + " pool)"
}

// parseTiers reads a `tiers` block strictly: exactly the keys routine and
// judgement, each {model, max_instances?}, model required and a model name,
// max_instances 1..MaxInstancesCap (default 1). An unknown key at either level
// is a refusal naming it.
//
// 🔴 withLLM (the officer declares `llm`) is the ONLY way a routine pool's
// model may be a provider model id (ProviderModel): the LLM proxy is what
// serves one. Without it, and always for the judgement pool, the model is a
// Claude model name (podagent.ValidModel).
func parseTiers(raw json.RawMessage, withLLM bool) (*Tiers, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
		return nil, fmt.Errorf(`tiers must be {"routine": {"model", "max_instances"}, "judgement": {"model", "max_instances"}}: %v`, err)
	}
	var unknown []string
	for k := range keys {
		if k != TierRoutine && k != TierJudgement {
			unknown = append(unknown, strconv.Quote(k))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("tiers: unknown tier %s (the tiers are %s and %s)", strings.Join(unknown, ", "), TierRoutine, TierJudgement)
	}
	var t Tiers
	for _, p := range []struct {
		name string
		dst  *TierPool
	}{{TierRoutine, &t.Routine}, {TierJudgement, &t.Judgement}} {
		v, ok := keys[p.name]
		if !ok || string(v) == "null" {
			return nil, fmt.Errorf("tiers.%s is required: a tiers block declares both pools, routine and judgement", p.name)
		}
		pool, err := parseTierPool(v, withLLM && p.name == TierRoutine, p.name == TierRoutine)
		if err != nil {
			return nil, fmt.Errorf("tiers.%s: %w", p.name, err)
		}
		*p.dst = pool
	}
	return &t, nil
}

// parseTierPool reads one pool. providerOK admits a provider model id (the
// routine pool beside `llm`); routine names the pool in that id's refusal.
func parseTierPool(raw json.RawMessage, providerOK, routine bool) (TierPool, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var p TierPool
	if err := dec.Decode(&p); err != nil {
		return TierPool{}, fmt.Errorf(`must be {"model", "max_instances"}: %v`, err)
	}
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" {
		return TierPool{}, errors.New("model is required")
	}
	switch {
	case ProviderModel(p.Model) /* MUTANT M13 */ || providerOK && false:
	case routine && ProviderModel(p.Model):
		return TierPool{}, fmt.Errorf("model %q is a provider model id, which a routine instance reaches only through the "+
			"LLM proxy: declare the officer's `llm` block, or use a Claude model", p.Model)
	default:
		if err := podagent.ValidModel(p.Model); err != nil {
			return TierPool{}, err
		}
	}
	if p.MaxInstances == 0 {
		p.MaxInstances = DefaultMaxInstances
	}
	if p.MaxInstances < 1 || p.MaxInstances > MaxInstancesCap {
		return TierPool{}, fmt.Errorf("max_instances %d is not 1..%d", p.MaxInstances, MaxInstancesCap)
	}
	return p, nil
}
