package api

import (
	"context"
	"sync"
)

// The chat session a chief's turn runs in, for the owner card it raises
// (raiseOwnerCard stores it, attention_delivery delivers the answer there).
//
// Two doors reach a tool call: an in-process turn's dispatch, whose ctx is the
// turn's own (withTurnSession), and a claude-code chief's CLI calling /mcp on a
// request of its own, which carries no session — for that one, the session of
// the turn muster is running for the agent at that moment (turnSessions, set
// by chatTurn for the turn's whole run).

type turnSessionKey struct{}

// withTurnSession marks ctx as a turn in chat session key.
func withTurnSession(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, turnSessionKey{}, key)
}

// turnSessionRegistry is agent id -> the session keys of its running turns
// (the latest begun answers, when two overlap).
type turnSessionRegistry struct {
	mu   sync.Mutex
	runs map[int64][]string
}

// begin records agent's turn in key until the returned func is called.
func (r *turnSessionRegistry) begin(agent int64, key string) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = map[int64][]string{}
	}
	r.runs[agent] = append(r.runs[agent], key)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		l := r.runs[agent]
		for i := len(l) - 1; i >= 0; i-- {
			if l[i] == key {
				l = append(l[:i:i], l[i+1:]...)
				break
			}
		}
		if len(l) == 0 {
			delete(r.runs, agent)
		} else {
			r.runs[agent] = l
		}
	}
}

func (r *turnSessionRegistry) current(agent int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.runs[agent]; len(l) > 0 {
		return l[len(l)-1]
	}
	return ""
}

// turnSessionOf is the chat session key agent's tool call runs in: ctx's turn,
// else the turn muster is running for it now, else "" (unknown: the answer
// goes to its default session).
func (s *Server) turnSessionOf(ctx context.Context, agent int64) string {
	if k, _ := ctx.Value(turnSessionKey{}).(string); k != "" {
		return k
	}
	return s.turnSessions.current(agent)
}
