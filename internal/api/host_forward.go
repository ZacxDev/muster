package api

import (
	"context"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
)

// ---------------------------------------------------------------------------
// FORWARDING INTO A HOST-SPAWNED AGENT (docs/design/chief-dispatch.md 3.10,
// decision 9): task_forward reaches a claude window the chief started on the
// task, as a CHIEF-FORWARD send-queue row — the queue's existing pattern for
// messages the chief causes (a nudge, a route), not a new paste path.
//
//   - ADDRESSED BY THE DISPATCH RECORD: a running host_dispatches row of the
//     task, whose window the hub lists NOW carrying that dispatch, running its
//     runtime, with the session id the row recorded (hostForwardReachable).
//   - GATED AT DELIVERY (sendQueueChiefForwardGate): at most 10 minutes old,
//     the row still running, its window and session still matching.
//   - 🔴 BOUNDED ON THE HOST: the paste carries the dispatch
//     (mirror.ForDispatch), and the host types it only into the window whose
//     OWN @muster-dispatch equals it, at most 12 times an hour. Everything
//     here decides only whether and when to ask.
//   - IDLE ONLY, with no new code: the dispatcher sends a row only to an idle
//     window, and ExpectTurn refuses a paste whose turn moved.
// ---------------------------------------------------------------------------

// chiefForwardMaxAge is a chief-forward row's own age limit: a forward
// describes the task as it was, so an old one is stale.
const chiefForwardMaxAge = 10 * time.Minute

// chiefForwardStale is the detail a too-old forward ends with.
const chiefForwardStale = "Not sent: this forward is more than 10 minutes old, and the task may have changed since; the chief can forward it again."

// hostForwardReachable reports whether host dispatch r can be forwarded to
// NOW, by the hub's state: its host connected, interactive and spawning; its
// latest listing has a window whose id is the row's, whose dispatch is the
// row's, running the row's runtime, with the row's session id.
func (s *Server) hostForwardReachable(ctx context.Context, r plugins.HostDispatch) bool {
	if s.ext.Mirror == nil || r.Status != plugins.HostDispatchRunning || r.Window == "" || r.AgentSession == "" {
		return false
	}
	for _, v := range s.ext.Mirror.Hosts(ctx) {
		if v.Host != r.Host || !hostSpawnable(v, r.Runtime) {
			continue
		}
		for _, w := range v.Windows {
			if w.ID == r.Window && w.Dispatch == r.ID && spawnRuntimeRuns(r.Runtime, w) &&
				(w.ClaudeSession == r.AgentSession || w.AgentSession == r.AgentSession) {
				return true
			}
		}
	}
	return false
}

// hostForwardTarget is the newest running host dispatch on taskID that can be
// reached now (hostForwardReachable); ok false when none can. A dispatch with
// no task never appears: it is not on any task.
func (s *Server) hostForwardTarget(ctx context.Context, taskID int64) (plugins.HostDispatch, bool) {
	st := s.hostDispatches()
	if st == nil {
		return plugins.HostDispatch{}, false
	}
	rows, err := st.RunningHostDispatchesForTask(ctx, taskID)
	if err != nil {
		s.logger.Printf("task routing: host dispatches on task %d: %v", taskID, err)
		return plugins.HostDispatch{}, false
	}
	for _, r := range rows { // newest first
		if s.hostForwardReachable(ctx, r) {
			return r, true
		}
	}
	return plugins.HostDispatch{}, false
}

// forwardToHostDispatch queues task n's forward (text) for host dispatch r as a
// chief-forward send-queue row. 🔴 THE ONLY CODE THAT SETS
// ChiefForwardDispatch (a ledger test pins it).
func (s *Server) forwardToHostDispatch(ctx context.Context, n notes.Note, r plugins.HostDispatch, text string) string {
	if s.ext.SendQueue == nil {
		return toolErr("this server has no send queue, so it cannot forward to an agent on a host")
	}
	it := sendqueue.Item{Target: sendqueue.Target{Host: r.Host, Session: mirror.AgentSpawnSession, Window: r.Window},
		Body: text, StepUpAt: s.stepUpNow(), ChiefForwardDispatch: r.ID}
	q, err := s.ext.SendQueue.Enqueue(ctx, it)
	if err != nil {
		s.logger.Printf("task routing: queue a forward for host dispatch %d: %v", r.ID, err)
		return toolErr("the forward could not be queued for the agent on %s", r.Host)
	}
	s.recordTaskRouted(ctx, n, taskForwardTool)
	s.logger.Printf("task routing: task %d forward queued for host dispatch %d (%s %s, row %d)", n.ID, r.ID, r.Host, r.Window, q.ID)
	return toolJSON(map[string]any{"task_id": n.ID,
		"forwarded_to": map[string]any{"host": r.Host, "window": r.Window, "dispatch": r.ID},
		"next": "queued for the agent on task " + strconv.FormatInt(n.ID, 10) + "; muster delivers it once that agent is idle. " +
			"Do not dispatch another agent for this task."})
}

// sendQueueChiefForwardGate is a chief-forward row's gate at delivery: "" to
// deliver, else why not (the row ends failed). It runs as the row's owner.
func (s *Server) sendQueueChiefForwardGate(ctx context.Context, it sendqueue.Item) string {
	if s.sendQueueNow().Sub(it.CreatedAt) > chiefForwardMaxAge {
		return chiefForwardStale
	}
	st := s.hostDispatches()
	if st == nil {
		return "Not sent: this server records no host dispatches."
	}
	r, err := st.HostDispatchByID(ctx, it.ChiefForwardDispatch)
	if err != nil {
		return "Not sent: the agent this forward was for is no longer recorded."
	}
	if r.Status != plugins.HostDispatchRunning {
		return "Not sent: the agent this forward was for has ended (" + firstNonBlank(r.EndedWhy, r.Status) + ")."
	}
	if r.Host != it.Target.Host || r.Window != it.Target.Window || !s.hostForwardReachable(ctx, r) {
		return "Not sent: the agent's window is not the one this forward was queued for any more."
	}
	return ""
}

// withForwardAuth is ctx's auth time moved to the delivery moment, as the nudge
// and route rows do: the gate, not a sign-in, authorises a chief forward.
func (s *Server) withForwardAuth(ctx context.Context) context.Context {
	return stepup.WithAuthTime(ctx, s.stepUpNow())
}
