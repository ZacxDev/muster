package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

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
//   - CLAIMED: a session or the task's upstream has CLAIMED the task
//     (notes.Claims: a session's explicit pickup, or a reference whose
//     snapshot names a claimant: agent, agent_session) and the forward above does
//     not apply: nothing is placed — no "needs you" card, no chief turn —
//     because someone is already on it;
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
// usage page: task_forward, task_route, task_board, task_claimed, or chief.
// ---------------------------------------------------------------------------

// The notes muster's triage passes where the chief passes its own: fixed,
// muster's words, so the card and the forward say who placed the task.
const (
	triageBoardNote   = "muster placed this itself: no agent or session is on this task, so it is yours to decide."
	triageForwardNote = "muster passed this on itself: the task was assigned upstream, and you are the agent working it."
	// triageClaimTTL bounds the chief's session claim the triage holds: its
	// three calls are server-side and short.
	triageClaimTTL = 2 * time.Minute
)

// Task triage decisions.
const (
	triageForward = "forward"
	triageRoute   = "route"
	triageBoard   = "board"
	triageClaimed = "claimed"
	triageChief   = "chief"
)

// triageFacts is what a task triage decides on: the gate, the agents bound to
// the task (forward: ones task_forward can reach; stuck: live ones it cannot),
// and the session hint the chief's turn would carry.
type triageFacts struct {
	gated          bool
	forward, stuck int
	hints          []plugins.SessionHint
	// claims is who claimed the task (notes.Claims); empty when nobody has.
	claims []notes.Claim
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
	if f.forward == 1 && f.stuck == 0 && len(f.hints) == 0 && !f.gated {
		return triageDecision{kind: triageForward}
	}
	// A claimed task needs no placing: someone is on it. Ahead of every case
	// but the clean forward (the agent working it still hears of it), so
	// neither a card nor a chief turn (which would card it) comes of its
	// assignment.
	if len(f.claims) > 0 {
		return triageDecision{kind: triageClaimed}
	}
	if f.stuck > 0 || f.forward > 0 {
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
	settler, ok := s.ext.Plugins.(plugins.PRReviewSettler)
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
	if d.kind == triageChief {
		s.logger.Printf("task triage: task %d: ambiguous; to the chief", n.ID)
		return false
	}
	// 🔴 CLAIM THE EVENT BEFORE ACTING, as every delivery to the chief does: the
	// chief's session claim (the one a delivery holds for its whole turn), then
	// the event itself, queued → delivering. A retry of the owner's queue
	// (retryQueuedPluginMessages, on every draft claim) running meanwhile finds
	// the claim held, or the event no longer queued, and delivers nothing — so
	// the chief never acts on the event muster is placing (no second forward,
	// no second card). Either refused: the chief's, as before.
	claimant := "triage/" + strconv.FormatInt(m.ID, 10)
	won, err := s.ext.Agents.ClaimKickoff(ctx, chief.ID, claimant, triageClaimTTL)
	if err != nil || !won {
		return false
	}
	defer func() {
		if err := s.ext.Agents.ReleaseKickoffClaim(context.WithoutCancel(ctx), chief.ID, claimant); err != nil {
			s.logger.Printf("task triage: task %d: release the chief's claim: %v", n.ID, err)
		}
	}()
	if ok, err := s.ext.Plugins.BeginDelivery(ctx, m.ID, chief.ID); err != nil || !ok {
		return false
	}
	if s.triageClaimed != nil {
		s.triageClaimed() // a test's seam: a concurrent retry lands here
	}
	var out, handler, did string
	switch d.kind {
	case triageForward:
		out, handler = s.toolTaskForward(ctx, chief, n.ID, triageForwardNote), chiefusage.HandlerTaskForward
		did = "forwarded it to the agent working the task"
	case triageRoute:
		args, _ := json.Marshal(map[string]any{"task_id": n.ID, "subject": d.subject})
		out, handler = s.toolRouteToSession(ctx, chief, string(args)), chiefusage.HandlerTaskRoute
		did = "routed it to " + d.subject + ", the one session on it"
	case triageClaimed:
		// Nothing to call: the task leaves "Task to route" as the chief's own
		// routing would take it off (recordTaskRouted), and no card is raised.
		s.recordTaskRouted(ctx, n, triageClaimedTool)
		out, handler = toolJSON(map[string]any{"task_id": n.ID, "claimed": true}), chiefusage.HandlerTaskClaimed
		did = "left it alone: it is already claimed by " + claimNames(facts.claims)
	default:
		out, handler = s.toolTaskRouteAttention(ctx, chief, n.ID, triageBoardNote), chiefusage.HandlerTaskBoard
		did = "put it in front of your owner (no session or agent is on it)"
	}
	if toolRefused(out) {
		// The call refused (a window gone between the hint and the route, the
		// queue full, …): nothing was done. The event goes back to the queue
		// and the caller hands it to the chief, as before.
		s.logger.Printf("task triage: task %d: the %s was refused (%s); to the chief", n.ID, d.kind, out)
		if err := s.ext.Plugins.AbandonDelivery(context.WithoutCancel(ctx), m.ID, chief.ID); err != nil {
			s.logger.Printf("task triage: task %d: requeue event %d: %v", n.ID, m.ID, err)
		}
		return false
	}
	reason := plugins.SettledByMuster + "task triage — " + did
	if ok, err := s.ext.Plugins.RecordDelivery(ctx, m.ID, chief.ID, plugins.StatusDelivered); err != nil || !ok {
		s.logger.Printf("task triage: task %d: record event %d delivered (%v): %v", n.ID, m.ID, ok, err)
	} else if err := settler.SettleAsMuster(ctx, m.ID, reason, "task-triage"); err != nil {
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
	// The task as it stands now, with its thread: the claims are read from it.
	cur, err := s.ext.Notes.Get(ctx, n.ID)
	if err != nil {
		s.logger.Printf("task triage: task %d: read it: %v", n.ID, err)
		return f, false
	}
	f.claims = notes.Claims(cur)
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

// triageClaimedTool is the routing the triage records for a claimed task
// (task.chief_routed's tool): it takes the task off "Task to route".
const triageClaimedTool = "task_triage_claimed"

// claimNames is claims' names, comma-separated, for a sentence.
func claimNames(claims []notes.Claim) string {
	names := make([]string, 0, len(claims))
	for _, c := range claims {
		names = append(names, c.Who)
	}
	return strings.Join(names, ", ")
}

// toolRefused reports whether a tool's answer is a refusal ({"error": …}).
func toolRefused(out string) bool {
	var v struct {
		Error any `json:"error"`
	}
	return json.Unmarshal([]byte(out), &v) != nil || v.Error != nil
}
