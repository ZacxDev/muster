package agents

import (
	"context"
	"sync"
)

// A TURN'S TOKENS, as its runtime reported them in response.completed's
// `usage` (internal/chiefusage records them per chief turn).
//
// Two shapes are read, because two kinds of runtime answer this wire:
//
//   - ccd (a Claude Code CLI) reports the Anthropic split — input_tokens
//     (uncached), cache_read_input_tokens, cache_creation_input_tokens and
//     output_tokens — summed over the turn's API messages (cmd/ccd
//     transcript.go, internal/podagent WriteResponseUsage);
//   - a Responses-API gateway reports input_tokens INCLUDING the cached ones,
//     with input_tokens_details.cached_tokens naming how many were.
//
// So when cache_read_input_tokens or cache_creation_input_tokens is present
// the numbers are taken as they are; otherwise cached_tokens is moved out of
// input into cache read. A response with no usage leaves the sink as it was,
// and a sink no request filled reports known false.

// responsesUsage is response.completed's `usage`.
type responsesUsage struct {
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	InputTokensDetails       *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// split is u as input (uncached), output, cache read and cache creation.
func (u responsesUsage) split() (in, out, read, create int64) {
	in, out = u.InputTokens, u.OutputTokens
	switch {
	case u.CacheReadInputTokens != nil || u.CacheCreationInputTokens != nil:
		if u.CacheReadInputTokens != nil {
			read = *u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens != nil {
			create = *u.CacheCreationInputTokens
		}
	case u.InputTokensDetails != nil:
		read = min(max(u.InputTokensDetails.CachedTokens, 0), max(in, 0))
		in -= read
	}
	return max(in, 0), max(out, 0), max(read, 0), max(create, 0)
}

// TurnUsage accumulates one turn's tokens over every request it made.
type TurnUsage struct {
	mu                                      sync.Mutex
	input, output, cacheRead, cacheCreation int64
	known                                   bool
}

// Snapshot is the tokens so far and whether any request reported usage.
func (t *TurnUsage) Snapshot() (input, output, cacheRead, cacheCreation int64, known bool) {
	if t == nil {
		return 0, 0, 0, 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.input, t.output, t.cacheRead, t.cacheCreation, t.known
}

type turnUsageKey struct{}

// WithTurnUsage returns ctx carrying a fresh sink every request made under it
// adds its reported usage to, and the sink.
func WithTurnUsage(ctx context.Context) (context.Context, *TurnUsage) {
	t := &TurnUsage{}
	return context.WithValue(ctx, turnUsageKey{}, t), t
}

// addTurnUsage adds u to ctx's sink, when ctx has one and u is not nil.
func addTurnUsage(ctx context.Context, u *responsesUsage) {
	t, _ := ctx.Value(turnUsageKey{}).(*TurnUsage)
	if t == nil || u == nil {
		return
	}
	in, out, read, create := u.split()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.input += in
	t.output += out
	t.cacheRead += read
	t.cacheCreation += create
	t.known = true
}
