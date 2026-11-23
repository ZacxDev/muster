// Package itemorigin carries, on a context, WHERE a needs-you row is being
// made: the officer instance whose tool call is running, its chat session and
// the moment in it (migration 0184's origin_* columns on attention_asks and
// plugin_reply_drafts).
//
// 🔴 ONLY THE OFFICER TOOL DISPATCHER INSTALLS ONE (internal/api
// officerToolCall), FROM ITS BOUND CALLER — the agent whose token made the
// call — and never from anything in the call's arguments. The stores that
// write the columns (attention.PGStore.Create, plugins.PGStore's outbound
// draft) read it from the context and nowhere else, so a value a caller
// supplies cannot reach them: there is no field to put one in.
//
// It is a RESOLVER, run once at the insert, so a tool call that makes no such
// row costs no read.
package itemorigin

import (
	"context"
	"sync"
	"time"
)

// Origin is where a row was made.
type Origin struct {
	// AgentID is the officer instance (agents.id).
	AgentID int64
	// SessionID is its chat session (chat_sessions.id) the call was made in;
	// 0 when it has none.
	SessionID int64
	// MessageID is the newest chat_messages row of that session when the
	// call was made: the moment; 0 when the session had none yet.
	MessageID int64
	// At is when the call was made.
	At time.Time
}

// Resolver answers the origin, ok false for none.
type Resolver func(ctx context.Context) (Origin, bool)

type key struct{}

// once caches a resolver's answer for one context: a call that makes two
// rows records one moment for both.
type once struct {
	mu   sync.Mutex
	done bool
	o    Origin
	ok   bool
	fn   Resolver
}

// With is ctx carrying r: the origin of every row made under it.
func With(ctx context.Context, r Resolver) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, &once{fn: r})
}

// Resolve is ctx's origin, resolved at most once per With; ok false when ctx
// carries none, or its resolver found none (no agent).
func Resolve(ctx context.Context) (Origin, bool) {
	c, _ := ctx.Value(key{}).(*once)
	if c == nil {
		return Origin{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.o, c.ok = c.fn(ctx)
		c.done = true
	}
	if !c.ok || c.o.AgentID <= 0 {
		return Origin{}, false
	}
	return c.o, true
}

// Args is o as the four column values an INSERT binds (origin_agent_id,
// origin_session_id, origin_message_id, origin_at): NULLs for none.
func Args(o Origin, ok bool) [4]any {
	if !ok || o.AgentID <= 0 {
		return [4]any{nil, nil, nil, nil}
	}
	out := [4]any{o.AgentID, nil, nil, o.At.UTC()}
	if o.SessionID > 0 {
		out[1] = o.SessionID
		if o.MessageID > 0 {
			out[2] = o.MessageID
		}
	}
	if o.At.IsZero() {
		out[3] = nil
	}
	return out
}
