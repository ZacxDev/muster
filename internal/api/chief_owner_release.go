package api

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
)

// ---------------------------------------------------------------------------
// THE OWNER'S OWN TURN RELEASES THE CHIEF'S GATE: task_dispatch's owner_asked.
//
// A plugin message becomes work only as a GATED task (task_create_gated), and
// task_dispatch refuses a gated task. Before this the only release was the owner
// removing the gate on the board — so an owner who TOLD their chief, in its
// chat, to do the work met a chief that could only answer "go and approve it on
// the board". The owner's decision: what they tell the chief to do, it does,
// without another screen.
//
// 🔴 THE PROPERTY THAT SURVIVES: TEXT FROM AN OUTSIDE SENDER NEVER RELEASES A
// GATE; ONLY A TURN THE OWNER TYPED DOES — and the SERVER checks that, not the
// chief. `owner_asked: true` is a claim a chief reading a plugin message can be
// talked into making, so the server releases only when ALL of these hold:
//
//  1. THE OWNER'S TURN IS IN FLIGHT on this chief: its marker (agents
//     owner_turn_*, migration 0069) names the chief's owner. Only the owner's
//     two signed-in chat doors set it (runEnteredTurn, for handleAgentWS and
//     handleAgentSend, when the typer IS the agent's owner), and only once the
//     runtime ACCEPTED the text (agents.MarkTurnAccepted: ccd's 103 after the
//     CLI's UserPromptSubmit for that very text). A text the runtime refused —
//     the CLI busy with a turn ccd already gave up on, not ready — never marks;
//     the marker is cleared the moment muster's turn call returns, whatever it
//     returns, and lapses on its own after the turn budget. So a turn Claude
//     Code started by itself, a plugin turn running past ccd's timeout, or the
//     owner's line that ccd refused cannot release anything.
//  2. NO MUSTER-STARTED TURN IS IN FLIGHT (the chief's session claim is not
//     held). Kept because 1 is per AGENT, not per turn: ccd runs one CLI per
//     chat session (lanes), and an MCP call does not say which lane made it, so
//     a plugin delivery, relayed answer, nudge or hand-back running in another
//     lane DURING the owner's turn could otherwise ride the owner's marker.
//     Every such turn holds the claim for its whole length; the owner's doors
//     never take it.
//  3. THE GATE IS ONE THE CHIEF FILED: the task's only gate: tag is the exact
//     one task_create_gated set and recorded (plugins.ActionGatedTaskFiled). An
//     owner's own hold (gate:waiting-on-legal) is never released here.
//
// Each failure is its own refusal, and a refused call leaves the gate on and
// starts nothing. The gate is removed only AFTER the agent was created (the
// reservation passed every cap and the one-start rule under its lock), so a
// refused start leaves the task gated; the release is then recorded as a comment
// quoting the owner's message verbatim, and the owner gets a push saying so.
//
// ⚠ WHAT IT CANNOT TELL: whether the owner's message was ABOUT this task. The
// chief decides that; the server bounds it to "the owner's own turn is running
// now", and the push makes a misread visible at once. A line the owner types
// straight into a claude-code chief's TERMINAL goes to the CLI, not through
// muster, so it never marks a turn.
// ---------------------------------------------------------------------------

// notTyped is runEnteredTurn's typedBy for a turn no person typed into muster's
// chat: every caller but the owner's two chat doors.
const notTyped int64 = 0

// ownerTypedBy is the user who typed a chat message arriving on ctx's request:
// the principal's user when the request came through a signed-in BROWSER
// session (authz.ViaSession), else 0.
//
// 🔴 ViaSession ONLY. A personal API token, the hook token (`chief ask`), a
// plugin, a relay, a route or a sweep is not the owner typing in the chat, and
// an all-scope principal is nobody in particular.
func ownerTypedBy(ctx context.Context) int64 {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.Via != authz.ViaSession || p.UserID <= 0 {
		return 0
	}
	return p.UserID
}

// ownerTurnStore is the agents store's owner-turn marker (migration 0069) and
// its chat-row read; found by assertion (pendingNoteAppender's pattern). A store
// without it marks nothing and releases nothing.
type ownerTurnStore interface {
	SetOwnerTurn(ctx context.Context, agentID, by, messageID int64, ttl time.Duration) error
	ClearOwnerTurn(ctx context.Context, agentID, messageID int64) error
	OwnerTurnInFlight(ctx context.Context, agentID int64) (agents.OwnerTurn, error)
	ChatMessage(ctx context.Context, id int64) (agents.ChatMessage, error)
}

var _ ownerTurnStore = (*agents.PGStore)(nil)

