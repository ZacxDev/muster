package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// ---------------------------------------------------------------------------
// agents_list: THE OWNER READS THEIR OWN AGENTS FROM THEIR OWN MIRRORED CLAUDE
// CODE SESSION — what `muster agent ls` (GET /api/agents) shows, from MCP: so
// a session that dispatched a worker with dispatch_request, or is about to
// agent_destroy one, can see the owner's agents, their kind, worker pool and
// Claude account, without a shell.
//
// Read-only. The SAME store read as GET /api/agents (Agents.List, under the
// caller's row-level security), and the SAME scoping as agent_destroy: only
// the owner's own mirrored sessions (ownersOwnSession), and only agents whose
// owner is the caller's owner (the RLS answer, with the owner compared again,
// as agent_destroy does).
//
//   - LISTED ONLY on the mirror door (mirrorTools) and dispatched there by
//     name (handleMirrorMCP), never in dispatchCallerTool, so no dispatched
//     agent — the chief included — can name it;
//   - refused to a mirrored opencode session (not in openCodeMirrorTools), as
//     every owner-authority tool is.
// ---------------------------------------------------------------------------

const agentsListTool = "agents_list"

// agentsListMirrorOnly refuses a caller that is not one of the owner's
// mirrored sessions.
const agentsListMirrorOnly = "agents_list is available only to your owner's own mirrored Claude Code sessions"

// agentsListArgs is the arguments refusal.
const agentsListArgs = "arguments are {worker_pool?}"

func agentsListToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: agentsListTool,
		Description: "List your owner's agents (only theirs), newest first, as `muster agent ls` shows them: each one's name (its slug, " +
			"what agent_destroy takes), id, display name, kind (claude-code or gateway), worker pool (a machine-job worker's pool, " +
			"\"\" for none), Claude account (a claude-code agent's, \"\" otherwise), status (provisioning, running, stopped, error, …), " +
			"task id and when it was created. worker_pool filters to that pool's workers. Read-only.",
		Parameters: objSchema(map[string]any{
			"worker_pool": strProp("Only the agents in this worker pool. Omit it for all of your owner's agents."),
		})}
}

// agentsListItem is one agent in agents_list's answer: the fields a session
// needs to tell its owner's agents apart and name one (agentJSON's subset,
// under the same keys' meanings, snake_case like the other MCP answers).
type agentsListItem struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	Kind        string    `json:"kind"`
	WorkerPool  string    `json:"worker_pool"`
	CCAccount   string    `json:"cc_account"`
	Status      string    `json:"status"`
	TaskID      *int64    `json:"task_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// toolAgentsList is agents_list (see the header).
func (s *Server) toolAgentsList(ctx context.Context, c toolCaller, arguments string) string {
	if why := s.ownersOwnSession(ctx, c); why != "" {
		if why == notAMirroredSession {
			return toolErr("%s", agentsListMirrorOnly)
		}
		return toolErr("%s", why)
	}
	var in struct {
		WorkerPool string `json:"worker_pool"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(orEmptyJSON(arguments))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return toolErr("%s", agentsListArgs)
	}
	pool := strings.TrimSpace(in.WorkerPool)
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 || p.All() || s.ext.Agents == nil {
		return toolErr("your owner's agents could not be read")
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		s.logger.Printf("agents_list: list: %v", err)
		return toolErr("your owner's agents could not be read; try again later")
	}
	// A host officer instance's row is never provisioned and is seen as its
	// window (agents.ProvisionedOnly; the hostRowConsumers ledger).
	list = agents.ProvisionedOnly(list)
	out := make([]agentsListItem, 0, len(list))
	for _, a := range list {
		// Row-level security already hides another owner's agents; the owner
		// comparison is the same rule stated again (agent_destroy's).
		if a.OwnerID != p.UserID || (pool != "" && a.WorkerPool != pool) {
			continue
		}
		it := agentsListItem{ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, Kind: agents.ResolveKind(a.Kind),
			WorkerPool: a.WorkerPool, CCAccount: a.CCAccount, Status: a.Status, CreatedAt: a.CreatedAt.UTC()}
		if a.NoteID != nil {
			id := *a.NoteID
			it.TaskID = &id
		}
		out = append(out, it)
	}
	res := map[string]any{"agents": out, "count": len(out)}
	if pool != "" {
		res["worker_pool"] = pool
	}
	return toolJSON(res)
}
