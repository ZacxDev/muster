package officers

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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
	// Image, when set, is the runner image the pool's pods run (image.go);
	// checked by the officer's parse, which holds the allowlist.
	Image string `json:"image,omitempty"`
	// MaxSessionsPerDay, on the judgement pool only, caps the distinct items
	// it claims per UTC day (sessions.go); 0 is no cap. The routine pool is
	// bounded by the `llm` budget instead.
	MaxSessionsPerDay int `json:"max_sessions_per_day,omitempty"`
	// MaxItemMinutes, on the judgement pool only, is the wall-time cap on ONE
	// item: once an instance of the pool has held an item this long, the wake
	// loop stops the instance and completes the item escalated (Decide,
	// Overrun). 0 is no cap.
	MaxItemMinutes int `json:"max_item_minutes,omitempty"`
	// FreshSessionPerItem, on either pool: every item an instance of the pool
	// claims is worked in a NEW Claude Code conversation
	// (Officer.FreshSessionPerItem).
	FreshSessionPerItem bool `json:"fresh_session_per_item,omitempty"`
}

// FreshSessionPerItem is THE rule for whether pool o works each item in a NEW
// Claude Code conversation (tiers.<pool>.fresh_session_per_item): false for
// an untiered officer, and for a tiered officer not read as one of its pools.
//
// Everything the option changes branches here, and nowhere else:
//
//   - RepromptText: the wake loop's prompt of an idle instance is the
//     kickoff, never "as before" (a new conversation has no "before");
//   - KickoffText: the kickoff ends with the one-item note (freshNote);
//   - internal/api OfficerTurnContext: every turn muster sends the instance —
//     the wake loop's prompt and the kickoff deliverer's first turn — carries
//     agents.HeaderCCNewConversation, on which the pod's ccd starts a new
//     conversation (the old transcript kept) before it pastes the turn;
//   - internal/api officer_claim: one item per conversation — the
//     conversation is marked at its first successful claim and refused any
//     other item before HQ, however that hold ends, until ccd confirms a new
//     conversation took a turn muster sent;
//   - InstanceState.Spent (internal/api Instances): such an instance, holding
//     nothing, is prompted without waiting out RepromptAfter.
//
// 🔴 AN UNTIERED OFFICER IS NOT OFFERED IT. An officer without `tiers` is
// exactly today's officer (see the top of this file); the option lives with
// the pool settings it sits beside.
func (o Officer) FreshSessionPerItem() bool {
	if o.Tiers == nil {
		return false
	}
	switch o.Tier {
	case TierJudgement:
		return o.Tiers.Judgement.FreshSessionPerItem
	case TierRoutine:
		return o.Tiers.Routine.FreshSessionPerItem
	}
	return false
}

// MaxItemMinutesCap bounds tiers.judgement.max_item_minutes: a day.
const MaxItemMinutesCap = 24 * 60

// ItemCap is the pool's wall-time cap on one item: 0 (none) unless o is the
// judgement pool of a tiered officer that declares max_item_minutes.
func (o Officer) ItemCap() time.Duration {
	if o.Tiers == nil || o.Tier != TierJudgement {
		return 0
	}
	return time.Duration(o.Tiers.Judgement.MaxItemMinutes) * time.Minute
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
	p.Model, p.MaxInstances, p.Image = src.Model, src.MaxInstances, src.Image
	if p.Tier == TierRoutine {
		// 🔴 ONLY THE JUDGEMENT POOL SHIPS. A routine instance (a cheaper
		// model, fed untriaged input) gets no ship tool, push gate or officer
		// App token: every one of those branches on Ship (ToolOffered,
		// api.Server.shipGate), so nil here is the whole rule.
		p.Ship, p.CILogs = nil, nil
	}
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
		return TierPool{}, fmt.Errorf(`must be {"model", "max_instances"?, "image"?, "fresh_session_per_item"?, "max_sessions_per_day"?, "max_item_minutes"? (both judgement only)}: %v`, err)
	}
	p.Model, p.Image = strings.TrimSpace(p.Model), strings.TrimSpace(p.Image)
	if p.Model == "" {
		return TierPool{}, errors.New("model is required")
	}
	switch {
	case providerOK && ProviderModel(p.Model):
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
	switch {
	case routine && p.MaxSessionsPerDay != 0:
		return TierPool{}, errors.New("max_sessions_per_day caps the judgement pool only; the routine pool is bounded by the officer's `llm` budget")
	case p.MaxSessionsPerDay < 0 || p.MaxSessionsPerDay > MaxSessionsPerDayCap:
		return TierPool{}, fmt.Errorf("max_sessions_per_day %d is not 1..%d (leave it out for no cap)", p.MaxSessionsPerDay, MaxSessionsPerDayCap)
	}
	switch {
	case routine && p.MaxItemMinutes != 0:
		return TierPool{}, errors.New("max_item_minutes caps the judgement pool only")
	case p.MaxItemMinutes < 0 || p.MaxItemMinutes > MaxItemMinutesCap:
		return TierPool{}, fmt.Errorf("max_item_minutes %d is not 1..%d (leave it out for no cap)", p.MaxItemMinutes, MaxItemMinutesCap)
	}
	return p, nil
}