// markOwnerTurn returns ctx carrying the owner-turn marker for a's turn whose
// user row is msg, typed by typedBy, and the function that clears it — to be
// called when the turn call returns. The marker is set only when typedBy is a's
// OWNER and only once the runtime accepts the text (agents.WithTurnAccepted);
// otherwise ctx comes back unchanged with a no-op.
func (s *Server) markOwnerTurn(ctx context.Context, a agents.Agent, msg agents.ChatMessage, typedBy int64) (context.Context, func()) {
	st, ok := s.ext.Agents.(ownerTurnStore)
	if !ok || typedBy <= 0 || typedBy != agentOwnerID(a) || msg.ID <= 0 {
		return ctx, func() {}
	}
	bctx := context.WithoutCancel(ctx)
	ttl := agentTurnBudget(a.Kind)
	ctx = agents.WithTurnAccepted(ctx, func() {
		wctx, cancel := context.WithTimeout(bctx, 10*time.Second)
		defer cancel()
		if err := st.SetOwnerTurn(wctx, a.ID, typedBy, msg.ID, ttl); err != nil {
			s.logger.Printf("agents: mark %s's owner turn (message %d): %v", a.Name, msg.ID, err)
		}
	})
	return ctx, func() {
		wctx, cancel := context.WithTimeout(bctx, 10*time.Second)
		defer cancel()
		if err := st.ClearOwnerTurn(wctx, a.ID, msg.ID); err != nil {
			s.logger.Printf("agents: clear %s's owner turn (message %d): %v", a.Name, msg.ID, err)
		}
	}
}

// The owner_asked refusals, each its own sentence (the condition that failed).
const (
	chiefReleaseNotChiefsGate = "owner_asked: this task's gate is not one you filed with task_create_gated (your owner set it, " +
		"or it carries a gate of theirs as well), so only your owner can release it, on the board; use task_route_attention"
	chiefReleaseNoOwnerTurn = "owner_asked: you are not answering a message your owner typed in your muster chat right now, " +
		"so muster cannot release the gate on their word (a plugin message, a relayed answer, a note from muster, a turn " +
		"you started yourself or anything typed in your terminal never counts); ask them in the chat, or use task_route_attention"
	chiefReleaseNotOwnersTurn = "owner_asked: the message you are answering was not typed by your owner, so muster cannot " +
		"release the gate on it; use task_route_attention"
	chiefReleaseOutsideTurn = "owner_asked: muster is also running a turn for you that your owner did not type (a plugin " +
		"message, a relayed answer, a nudge), so muster cannot tell which of them asked for this; ask your owner again " +
		"in the chat once it is over, or use task_route_attention"
	chiefReleaseUnverifiable = "owner_asked: your owner's turn could not be read, so the gate was not released; " +
		"try again later or use task_route_attention"
	chiefReleaseChanged = "owner_asked: your owner's turn or the task's gate changed while the start was being checked; call again"
)

// gateRelease is a release task_dispatch has verified and will apply once the
// agent exists: the gate tags to remove and the owner's message it rests on.
type gateRelease struct {
	gates []string
	msg   agents.ChatMessage
}

// gateTags is every gate: tag on tags.
func gateTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if ns, _ := notes.ParseTag(t); ns == notes.NSGate {
			out = append(out, t)
		}
	}
	return out
}

// withoutTags is tags minus drop.
func withoutTags(tags, drop []string) []string {
	gone := map[string]bool{}
	for _, d := range drop {
		gone[d] = true
	}
	var out []string
	for _, t := range tags {
		if !gone[t] {
			out = append(out, t)
		}
	}
	return out
}

// gatedTaskLedger is the plugin store's record of which gate task_create_gated
// set on which task; found by assertion. A store without it releases nothing.
type gatedTaskLedger interface {
	RecordGatedTaskFiled(ctx context.Context, taskID, agentID int64, gate string) error
	GatedTaskFiled(ctx context.Context, taskID int64) (string, bool, error)
}

// chiefsGate is task n's gate tags when they are exactly the one gate
// task_create_gated filed on it, else the refusal.
func (s *Server) chiefsGate(ctx context.Context, n notes.Note) ([]string, string) {
	led, ok := s.ext.Plugins.(gatedTaskLedger)
	if !ok {
		return nil, chiefReleaseNotChiefsGate
	}
	filed, ok, err := led.GatedTaskFiled(ctx, n.ID)
	if err != nil {
		s.logger.Printf("task routing: read task %d's filed gate: %v", n.ID, err)
		return nil, chiefReleaseUnverifiable
	}
	gates := gateTags(n.Tags)
	if !ok || len(gates) != 1 || gates[0] != filed {
		return nil, chiefReleaseNotChiefsGate
	}
	return gates, ""
}

