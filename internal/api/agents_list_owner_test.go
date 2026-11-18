package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
)

// leakyAgents is an agent store whose List returns every row it holds, the
// caller's owner's or not: a store that does NOT apply row-level security, so
// agents_list's own owner comparison is the only thing between another owner's
// agent and the answer.
type leakyAgents struct {
	agents.Store
	rows []agents.Agent
}

func (l leakyAgents) List(context.Context) ([]agents.Agent, error) { return l.rows, nil }

// TestAgentsListDropsAnotherOwnersRowTheStoreReturned: with a store that
// returns another owner's agent (row-level security bypassed), agents_list
// answers only the caller's owner's — with and without worker_pool, in a pool
// both agents share — so the owner comparison holds on its own. (The pg test's
// fixture cannot show this: RLS already hides the other owner's row there.)
func TestAgentsListDropsAnotherOwnersRowTheStoreReturned(t *testing.T) {
	const owner, other = int64(7), int64(8)
	mine := agents.Agent{ID: 101, Name: "mine-w1", OwnerID: owner, Kind: agents.KindClaudeCode, WorkerPool: "shared-pool",
		Status: agents.StatusRunning}
	theirs := agents.Agent{ID: 202, Name: "theirs-w1", OwnerID: other, Kind: agents.KindClaudeCode, WorkerPool: "shared-pool",
		Status: agents.StatusRunning}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: leakyAgents{rows: []agents.Agent{theirs, mine}}})
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
	// A per-session host's session: a mirrored session with no window, which
	// ownersOwnSession lets through without a mirror hub.
	c := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: "sess-1"}}
	for _, args := range []string{`{}`, `{"worker_pool":"shared-pool"}`} {
		text := s.toolAgentsList(ctx, c, args)
		var out agentsListOut
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("agents_list %s answered %s", args, text)
		}
		if out.Count != 1 || len(out.Agents) != 1 || out.Agents[0].ID != mine.ID {
			t.Errorf("agents_list %s answered %s; want only %s (not the other owner's %s)", args, text, mine.Name, theirs.Name)
		}
	}
}
