package main

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// kindSet implements api.AgentKinds over the configured kinds and the
// claude-code account pool (nil when that kind is not enabled).
type kindSet struct {
	kinds []string
	pool  *ccpool.Pool
}

// Enabled is in agents.Kinds order — gateway first — whatever order
// MUSTER_AGENT_KINDS was written in: the picker checks its first option, and
// the default must be the kind every deployment had before kinds existed.
func (k kindSet) Enabled() []string {
	on := map[string]bool{}
	for _, kind := range k.kinds {
		on[kind] = true
	}
	var out []string
	for _, kind := range agents.Kinds {
		if on[kind] {
			out = append(out, kind)
		}
	}
	return out
}

func (k kindSet) ClaudeAccounts() []string {
	if k.pool == nil {
		return nil
	}
	return k.pool.Names()
}

func (k kindSet) SelectClaudeAccount(ctx context.Context, pin string) (string, error) {
	if k.pool == nil {
		return "", ccpool.ErrNoUsableAccount
	}
	return k.pool.Select(ctx, pin)
}
