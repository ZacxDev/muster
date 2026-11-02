package api

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// THE OWNER'S OWN WORDS RELEASE A GATE: task_dispatch's owner_asked.
//
// A plugin message becomes work only as a GATED task (task_create_gated), and
// task_dispatch refuses a gated task. Before this the only release was the owner
// removing the gate on the board — so an owner who TOLD their chief, in its
// chat, to do the work met a chief that could only answer "go and approve it on
// the board". The owner's decision: what they tell the chief to do, it does,
// without another screen.
//
// 🔴 THE PROPERTY THAT SURVIVES: TEXT FROM AN OUTSIDE SENDER NEVER RELEASES A
// GATE; ONLY THE OWNER'S OWN TYPED WORDS DO — and the SERVER checks that, not
// the chief. The chief's `owner_asked: true` is a claim, and a chief reading a
// plugin message can be talked into making it. So the server releases the gate
// only when, in THIS chief's persisted chat:
//
//  1. its owner has TYPED a message (chat_messages.typed_by = the chief's owner,
//     migration 0069 — stamped by the signed-in chat doors and nothing else:
//     ownerTypedBy, TestOnlyTheOwnersChatDoorsStampTypedBy);
//  2. that message is recent (ownerRequestMaxAge); and
//  3. it is the NEWEST user-side message the chief has been handed, in any of
//     its sessions — nothing came after it: no plugin delivery, relayed answer,
//     route note, nudge or anybody else's line; and
//  4. no muster-started turn is IN FLIGHT for the chief: nobody holds its
//     session claim (agents.Store.KickoffClaimHolder).
//
// 3 and 4 together are what make "the turn the chief is acting in is the
// owner's" true, and a turn an outside sender started unable to borrow an
// earlier line of the owner's. Neither alone is enough: a plugin delivery and a
// relayed attention answer store their user row AFTER their turn ends
// (plugins.Deliverer, runDeliveredTurn), so DURING that turn the owner's older
// line still reads as the newest (3 passes) — but every turn muster starts on
// the chief holds the claim for its whole length (a delivery, a relayed answer,
// a route hand-back, a nudge, chief_ask, the first turn), and the owner's chat
// doors never take it. Writers that start no claimed turn (a route note, a
// handed message, a forward, `chief ask` over the machine token) store their
// row BEFORE anything runs, so 3 sees them.
//
// Each failure is its own refusal, and a refused call leaves the gate on and
// starts nothing. The chief names no message id: it cannot see muster's ids (a
// claude-code chief is handed the TEXT of a turn, never its row), so the server
// resolves "the owner's message" itself and the check above is what makes that
// safe — the only message it can resolve to is the one the chief is answering.
//
// ⚠ WHAT IT CANNOT TELL: whether the owner's message was ABOUT this task. The
// chief decides that; the server bounds it to "the owner spoke last, recently".
// And a line the owner types straight into a claude-code chief's TERMINAL
// (the rw tmux attach) goes to the CLI, not through muster, so muster never
// recorded it and it can never release a gate: the refusal says to use the chat.
// ---------------------------------------------------------------------------

// notTyped is runEnteredTurn's typedBy for a turn no person typed into muster's
// chat: every caller but the owner's two chat doors.
const notTyped int64 = 0

// ownerTypedBy is the user who typed a chat message arriving on ctx's request,
// for agents.ChatMessage.TypedBy: the principal's user when the request came
// through a signed-in BROWSER session (authz.ViaSession), else 0.
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

// ownerRequestMaxAge bounds how old the owner's typed message may be when it
// releases a gate. Long enough for a chief to file the task and dispatch it in
// the same exchange (a claude-code turn alone may run 37 minutes:
// agentTurnBudget), short enough that a line typed this morning does not
// authorise work an outside sender asks for this afternoon.
const ownerRequestMaxAge = 2 * time.Hour

// The owner_asked refusals, each its own sentence (the condition that failed),
// in the order they are checked.
const (
	chiefReleaseNoOwnerMessage = "owner_asked: your owner has not typed a message in your muster chat, so muster cannot release " +
		"the gate on their word (only a line they typed in muster's chat counts: not a plugin message, a relayed answer, " +
		"a note from muster or anything typed in your terminal); use task_route_attention"
	chiefReleaseStale = "owner_asked: your owner's latest message in your chat is too old to release a gate (more than 2 hours); " +
		"ask them again in the chat, or use task_route_attention"
	chiefReleaseNotLatest = "owner_asked: your owner's latest message is not the newest one in your chat — something else " +
		"(a plugin message, a relayed answer, a note from muster) arrived after it, so muster cannot tell that your " +
		"owner asked for this; ask them again in the chat, or use task_route_attention"
	chiefReleaseOutsideTurn = "owner_asked: muster is running a turn for you that your owner did not type (a plugin message, " +
		"a relayed answer, a nudge), so muster cannot tell that your owner asked for this; ask them again in the chat, " +
		"or use task_route_attention"
	chiefReleaseUnverifiable = "owner_asked: your owner's messages could not be read, so the gate was not released; " +
		"try again later or use task_route_attention"
)

