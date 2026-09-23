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

func pick(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return list[0]
	}
	return list[n.Int64()]
}

// generateName returns a random adjective-noun slug.
func generateName() string { return pick(adjectives) + "-" + pick(nouns) }

// BuildUniqueAgentName returns an adjective-noun name not already used by an
// existing agent (10 attempts).
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
