package mirrorhost

import (
	"context"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// THE CONTEXT READOUT OF AN OPENCODE VIEW (docs/design/opencode-first-class.md,
// phase 3): the view's stream carries the session's context fill
// (mirror.Msg.Context) beside its events — opencodehost.ContextOf over the
// messages it read (the newest step's input + cache read + cache write, the
// quantity a Claude Code view counts), the model that step ran on, and that
// model's context window from GET /api/model (limit.context), read once per
// openCodeModelsMaxAge for the whole host. A model the list does not name
// has no limit (0): the readout then shows the count alone, as Claude
// Code's does.

// openCodeModelsMaxAge bounds how old a model list a limit is read from;
// openCodeModelsRetry how soon a model the list lacks (or a failed read)
// reads it again.
const (
	openCodeModelsMaxAge = 10 * time.Minute
	openCodeModelsRetry  = time.Minute
)

// openCodeModels is a host's cached GET /api/model.
type openCodeModels struct {
	mu   sync.Mutex
	list []opencodehost.Model
	at   time.Time // when list was read (zero: never)
	try  time.Time // when it was last read, or tried
}

// contextLimit is the context window of model id of provider (GET
// /api/model's limit.context), 0 when unknown.
func (s *OpenCodeSource) contextLimit(provider, id string) int64 {
	if id == "" {
		return 0
	}
	c := &s.models
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	limit := opencodehost.ContextLimit(c.list, provider, id)
	stale := c.at.IsZero() || now.Sub(c.at) >= openCodeModelsMaxAge
	if (limit > 0 && !stale) || now.Sub(c.try) < openCodeModelsRetry {
		return limit
	}
	c.try = now
	cl, err := s.client()
	if err != nil {
		return limit
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	list, err := cl.Models(ctx)
	if err != nil {
		return limit // the last list holds until a read succeeds
	}
	c.list, c.at = list, now
	return opencodehost.ContextLimit(list, provider, id)
}

// agentContext is the context fill of msgs (a session's messages, oldest
// first) as the wire carries it, nil when no step reported usage.
func (s *OpenCodeSource) agentContext(msgs []opencodehost.Message) *mirror.AgentContext {
	c, ok := opencodehost.ContextOf(msgs)
	if !ok {
		return nil
	}
	return mirror.CleanAgentContext(&mirror.AgentContext{Tokens: c.Tokens, Limit: s.contextLimit(c.ProviderID, c.ModelID), Model: c.ModelID})
}

// sameContext reports whether a and b say the same.
func sameContext(a, b *mirror.AgentContext) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
