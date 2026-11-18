package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// agent_destroy: THE OPERATOR TEARS DOWN ONE OF THEIR AGENTS FROM THEIR OWN
// MIRRORED CLAUDE CODE SESSION.
//
// Operator decision ("Build agent_destroy MCP": "Mirrored owner sessions only,
// same pattern; tears down the pod via the existing lifecycle path, keeps the
// task"), on attention_answer's pattern:
//
//   - LISTED ONLY on the mirror door (mirror_mcp.go's mirrorTools), never in the
//     agent registry, so POST /mcp (a dispatched agent, the chief included) does
//     not offer it and refuses it by name as unknown;
//   - 🔴 REFUSED IN ITS HANDLER to every caller that is not a mirrored session
//     (toolCaller.mirroredSession): dispatchCallerTool is reached by a gateway
//     agent naming ANY tool (dispatchAgentTool's default case), the chief
//     included.
//
// It takes the browser's own teardown (destroyAgent, which handleAgentDelete
// calls too): the chief is refused there, then the provisioner's Destroy runs
// in the background — the instance, then the row — and, after a successful
// teardown and on BOTH doors, the chief's start on the task is released when
// the chief's start made this agent (releaseChiefStart) and the owner's open
// tabs are told (agent.changed, attention_queue.changed). What the row's
// deletion takes with it is the browser path's: the agent's open attention asks
// are deleted (0025's ON DELETE CASCADE) and a turn in flight is not stopped
// first — the pod's teardown ends it.
//
// ⚠ NO AUDIT ROW. The browser's Delete writes none, and an MCP destroy writes
// none either (operator decision): its provenance — the host the mirror token
// proves and the session the caller named — is in muster's server log only.
//
// The one thing only this door does: the TASK IS KEPT and, once the teardown
// has succeeded, moved back from in_progress to open when no other live agent
// is on it (reopenTaskAfterDestroy) — the state agent_dispatch needs
// (chiefDispatchTaskRefusal: open, ungated, no live agent).
// ---------------------------------------------------------------------------

const agentDestroyTool = "agent_destroy"

// agentDestroyMirrorOnly is agent_destroy's refusal of a caller that is not a
// mirrored session.
const agentDestroyMirrorOnly = "agent_destroy is available only to your owner's own mirrored Claude Code sessions"

// agentDestroyToolDef is the mirrored sessions' agent_destroy.
func agentDestroyToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: agentDestroyTool,
		Description: "Destroy one of your owner's agents ON YOUR OWNER'S BEHALF — use it only when your owner asked you to. It is the same " +
			"teardown as Delete on muster's Agents page: the agent's pod and record are removed in the background, its open attention asks " +
			"go with it, and a turn it is running is cut off. The task it worked is KEPT: once the agent is gone, a task still in_progress " +
			"with no other live agent goes back to open, and if your owner's chief started this agent, the chief may start one new agent " +
			"on that task again. Name the agent by EXACTLY ONE of: name (its slug, as `muster agent ls` shows it), id, or host_dispatch " +
			"(an agent your owner's chief started in a window on one of their hosts: that ENDS its record and frees the chief's slot, " +
			"but does not close the window — close it on the host). The chief cannot be destroyed this way.",
		Parameters: objSchema(map[string]any{
			"name": strProp("The agent's name (slug), as `muster agent ls` shows it. Give this OR id."),
			"id":   intProp("The agent's id. Give this OR name.", 1, 1<<53),
			"host_dispatch": intProp("A host dispatch's id (an agent the chief started on one of your owner's hosts): its record "+
				"ends and its slot is freed; the window keeps running until it is closed on the host.", 1, 1<<53),
		})}
}

