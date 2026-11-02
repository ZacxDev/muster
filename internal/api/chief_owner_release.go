package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
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
//  1. THE OWNER'S TURN IS IN FLIGHT on this chief, in a known ccd LANE: its
//     marker (agents owner_turn_*, migration 0069) names the chief's owner and
//     the lane. Only the owner's two signed-in chat doors set it
//     (runEnteredTurn, for handleAgentWS and handleAgentSend, when the typer IS
//     the agent's owner), and only once ccd ACCEPTED the text — its 103 after
//     the CLI's UserPromptSubmit for that very text, which names the lane
//     (agents.MarkTurnAccepted). A text ccd refused never marks; the marker is
//     cleared when muster's turn call returns, whatever it returns (a set still
//     in flight then is waited for and undone: markOwnerTurn), and lapses after
//     the turn budget.
//  2. EVERY OTHER CLI IN THE CHIEF'S POD IS IDLE, by its OWN account (ccd's GET
//     /v1/clis: the status each live Claude Code process writes in its sessions
//     file), and the owner's lane is among them. The pod's CLIs share one
//     muster credential, so a tool call does not say which CLI made it; but a
//     tool call only comes from a CLI that is working, so when the owner's lane
//     is the ONLY one working, the caller is the owner's lane. This is what
//     refuses a turn ccd gave up on that the CLI is still running (a zombie), a
//     turn the CLI started by itself, `chief ask` over the machine token, a job
//     in a lane, and a muster-started turn in another lane. A ccd that cannot
//     answer (unreachable, an image without the route) refuses: fail closed.
//  3. NO MUSTER-STARTED TURN HOLDS THE CHIEF'S SESSION CLAIM. Mostly implied by
//     2 now; kept because the claim is taken before a delivery's CLI turn
//     starts, so it also covers the instant before that CLI reports busy.
//  4. THE CALL CAME OVER MCP. A gateway-runtime session's tool calls run in
//     muster's own process (AgentToolDispatch), from a session that is no CLI
//     and so invisible to 2; they never release.
//  5. THE GATE IS ONE THE CHIEF FILED: the task's only gate: tag is the exact
//     one task_create_gated set and recorded (plugins.ActionGatedTaskFiled). An
//     owner's own hold (gate:waiting-on-legal) is never released here.
//
// Each failure is its own refusal, and a refused call leaves the gate on and
// starts nothing. 1–3 run before the reservation and again under its lock; the
// gate is removed only AFTER the agent was created, so a refused start leaves
// the task gated; the release is then recorded as a comment quoting the owner's
// message that passed the check, verbatim, and the owner gets a push saying so.
//
// ⚠ WHAT IT CANNOT TELL: whether the owner's message was ABOUT this task. The
// chief decides that; the server bounds it to "the owner's own turn is the only
// thing running", and the push makes a misread visible at once. A line the
// owner types straight into a claude-code chief's TERMINAL goes to the CLI, not
// through muster, so it never marks a turn.
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
	SetOwnerTurn(ctx context.Context, agentID, by, messageID int64, lane string, ttl time.Duration) error
	ClearOwnerTurn(ctx context.Context, agentID, messageID int64) error
	OwnerTurnInFlight(ctx context.Context, agentID int64) (agents.OwnerTurn, error)
	ChatMessage(ctx context.Context, id int64) (agents.ChatMessage, error)
}

var _ ownerTurnStore = (*agents.PGStore)(nil)

// ownerTurnWriteTimeout bounds one marker write.
const ownerTurnWriteTimeout = 10 * time.Second