// ownerTurnFor is the owner's message whose turn is in flight on chief now, or
// the refusal saying which condition failed (see the header).
func (s *Server) ownerTurnFor(ctx context.Context, chief agents.Agent) (agents.ChatMessage, string) {
	st, ok := s.ext.Agents.(ownerTurnStore)
	if !ok {
		return agents.ChatMessage{}, chiefReleaseUnverifiable
	}
	turn, err := st.OwnerTurnInFlight(ctx, chief.ID)
	if err != nil {
		s.logger.Printf("task routing: read %s's owner turn: %v", chief.Name, err)
		return agents.ChatMessage{}, chiefReleaseUnverifiable
	}
	if turn.MessageID <= 0 {
		return agents.ChatMessage{}, chiefReleaseNoOwnerTurn
	}
	if owner := agentOwnerID(chief); owner <= 0 || turn.By != owner {
		return agents.ChatMessage{}, chiefReleaseNotOwnersTurn
	}
	msg, err := st.ChatMessage(ctx, turn.MessageID)
	if err != nil || msg.AgentID != chief.ID || msg.Role != "user" {
		s.logger.Printf("task routing: %s's owner turn names message %d, which is not its own user row (%v)", chief.Name, turn.MessageID, err)
		return agents.ChatMessage{}, chiefReleaseUnverifiable
	}
	holder, err := s.ext.Agents.KickoffClaimHolder(ctx, chief.ID)
	if err != nil {
		s.logger.Printf("task routing: read %s's session claim: %v", chief.Name, err)
		return agents.ChatMessage{}, chiefReleaseUnverifiable
	}
	if holder != "" {
		return agents.ChatMessage{}, chiefReleaseOutsideTurn
	}
	return msg, ""
}

// verifyGateRelease is task_dispatch's owner_asked check on open gated task n:
// the release to apply after the start, or the refusal.
func (s *Server) verifyGateRelease(ctx context.Context, chief agents.Agent, n notes.Note) (*gateRelease, string) {
	gates, why := s.chiefsGate(ctx, n)
	if why != "" {
		return nil, why
	}
	msg, why := s.ownerTurnFor(ctx, chief)
	if why != "" {
		return nil, why
	}
	return &gateRelease{gates: gates, msg: msg}, ""
}

// sameGates reports whether tags carry exactly the gates rel verified.
func (rel *gateRelease) sameGates(tags []string) bool {
	g := gateTags(tags)
	return len(g) == len(rel.gates) && strings.Join(g, "\x00") == strings.Join(rel.gates, "\x00")
}

// ownerReleaseAuthor is the author of the comment a release leaves on the task.
const ownerReleaseAuthor = "muster"

// ownerReleaseComment is that comment: which gate went, on which of the owner's
// messages, and the message quoted VERBATIM (each line as a quote line) — the
// owner's own words, on the owner's own task, so the board shows why.
func ownerReleaseComment(chief agents.Agent, agentName string, gates []string, msg agents.ChatMessage) string {
	lines := strings.Split(strings.ReplaceAll(msg.Content, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return "muster released the gate (" + strings.Join(gates, ", ") + ") and " + firstNonBlank(chief.DisplayName, chief.Name) +
		" started " + agentName + " on it, during your own message #" + strconv.FormatInt(msg.ID, 10) + " in its chat:\n\n" +
		strings.Join(lines, "\n")
}

// ownerReleasePushBodyMax bounds the owner's words quoted in the release push.
const ownerReleasePushBodyMax = 80

// applyGateRelease removes rel's gates from task taskID once agentName was
// started on it, records the release as a comment and tells the owner with a
// push. Best-effort past the start: the agent already runs, so a failure is
// logged rather than undone.
func (s *Server) applyGateRelease(ctx context.Context, chief agents.Agent, taskID int64, agentName string, rel *gateRelease) {
	if _, err := s.ext.Notes.RemoveTags(ctx, taskID, rel.gates); err != nil {
		s.logger.Printf("task routing: remove task %d's gate after the start: %v", taskID, err)
		return
	}
	if _, err := s.ext.Notes.AddComment(ctx, notes.Comment{NoteID: taskID, Author: ownerReleaseAuthor,
		Body: ownerReleaseComment(chief, agentName, rel.gates, rel.msg)}); err != nil {
		s.logger.Printf("task routing: record task %d's gate release: %v", taskID, err)
	}
	s.logger.Printf("task routing: task %d's gate %v released on owner message %d in %s's chat", taskID, rel.gates, rel.msg.ID, chief.Name)
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(taskID, 10))
	id := strconv.FormatInt(taskID, 10)
	s.pushTask(agentOwnerID(chief), push.EventAgent, RouterNotification{
		Type:  taskNotificationType,
		ID:    id,
		Title: "Task #" + id + ": your chief released its gate",
		Body: "Your chief released the gate on task #" + id + " on your message: " +
			mirror.OneLine(rel.msg.Content, ownerReleasePushBodyMax),
		Tag: "task-" + id + "-gate-released",
	})
}
