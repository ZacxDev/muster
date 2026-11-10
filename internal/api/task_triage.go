package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// THE TASK TRIAGE (docs/plugins.md#an-assigned-task-muster-places-itself): a
// "task assigned" event muster can place deterministically is placed WITHOUT a
// chief turn, by the chief's own tools, and only an ambiguous one reaches the
// chief.
//
// The chief's turn for such an event is formulaic: look for the session or
// agent already on the task (the session hint its turn carries, focus_search),
// hand the task there when one is clearly on it, and otherwise put it on the
// owner's board (task_route_attention: the "Task #N needs you" ask, with the
// Release & dispatch buttons on a gated task). muster does exactly that, with
// the SAME calls — toolTaskForward, toolRouteToSession, toolTaskRouteAttention,
// as the chief — so the result is the chief's own route, forward or ask,
// byte for byte; and with the SAME evidence the chief's turn would be shown —
// the agents bound to the task (forwardTargets) and the turn's session hint
// (pluginSessionHints).
//
// 🔴 STRONG, DETERMINISTICALLY AND CONSERVATIVELY (decideTaskTriage):
//
//   - FORWARD: exactly one agent task_forward can reach is bound to the task,
//     no other live agent is, and the hint names no session at all;
//   - ROUTE: no agent is bound, and the hint names exactly one session, by a
//     STRONG link — a pull request or branch the task names that muster
//     recorded that session creating or pushing (session_prs), or the task
//     naming the repository only that one online session reports — and names
//     no other session, strong or weak (a focus-title match is weak);
//   - BOARD: no agent is bound and the hint names no session at all.
//
// Everything else is the chief's, exactly as before: two candidates, a weak
// match, a live agent that cannot be sent to, a gated task with a candidate
// (only its owner can release it), no chief, a chief turned off, or any of the
// three calls refusing. A session the task merely links (task_sessions: one
// that read it) is NOT strong and is not consulted.
//
// The event is then settled by muster ("muster: task triage — …") and logged
// in the chief's chat with muster's note (Deliverer.Handed): the chief holds it
// for its next turn, at no turn's cost. Every outcome is a handling row for the
// usage page: task_forward, task_route, task_board, or chief.
// ---------------------------------------------------------------------------

// Task triage decisions.
const (
	triageForward = "forward"
	triageRoute   = "route"
	triageBoard   = "board"
	triageChief   = "chief"
)

// triageFacts is what a task triage decides on: the gate, the agents bound to
// the task (forward: ones task_forward can reach; stuck: live ones it cannot),
// and the session hint the chief's turn would carry.
type triageFacts struct {
	gated          bool
	forward, stuck int
	hints          []plugins.SessionHint
}

// triageDecision is a triage's outcome: its kind and, for a route, the session.
type triageDecision struct {
	kind, subject string
}

// strongHint reports whether h names its session by a STRONG link: a mirrored
// session listed by a pull request or branch it was recorded creating or
// pushing, or by its repository's name. A session listed by focus titles only
// is weak.
func strongHint(h plugins.SessionHint) bool {
	return strings.HasPrefix(h.ID, "mirror:") && (len(h.PRs) > 0 || h.ByRepo)
}

// decideTaskTriage is THE triage rule (see the banner): pure, so its cases are
// pinned one by one.
func decideTaskTriage(f triageFacts) triageDecision {
	chief := triageDecision{kind: triageChief}
	if f.stuck > 0 || f.forward > 1 {
		return chief
	}
	if f.forward == 1 {
		if len(f.hints) == 0 && !f.gated {
			return triageDecision{kind: triageForward}
		}
		return chief
	}
	var strong []plugins.SessionHint
	for _, h := range f.hints {
		if !strongHint(h) {
			return chief // a weak match: the chief judges it
		}
		strong = append(strong, h)
	}
	switch {
	case len(strong) == 0:
		return triageDecision{kind: triageBoard}
	case len(strong) == 1 && !f.gated:
		return triageDecision{kind: triageRoute, subject: strong[0].ID}
	}
	return chief
}

