package opencodehost

// CONTEXT FILL OF AN OPENCODE SESSION (docs/design/opencode-first-class.md,
// phase 3): how many tokens of context the conversation fills, as of its
// latest step that reported usage — the SAME quantity a Claude Code view's
// readout counts (internal/cctranscript usage.go), so the two runtimes'
// numbers mean one thing.
//
//	context = tokens.input + tokens.cache.read + tokens.cache.write
//
// of the NEWEST assistant message whose sum is above zero. An opencode
// assistant message is one model request (a step: session.step.started and
// session.step.ended name it by assistantMessageID), so its input is
// everything the model was SENT for that request. opencode's input is net
// of the cached tokens (measured, testdata/prompt-v2.0.32.json: a turn's two
// steps read input 3533 + cache 1792, then input 91 + cache 5248 — the
// prompt grew, the uncached part shrank), so the cache terms are added back.
//
// NOT COUNTED: output and reasoning. What a step wrote is in the NEXT step's
// input, exactly as Claude Code's output_tokens is left out of its readout.
// ⚠ opencode's own TUI shows a larger figure: it adds output and reasoning
// too (packages/tui/src/util/session.ts at 6ec5682). muster's is the
// Claude-comparable one.
//
// A message still streaming has no tokens (or all zero) and is skipped, so
// the readout holds the last completed step's figure until the next one
// completes. A session with no such message has none (nil: no readout).

// Context is a session's context fill: Tokens of the model ModelID (of
// ProviderID) — the newest assistant message's that reported usage.
type Context struct {
	Tokens     int64
	ModelID    string
	ProviderID string
}

// ContextOf is the context fill of msgs (a session's messages, oldest first,
// as Client.Messages returns them), false when no assistant message reported
// usage.
func ContextOf(msgs []Message) (Context, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Type != "assistant" || m.Tokens == nil {
			continue
		}
		n := m.Tokens.Input + m.Tokens.Cache.Read + m.Tokens.Cache.Write
		if n <= 0 {
			continue
		}
		c := Context{Tokens: n}
		if m.Model != nil {
			c.ModelID, c.ProviderID = m.Model.ID, m.Model.ProviderID
		}
		return c, true
	}
	return Context{}, false
}

// ContextLimit is the context window of model id of provider in models (GET
// /api/model's limit.context), 0 when the list does not name it or names no
// limit.
func ContextLimit(models []Model, provider, id string) int64 {
	if id == "" {
		return 0
	}
	for _, m := range models {
		if m.ProviderID == provider && (m.ID == id || m.ModelID == id) && m.Limit.Context > 0 {
			return m.Limit.Context
		}
	}
	return 0
}