// toolAgentDestroy is agent_destroy. Another owner's agent and a missing one
// read the same ("no such agent").
func (s *Server) toolAgentDestroy(ctx context.Context, c toolCaller, arguments string) string {
	if why := s.ownersOwnSession(ctx, c); why != "" {
		if why == notAMirroredSession {
			return toolErr("%s", agentDestroyMirrorOnly)
		}
		return toolErr("%s", why)
	}
	var in struct {
		Name         *string `json:"name"`
		ID           *int64  `json:"id"`
		HostDispatch *int64  `json:"host_dispatch"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("arguments are {name or id or host_dispatch}")
	}
	given := 0
	for _, set := range []bool{in.Name != nil, in.ID != nil, in.HostDispatch != nil} {
		if set {
			given++
		}
	}
	if given != 1 {
		return toolErr("name the agent by exactly one of name (its slug), id or host_dispatch, not several and not none")
	}
	if in.HostDispatch != nil {
		return s.destroyHostDispatch(ctx, c, *in.HostDispatch)
	}
	// 🔴 THE PROVISIONER BARRIER FOR THE MCP DOORS: they are not lifecycle
	// routes (requireLifecycleProvisioner), so this body — which also reaches
	// the provisioner, through the destroy closure below — is the nil check
	// provisioner_seam_test.go's walk looks for.
	if s.ext.Provisioner == nil || s.ext.Agents == nil {
		return toolErr("this deployment has no agent provisioner wired, so it cannot destroy an agent")
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return toolErr("the agent could not be read")
	}
	var (
		a     agents.Agent
		err   error
		label string
	)
	if in.ID != nil {
		label = strconv.FormatInt(*in.ID, 10)
		if *in.ID <= 0 {
			return toolErr("no such agent: %s", label)
		}
		a, err = s.ext.Agents.Get(ctx, *in.ID)
	} else {
		label = strings.TrimSpace(*in.Name)
		if label == "" {
			return toolErr("no such agent: %q", label)
		}
		a, err = s.ext.Agents.GetByName(ctx, label)
	}
	switch {
	// Row-level security already hides another owner's agent; the owner
	// comparison is the same rule stated again.
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && a.OwnerID != p.UserID):
		return toolErr("no such agent: %s", label)
	case err != nil:
		s.logger.Printf("agent_destroy: read %s: %v", label, err)
		return toolErr("the agent could not be read")
	}
	from := c.asker.MirrorHost + "/" + c.asker.MirrorSession
	var chief errDestroyChief
	destroy := func(ctx context.Context, id int64) error { return s.ext.Provisioner.Destroy(ctx, id) }
	_, err = s.destroyAgent(ctx, a.ID, destroy, destroyHooks{done: s.reopenTaskAfterDestroy})
	switch {
	case err == nil:
	case errors.As(err, &chief):
		return toolErr("%s; to replace the chief use `muster chief replace`", chief.Error())
	default:
		s.logger.Printf("agent_destroy: %d (%s) via MCP from %s: %v", a.ID, a.Name, from, err)
		return toolErr("nothing was destroyed: could not check whether this is the chief agent")
	}
	// The ONLY record of where an MCP destroy came from (no audit row).
	s.logger.Printf("agents: destroy of %d (%s) started by the operator via MCP from %s", a.ID, a.Name, from)
	out := map[string]any{"agent_id": a.ID, "agent": a.Name, "destroying": true}
	next := "The teardown runs in the background: the agent is gone from muster when its pod and record are. " +
		"If the teardown fails the agent stays, with status error."
	if a.NoteID != nil {
		out["task_id"] = *a.NoteID
		next += " Its task is kept; once the agent is gone, if the task is still in_progress and no other agent is live on it, it goes back to open."
	}
	out["next"] = next
	return toolJSON(out)
}

// releaseChiefStart is the shared after-step of an owner's destroy of agent a
// (both doors, destroyAgent): when the chief's latest start on a's task made
// a, that start is released, so the chief may start ONE new agent on the task
// (plugins.ReleaseChiefDispatch; operator decision "Owner's destroy resets the
// chief's allowance"). An agent with no task, a deployment with no
// chief-dispatch ledger, or an agent the chief did not start release nothing.
// Best-effort: a failure is logged and the task keeps its one start (the
// fail-closed direction).
func (s *Server) releaseChiefStart(ctx context.Context, a agents.Agent) {
	if a.ID == 0 || a.NoteID == nil || s.ext.ChiefDispatch == nil {
		return
	}
	released, err := s.ext.ChiefDispatch.ReleaseChiefDispatch(ctx, *a.NoteID, a.ID)
	if err != nil {
		s.logger.Printf("agents: release the chief's start on task %d after destroying %d: %v", *a.NoteID, a.ID, err)
		return
	}
	if released {
		s.logger.Printf("agents: destroying %d released the chief's start on task %d", a.ID, *a.NoteID)
	}
}

// reopenTaskAfterDestroy moves destroyed agent a's task back from in_progress
// to open when no other live agent is on it, so the chief's agent_dispatch can
// start a new one (chiefDispatchTaskRefusal wants it open). A task in any other
// state (open already, ready_for_review, complete) is left as it is. Best-effort:
// a failure is logged and the task stays in_progress, as the browser path leaves
// it.
//
// 🔴 THE WRITE IS CONDITIONAL (notes.OnlyFrom): the UPDATE moves the row only
// while it is still in_progress, so a status change landing between the read
// below and the write is never overwritten — the write answers
// notes.ErrStatusMoved and the task keeps its new status.
func (s *Server) reopenTaskAfterDestroy(ctx context.Context, a agents.Agent) {
	if a.NoteID == nil || s.ext.Notes == nil {
		return
	}
	n, err := s.ext.Notes.Get(ctx, *a.NoteID)
	if err != nil || n.Status != notes.StatusInProgress {
		return
	}
	// 🔴 A PLUGIN TASK REFERENCE IS NEVER REOPENED HERE. Its status is the
	// upstream's (task_ref_queue.go), and on a reference this write would be
	// QUEUED for the upstream: tearing down muster's agent would move the
	// UPSTREAM task back to open — a side effect of tidying this board, which
	// is the same reason a reference is never a merge loser (merge.go). Someone
	// may be working it upstream; the plugin's next snapshot keeps the truth.
	if n.IsReference() {
		return
	}
	on, err := s.liveOnTask(ctx, a.OwnerID, n.ID)
	if err != nil {
		s.logger.Printf("agent_destroy: agents on task %d: %v; leaving it in_progress", n.ID, err)
		return
	}
	for _, x := range on {
		if x.ID != a.ID && agentLive(x.Status) {
			return
		}
	}
	// A host-spawned agent still working the task keeps it in_progress too
	// (design 2.6: "a live agent on this task" includes a running host
	// dispatch).
	if st := s.hostDispatches(); st != nil {
		hs, err := st.RunningHostDispatchesForTask(ctx, n.ID)
		if err != nil {
			s.logger.Printf("agent_destroy: host dispatches on task %d: %v; leaving it in_progress", n.ID, err)
			return
		}
		if len(hs) > 0 {
			return
		}
	}
	hooks := reopenTestHooks.Load()
	if hooks != nil && hooks.beforeWrite != nil {
		hooks.beforeWrite(n.ID)
	}
	_, err = s.setNoteStatus(notes.OnlyFrom(ctx, notes.StatusInProgress), writerDestroyReopen, n.ID, notes.StatusOpen)
	if hooks != nil && hooks.afterWrite != nil {
		hooks.afterWrite(n.ID, err)
	}
	if errors.Is(err, errQueuedUpstream) {
		// The row became a reference between the read above and this write:
		// queued, which broadcast already. (Only that window reaches here.)
		return
	}
	if err != nil {
		s.logger.Printf("agent_destroy: reopen task %d: %v", n.ID, err)
		return
	}
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(n.ID, 10))
}

// reopenHooks are test-only seams around reopenTaskAfterDestroy's write:
// beforeWrite runs between its read and its write (the window the conditional
// write closes), afterWrite with the write's result.
type reopenHooks struct {
	beforeWrite func(taskID int64)
	afterWrite  func(taskID int64, err error)
}

// reopenTestHooks holds the hooks (nil in production). 🔴 ATOMIC, because it
// is read by destroyAgent's background goroutine — possibly one a previous
// test started — while a test stores or clears it.
var reopenTestHooks atomic.Pointer[reopenHooks]

// destroyHostDispatch is agent_destroy's host_dispatch: the OWNER'S RECOVERY for
// a host dispatch (design 3.7). A pending or running row of the owner's ends
// "ended by owner", which frees its live slot and releases its task's start
// at once. The window is NOT closed (there is no kill message, D7): the answer
// says so. Another owner's row and a missing one read the same.
func (s *Server) destroyHostDispatch(ctx context.Context, c toolCaller, id int64) string {
	p, ok := authz.From(ctx)
	st := s.hostDispatches()
	if !ok || p.UserID <= 0 || st == nil {
		return toolErr("no such host dispatch: %d", id)
	}
	r, err := st.HostDispatchByID(ctx, id)
	if err != nil || r.Owner != p.UserID {
		return toolErr("no such host dispatch: %d", id)
	}
	if r.Status == plugins.HostDispatchEnded {
		return toolErr("host dispatch %d has already ended (%s)", id, r.EndedWhy)
	}
	if _, err := s.endHostDispatch(ctx, p.UserID, id, "ended by owner", false); err != nil {
		s.logger.Printf("agent_destroy: end host dispatch %d: %v", id, err)
		return toolErr("host dispatch %d could not be ended; try again", id)
	}
	s.logger.Printf("agents: host dispatch %d ended by the operator via MCP from %s/%s", id, c.asker.MirrorHost, c.asker.MirrorSession)
	return toolJSON(map[string]any{"host_dispatch": id, "ended": true,
		"next": "ended host dispatch " + strconv.FormatInt(id, 10) + "; its window, if any, is still open on " + r.Host + ": close it there"})
}
