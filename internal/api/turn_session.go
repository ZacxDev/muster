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

// THE TURN'S OWN INPUT: the chat_messages row a turn was started for (the
// owner's line runEnteredTurn stored), so the owner's view line a tool call
// answers is read relative to THAT row, not to whatever the newest row is
// (ownerViewLine). The same two doors: an in-process dispatch reads its turn's
// ctx (withTurnInput); a CLI's /mcp call reads the registry, which answers
// only while exactly ONE turn runs for the agent and that turn's input is
// known — two overlapping turns (one waiting out the other's busy, a delivery
// beside a typed turn) cannot be told apart, so they answer 0.

type turnInputKey struct{}

// withTurnInput marks ctx as the turn whose input is chat message id.
func withTurnInput(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, turnInputKey{}, id)
}

// turnInputFrom is ctx's turn input, 0 for none.
func turnInputFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(turnInputKey{}).(int64)
	return id
}

// turnInputRegistry is agent id -> the inputs of its running turns (0 for a
// turn whose input muster did not store first: a delivery, a nudge).
type turnInputRegistry struct {
	mu   sync.Mutex
	runs map[int64][]int64
}

// begin records agent's turn with input id until the returned func is called.
func (r *turnInputRegistry) begin(agent, id int64) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = map[int64][]int64{}
	}
	r.runs[agent] = append(r.runs[agent], id)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		l := r.runs[agent]
		for i := len(l) - 1; i >= 0; i-- {
			if l[i] == id {
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

// only is the input of agent's one running turn; 0 when none runs, more than
// one does, or its input is unknown.
func (r *turnInputRegistry) only(agent int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.runs[agent]; len(l) == 1 {
		return l[0]
	}
	return 0
}

// turnInputOf is the input row of the turn agent's tool call runs in: ctx's
// turn, else the one turn muster is running for it now, else 0 (unknown).
func (s *Server) turnInputOf(ctx context.Context, agent int64) int64 {
	if id := turnInputFrom(ctx); id > 0 {
		return id
	}
	return s.turnInputs.only(agent)
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
