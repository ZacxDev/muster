package main

import (
	"context"

	"github.com/ZacxDev/muster/internal/ccpool"
)

// kindSet implements api.AgentKinds over the configured kinds and the
// claude-code account pool (nil when that kind is not enabled).
type kindSet struct {
	kinds []string
	pool  *ccpool.Pool
}

func (k kindSet) Enabled() []string {
	out := make([]string, len(k.kinds))
	copy(out, k.kinds)
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