// triageAssignedTask places NEW task event m (an assignment of task n, through
// plugin p) by the triage rule, reporting whether it did; false leaves it for
// the chief, untouched. ctx acts as the owner (the plugin's principal), without
// the request's cancellation.
func (s *Server) triageAssignedTask(ctx context.Context, p plugins.Plugin, n notes.Note, m plugins.Message) bool {
	// Only where the chief would take a turn for it now, and with what the
	// board card needs.
	if s.pluginDeliverer == nil || s.ext.Attention == nil || s.ext.Agents == nil {
		return false
	}
	settler, ok := s.ext.Plugins.(interface {
		SettleQueuedTaskEventAsMuster(ctx context.Context, chiefID, ownerID, messageID int64, reason, via string) error
	})
	if !ok {
		return false
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil || chief.ID == 0 || agentOwnerID(chief) != n.OwnerID || !s.chiefActing(ctx, chief) {
		return false
	}
	facts, ok := s.triageFactsFor(ctx, chief, n, m)
	if !ok {
		return false
	}
	d := decideTaskTriage(facts)
	var out, handler, did string
	switch d.kind {
	case triageForward:
		out, handler = s.toolTaskForward(ctx, chief, n.ID, ""), chiefusage.HandlerTaskForward
		did = "forwarded it to the agent working the task"
	case triageRoute:
		args, _ := json.Marshal(map[string]any{"task_id": n.ID, "subject": d.subject})
		out, handler = s.toolRouteToSession(ctx, chief, string(args)), chiefusage.HandlerTaskRoute
		did = "routed it to " + d.subject + ", the one session on it"
	case triageBoard:
		out, handler = s.toolTaskRouteAttention(ctx, chief, n.ID, ""), chiefusage.HandlerTaskBoard
		did = "put it in front of your owner (no session or agent is on it)"
	default:
		s.logger.Printf("task triage: task %d: ambiguous; to the chief", n.ID)
		return false
	}
	if toolRefused(out) {
		// The call refused (a window gone between the hint and the route, the
		// queue full, …): nothing was done, so the chief decides, as before.
		s.logger.Printf("task triage: task %d: the %s was refused (%s); to the chief", n.ID, d.kind, out)
		return false
	}
	reason := plugins.SettledByMuster + "task triage — " + did
	if err := settler.SettleQueuedTaskEventAsMuster(ctx, chief.ID, n.OwnerID, m.ID, reason, "task-triage"); err != nil {
		// Done, but another delivery took the event meanwhile: the chief holds
		// it and finds the task already routed.
		s.logger.Printf("task triage: task %d: settle event %d: %v", n.ID, m.ID, err)
	} else if cur, err := s.ext.Plugins.Message(ctx, m.ID); err == nil {
		note := "[muster] task event message_id " + strconv.FormatInt(m.ID, 10) + " (task #" + strconv.FormatInt(n.ID, 10) +
			" assigned): muster " + did + ", as you would have. No turn was spent on it; nothing to do."
		if err := s.pluginDeliverer.Handed(ctx, p, chief, cur, note); err != nil {
			s.logger.Printf("task triage: task %d: record it in the chief's conversation: %v", n.ID, err)
		}
	}
	s.recordHandling(ctx, p, m, handler, 0, 0)
	s.logger.Printf("task triage: task %d: %s by muster", n.ID, d.kind)
	return true
}

// triageFactsFor reads the triage's facts for task n's event m as chief's turn
// would see them; false when they cannot be read (the chief decides then).
func (s *Server) triageFactsFor(ctx context.Context, chief agents.Agent, n notes.Note, m plugins.Message) (triageFacts, bool) {
	var f triageFacts
	_, f.gated = notes.GateReason(n.Tags)
	targets, err := s.forwardTargets(ctx, chief, n.ID)
	if err != nil {
		s.logger.Printf("task triage: task %d: agents on it: %v", n.ID, err)
		return f, false
	}
	f.forward = len(targets)
	if _, ok := s.hostForwardTarget(ctx, n.ID); ok {
		f.forward++
	}
	on, err := s.liveOnTask(ctx, n.OwnerID, n.ID)
	if err != nil {
		return f, false
	}
	for _, a := range on {
		if a.ID != chief.ID && agentLive(a.Status) && !agentCanBeSent(a.Status) {
			f.stuck++
		}
	}
	if hint := s.pluginHints(s.ext); hint != nil {
		f.hints = hint(ctx, chief, m).Sessions
	}
	return f, true
}

// toolRefused reports whether a tool's answer is a refusal ({"error": …}).
func toolRefused(out string) bool {
	var v struct {
		Error any `json:"error"`
	}
	return json.Unmarshal([]byte(out), &v) != nil || v.Error != nil
}