// ownerTurnReader is the agents store's LatestUserTurns; the gate release finds
// it by assertion (pendingNoteAppender's pattern). A store without it can
// release nothing.
type ownerTurnReader interface {
	LatestUserTurns(ctx context.Context, agentID, typedBy int64) (agents.UserTurns, error)
}

var _ ownerTurnReader = (*agents.PGStore)(nil)

// ownerRequest is the owner's typed message that releases a gate for chief, or
// the refusal saying which condition failed (see the header).
func (s *Server) ownerRequest(ctx context.Context, chief agents.Agent) (agents.ChatMessage, string) {
	rd, ok := s.ext.Agents.(ownerTurnReader)
	if !ok {
		return agents.ChatMessage{}, chiefReleaseUnverifiable
	}
	owner := agentOwnerID(chief)
	if owner <= 0 {
		return agents.ChatMessage{}, chiefReleaseNoOwnerMessage
	}
	turns, err := rd.LatestUserTurns(ctx, chief.ID, owner)
	if err != nil {
		s.logger.Printf("task routing: read %s's latest user turns: %v", chief.Name, err)
		return agents.ChatMessage{}, chiefReleaseUnverifiable
	}
	typed := turns.Typed
	// 🔴 THE THREE CHECKS RE-READ THE ROW, NOT ONLY THE QUERY'S FILTER: typed_by
	// equal to the owner and the row this chief's (a store whose filter drifted
	// must still not release).
	if typed.ID == 0 || typed.TypedBy != owner || typed.AgentID != chief.ID {
		return agents.ChatMessage{}, chiefReleaseNoOwnerMessage
	}
	if s.now().Sub(typed.CreatedAt) > ownerRequestMaxAge {
		return agents.ChatMessage{}, chiefReleaseStale
	}
	if turns.Latest.ID != typed.ID {
		return agents.ChatMessage{}, chiefReleaseNotLatest
	}
	holder, err := s.ext.Agents.KickoffClaimHolder(ctx, chief.ID)
	if err != nil {
		s.logger.Printf("task routing: read %s's session claim: %v", chief.Name, err)
		return agents.ChatMessage{}, chiefReleaseUnverifiable
	}
	if holder != "" {
		return agents.ChatMessage{}, chiefReleaseOutsideTurn
	}
	return typed, ""
}

// ownerReleaseAuthor is the author of the comment a release leaves on the task.
const ownerReleaseAuthor = "muster"

// ownerReleaseComment is that comment: which gates went, on which of the
// owner's messages, and the message quoted VERBATIM (each line as a quote line)
// — the owner's own words, on the owner's own task, so the board shows why.
func ownerReleaseComment(chief agents.Agent, gates []string, msg agents.ChatMessage) string {
	lines := strings.Split(strings.ReplaceAll(msg.Content, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return "muster released the gate (" + strings.Join(gates, ", ") + ") so " + firstNonBlank(chief.DisplayName, chief.Name) +
		" could start an agent: your own message #" + strconv.FormatInt(msg.ID, 10) + " in its chat, typed " +
		msg.CreatedAt.UTC().Format(time.RFC3339) + ", was the newest message it had been given:\n\n" + strings.Join(lines, "\n")
}

// gateTags is every gate: tag on tags. A task may carry more than one, and the
// release removes them all — one left behind would still refuse the dispatch.
func gateTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if ns, _ := notes.ParseTag(t); ns == notes.NSGate {
			out = append(out, t)
		}
	}
	return out
}

// releaseGateOnOwnerWord removes every gate on task n on the owner's message
// msg and records it as a comment. 🔴 FAIL CLOSED: a comment that cannot be
// written puts the gates back, so a gate never goes without the record saying
// why.
func (s *Server) releaseGateOnOwnerWord(ctx context.Context, chief agents.Agent, n notes.Note, msg agents.ChatMessage) error {
	gates := gateTags(n.Tags)
	if _, err := s.ext.Notes.RemoveTags(ctx, n.ID, gates); err != nil {
		return err
	}
	if _, err := s.ext.Notes.AddComment(ctx, notes.Comment{NoteID: n.ID, Author: ownerReleaseAuthor,
		Body: ownerReleaseComment(chief, gates, msg)}); err != nil {
		if _, rerr := s.ext.Notes.AddTags(context.WithoutCancel(ctx), n.ID, gates); rerr != nil {
			s.logger.Printf("task routing: put the gate back on task %d after the release comment failed: %v", n.ID, rerr)
		}
		return err
	}
	s.logger.Printf("task routing: task %d's gates %v released on owner message %d in %s's chat", n.ID, gates, msg.ID, chief.Name)
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(n.ID, 10))
	return nil
}
