package officers

// Instance tiers, as HQ names an item's `tier` (civ-hq docs/OFFICERS.md,
// "Tiers and due dates").
const (
	TierRoutine   = "routine"
	TierJudgement = "judgement"
)

// TierPool is one tier's pool of instances.
type TierPool struct {
	Model        string `json:"model"`
	MaxInstances int    `json:"max_instances"`
}

// Tiers is an officer's `tiers` block.
type Tiers struct {
	Routine   TierPool `json:"routine"`
	Judgement TierPool `json:"judgement"`
}

// Pools is the officer's instance pools.
func (o Officer) Pools() []Officer { return []Officer{o} }

// InstanceName is the agent name of instance n of this pool.
func (o Officer) InstanceName(n int) string { return InstanceName(o.ID, n) }
