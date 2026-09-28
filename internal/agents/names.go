package agents

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
)

// adjective-noun name parts, DNS-label-safe (lowercase alphanumeric) because
// provision.Ref.Name is used directly as an object, container or directory name
// by several drivers and is validated against that shape.
//
// ⚠ NO TENANT PREFIX. The generator this was modelled on prefixed each name
// with a tenant and a template id; muster is single-tenant, so the pair alone
// is the whole name. A multi-tenant deployment would have to widen this, and
// the collision loop in BuildUniqueAgentName is what would notice first.
var (
	adjectives = []string{
		"swift", "brave", "clever", "calm", "bold", "bright", "keen", "lively",
		"nimble", "quiet", "sharp", "sturdy", "witty", "zesty", "mellow", "spry",
	}
	nouns = []string{
		"claw", "fox", "owl", "lynx", "otter", "hawk", "wren", "raven", "moth",
		"newt", "vole", "finch", "shrew", "stoat", "heron", "crane",
	}
)

// ChiefName is the one RESERVED agent name: the standing agent the operator
// talks to, as opposed to a worker dispatched against a task.
//
// 🔴 IT IS A CONSTANT BECAUSE THE INSTRUCTIONS BRANCH ON IT, AND A NAME THE
// GENERATOR PRODUCED WOULD SILENTLY FAIL THAT BRANCH. Ordinary agent names come
// from the adjective-noun pool above, so nothing a human can type reaches this
// value — POST /chief/provision is the only producer. Upstream, a release that
// reserved this name before anything could create one shipped an agent whose
// whole instruction set was unreachable: it was handed the WORKER instructions,
// dutifully asked for its assigned task, was told it had none, and reported to
// the operator that there was nothing to do. It was not malfunctioning; it was
// executing the only instructions it had.
//
// ⚠ IT IS NOT IN adjectives/nouns AND MUST NOT BE. The collision loop in
// BuildUniqueAgentName guards against an accidental duplicate of an EXISTING
// agent, not against the pool minting a reserved word — so a reserved name that
// the pool could also produce would be a coin flip, not a guard.
const ChiefName = "chief"

func pick(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return list[0]
	}
	return list[n.Int64()]
}

// generateName returns a random adjective-noun slug.
func generateName() string { return pick(adjectives) + "-" + pick(nouns) }

// BuildUniqueAgentName returns an adjective-noun name that is not TAKEN, where
// taken means "held by a live agent, OR ever held by one that was destroyed"
// (10 attempts, then an error).
//
// 🔴 A DESTROYED AGENT'S NAME IS NEVER REISSUED, AND THE REFUSAL IS THE WHOLE
// POINT OF THE LOOP RATHER THAN A TIDINESS RULE. The name is the provisioning
// driver's Ref.Name, so it is what the per-instance namespace, the
// ServiceAccount and a granted profile's cluster-scoped ClusterRole /
// ClusterRoleBinding are all named after. Those policy objects are
// cluster-scoped and outlive the agent row; a namesake gets the same
// ServiceAccount and silently inherits access nobody granted it. The
// enforcement lives in the STORE — PGStore.NameExists consults the
// agent_retired_names ledger that migration 0002's AFTER DELETE trigger fills —
// because a check written here would only cover this one call path, while the
// trigger covers every DELETE in any binary.
//
// ⚠ SO THE POOL ONLY EVER SHRINKS, AND EXHAUSTION IS A REAL ENDING. With 16
// adjectives and 16 nouns there are 256 combinations; with k of them taken each
// draw misses with probability k/256, so this returns an error with probability
// (k/256)^10 — 0.1% at k=128, 8.5% at k=200, certain at k=256. The error is the
// designed behaviour at that point: returning a name it cannot prove is free
// would hand a caller the namesake. Widening the lists is the fix when it
// starts failing.
func BuildUniqueAgentName(ctx context.Context, store Store) (string, error) {
	for i := 0; i < 10; i++ {
		n := generateName()
		exists, err := store.NameExists(ctx, n)
		if err != nil {
			return "", err
		}
		if !exists {
			return n, nil
		}
	}
	return "", errors.New("agents: could not generate a unique name")
}