// markOwnerTurn returns ctx carrying the owner-turn marker for a's turn whose
// user row is msg, typed by typedBy, and the function that clears it — to be
// called when the turn call returns. The marker is set only when typedBy is a's
// OWNER and only once ccd accepts the text (agents.WithTurnAccepted), in the
// lane it names; otherwise ctx comes back unchanged with a no-op.
//
// 🔴 THE CLEAR WINS, WHATEVER THE ORDER. ccd's 103 is handled on a transport
// goroutine, so the set can be running when the turn returns. Set and clear
// share a mutex and a done flag: a clear waits for a set in progress and then
// clears it; a set that starts after the clear sees done and writes nothing.
// So no marker outlives its turn — on success, error, cancellation or timeout.
func (s *Server) markOwnerTurn(ctx context.Context, a agents.Agent, msg agents.ChatMessage, typedBy int64) (context.Context, func()) {
	st, ok := s.ext.Agents.(ownerTurnStore)
	if !ok || typedBy <= 0 || typedBy != agentOwnerID(a) || msg.ID <= 0 {
		return ctx, func() {}
	}
	bctx := context.WithoutCancel(ctx)
	ttl := agentTurnBudget(a.Kind)
	var mu sync.Mutex
	done := false
	ctx = agents.WithTurnAccepted(ctx, func(acc agents.TurnAcceptance) {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return
		}
		wctx, cancel := context.WithTimeout(bctx, ownerTurnWriteTimeout)
		defer cancel()
		if err := st.SetOwnerTurn(wctx, a.ID, typedBy, msg.ID, acc.Lane, ttl); err != nil {
			s.logger.Printf("agents: mark %s's owner turn (message %d): %v", a.Name, msg.ID, err)
		}
	})
	return ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		done = true
		wctx, cancel := context.WithTimeout(bctx, ownerTurnWriteTimeout)
		defer cancel()
		if err := st.ClearOwnerTurn(wctx, a.ID, msg.ID); err != nil {
			s.logger.Printf("agents: clear %s's owner turn (message %d): %v", a.Name, msg.ID, err)
		}
	}
}

// inProcessToolCallKey marks a tool call muster dispatched in its own process
// (AgentToolDispatch: a gateway-runtime session's turn), as opposed to one that
// came over MCP from a CLI.
type inProcessToolCallKey struct{}

func withInProcessToolCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, inProcessToolCallKey{}, true)
}

func isInProcessToolCall(ctx context.Context) bool {
	v, _ := ctx.Value(inProcessToolCallKey{}).(bool)
	return v
}

// cliActivityReader is the gateway's read of a pod's CLIs (agentgateway
// CLIActivity, ccd's GET /v1/clis); found by assertion. A gateway without it
// releases nothing.
type cliActivityReader interface {
	CLIActivity(ctx context.Context, a agents.Agent) (agents.CLIActivity, error)
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
	chiefReleaseOtherCLIBusy = "owner_asked: another of your sessions is working at the same time as your owner's turn (a " +
		"turn still running, one you started yourself, a job or a question from outside), so muster cannot tell that this " +
		"call comes from your owner's; ask your owner again in the chat once only their turn is running, or use task_route_attention"
	chiefReleaseCLIsUnknown = "owner_asked: muster could not read which of your sessions are working (your pod did not answer, " +
		"or its image is too old to say), so the gate was not released; ask your owner to release it on the board, or use " +
		"task_route_attention"
	chiefReleaseNotOverMCP = "owner_asked: this session is not a Claude Code session, and the gate is released only from one; " +
		"ask your owner to release it on the board, or use task_route_attention"
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

// onlyTheOwnersLaneWorks is "" when, of the CLIs in act, the owner's lane is
// listed and every OTHER one is idle; else the refusal. lane "" is ccd's
// primary CLI.
func onlyTheOwnersLaneWorks(act agents.CLIActivity, lane string) string {
	found := false
	for _, c := range act.CLIs {
		own := c.Known && ((lane == "" && c.Primary) || (lane != "" && !c.Primary && c.Lane == lane))
		if own {
			found = true
			continue
		}
		if c.Status != agents.CLIStatusIdle {
			return chiefReleaseOtherCLIBusy
		}
	}
	if !found {
		// The owner's own CLI is not in the answer: ccd cannot see what it
		// must, so nothing it says about the others is trusted.
		return chiefReleaseCLIsUnknown
	}
	return ""
}

// ownerTurnFor is the owner's message whose turn is in flight on chief now —
// and the only thing working in its pod — or the refusal saying which condition
// failed (see the header).
func (s *Server) ownerTurnFor(ctx context.Context, chief agents.Agent) (agents.ChatMessage, string) {
	if isInProcessToolCall(ctx) {
		return agents.ChatMessage{}, chiefReleaseNotOverMCP
	}
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
	rd, ok := s.ext.Gateway.(cliActivityReader)
	if !ok {
		return agents.ChatMessage{}, chiefReleaseCLIsUnknown
	}
	act, err := rd.CLIActivity(ctx, chief)
	if err != nil {
		s.logger.Printf("task routing: read %s's CLIs: %v", chief.Name, err)
		return agents.ChatMessage{}, chiefReleaseCLIsUnknown
	}
	if why := onlyTheOwnersLaneWorks(act, turn.Lane); why != "" {
		return agents.ChatMessage{}, why
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

// quoted is msg's text as quote lines, verbatim.
func quoted(msg agents.ChatMessage) string {
	lines := strings.Split(strings.ReplaceAll(msg.Content, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// ownerReleaseComment is that comment: which gate went, on which of the owner's
// messages — the one whose turn passed the check — quoted VERBATIM: the owner's
// own words, on the owner's own task, so the board shows why.
func ownerReleaseComment(chief agents.Agent, agentName string, gates []string, msg agents.ChatMessage) string {
	return "muster released the gate (" + strings.Join(gates, ", ") + ") and " + firstNonBlank(chief.DisplayName, chief.Name) +
		" started " + agentName + " on it, during your own message #" + strconv.FormatInt(msg.ID, 10) + " in its chat:\n\n" +
		quoted(msg)
}

// ownerReleaseStuckComment is the comment when the agent started but the gate
// tag could not be removed.
func ownerReleaseStuckComment(chief agents.Agent, agentName string, gates []string, msg agents.ChatMessage) string {
	return firstNonBlank(chief.DisplayName, chief.Name) + " started " + agentName + " on this task during your own message #" +
		strconv.FormatInt(msg.ID, 10) + " in its chat, but muster could not remove the gate (" + strings.Join(gates, ", ") +
		"): the agent is working while the task still shows it. Remove the gate on the board, or stop the agent.\n\n" + quoted(msg)
}

// errGateNotRemoved: applyGateRelease could not take the gate off.
var errGateNotRemoved = errors.New("the gate tag could not be removed")

// ownerReleasePushBodyMax bounds the owner's words quoted in the release push.
const ownerReleasePushBodyMax = 80

// applyGateRelease removes rel's gates from task taskID once agentName was
// started on it, records the release as a comment and tells the owner with a
// push. The agent already runs, so nothing here undoes the start: a gate that
// cannot be removed is SAID — on the task and in the push — never left silent.
func (s *Server) applyGateRelease(ctx context.Context, chief agents.Agent, taskID int64, agentName string, rel *gateRelease) error {
	id := strconv.FormatInt(taskID, 10)
	body, title, pushBody := ownerReleaseComment(chief, agentName, rel.gates, rel.msg),
		"Task #"+id+": your chief released its gate",
		"Your chief released the gate on task #"+id+" on your message: "+mirror.OneLine(rel.msg.Content, ownerReleasePushBodyMax)
	var failed error
	if _, err := s.ext.Notes.RemoveTags(ctx, taskID, rel.gates); err != nil {
		s.logger.Printf("task routing: remove task %d's gate after the start: %v", taskID, err)
		failed = errGateNotRemoved
		body, title = ownerReleaseStuckComment(chief, agentName, rel.gates, rel.msg), "Task #"+id+": agent started, gate still on"
		pushBody = "Your chief started an agent on task #" + id + " on your message, but its gate tag could not be removed: " +
			mirror.OneLine(rel.msg.Content, ownerReleasePushBodyMax)
	}
	if _, err := s.ext.Notes.AddComment(ctx, notes.Comment{NoteID: taskID, Author: ownerReleaseAuthor, Body: body}); err != nil {
		s.logger.Printf("task routing: record task %d's gate release: %v", taskID, err)
	}
	s.logger.Printf("task routing: task %d's gate %v released on owner message %d in %s's chat (removed: %v)", taskID, rel.gates, rel.msg.ID, chief.Name, failed == nil)
	s.broadcast(ctx, EventTaskChanged, id)
	s.pushTask(agentOwnerID(chief), push.EventAgent, RouterNotification{
		Type: taskNotificationType, ID: id, Title: title, Body: pushBody, Tag: "task-" + id + "-gate-released",
	})
	return failed
}
