package api

import (
	"context"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/redact"
)

// ---------------------------------------------------------------------------
// CHIEF-CHAT QUOTES AS COVERING WORDS (docs/design/session-state.md, "Covering
// words", step 3b-1; operator decisions 10 and 13, and the split "Chief chat
// first", 2026-11-04).
//
// With session state on, a stall nudge's quote may come from the owner's own
// Chief chat as well as from the candidate session: the Chief chat is a source
// for ANY of the owner's sessions (decision 10). Only turns muster recorded as
// TYPED by the owner count (chat_messages.typed_by, migration 0079): written at
// runEnteredTurn from the owner's signed-in chat doors only (ownerTypedBy — the
// chat page's socket and the composer's POST, on a browser session). Relayed
// answers, session questions (and `chief ask` over a token), plugin messages,
// task events and agent-sent turns never count, fenced or not (decision 13);
// TestEveryChatWriterIsClassified pins every writer, and rows from before 0079
// count for nobody.
//
// 🔴 THE GUARDS ARE CODE, AS FOR ANY QUOTE: the chief names the quote's source
// (quote_source), and chiefnudge.CheckQuoteFrom looks for it, verbatim, in that
// source only. A Chief-chat quote that does not verify meets the same guard as
// any quote (suppressed:quote_unverified, its source recorded; the stop
// pushes), never a paste. A read of the Chief chat that fails contributes
// nothing: no Chief-chat quote can pass.
//
// 🔴 SHADOW FIRST. A nudge whose quote is from the Chief chat is sent only for
// an owner ALSO on MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS, on top of
// every gate step 3a has (chiefNudgeLiveFor); otherwise it is shadow — recorded
// with its source on the Chief nudges tab, never pasted, and the stop pushed.
// The gate is read again at the paste and at an agent's turn (nudgeSendLive),
// from the source stored on the decision. A same-session quote keeps the gates
// it had.
//
// With session state off nothing here runs: the Chief chat is never read, the
// prompt is the one from before 3b-1, and a quote is checked in the session
// alone.
// ---------------------------------------------------------------------------

// chiefChatReader is the agents store's read of the owner's typed turns
// (agents.PGStore.OwnerTypedChatMessages); found by assertion, so a store
// without it gives no Chief-chat quotes.
type chiefChatReader interface {
	OwnerTypedChatMessages(ctx context.Context, agentID, by int64, since time.Time, limit int) ([]agents.ChatMessage, error)
}

var _ chiefChatReader = (*agents.PGStore)(nil)

// nudgeChiefChat is owner's TYPED turns in their Chief chat with chief, within
// OwnerWindow, as the chief is shown them: each named "chief:<id>", redacted
// and cut exactly like a session's owner messages (the bytes CheckQuoteFrom
// matches), the newest MaxOwnerMessages. nil — no Chief-chat quote can pass —
// when session state is off, when the store cannot read them, or when the read
// failed; empty when there are none.
//
// The read is the newest MaxOwnerMessages rows of one table, newest first: it
// cannot have a gap, and only those are ever shown or checked, so it reads no
// more than that.
func (s *Server) nudgeChiefChat(octx context.Context, chief agents.Agent, owner int64) []chiefnudge.Msg {
	if !s.sessionStateOn() {
		return nil
	}
	r, ok := s.ext.Agents.(chiefChatReader)
	if !ok {
		return nil
	}
	since := s.now().Add(-chiefnudge.OwnerWindow)
	rows, err := r.OwnerTypedChatMessages(octx, chief.ID, owner, since, chiefnudge.MaxOwnerMessages)
	if err != nil {
		s.logger.Printf("chief nudges: owner %d: the Chief chat: %v; no Chief-chat quote this pass", owner, err)
		return nil
	}
	msgs := make([]chiefnudge.Msg, 0, len(rows))
	for _, m := range rows {
		if m.TypedBy != owner || m.Role != "user" {
			continue // the store's own filter, held again: only the owner's typing
		}
		msgs = append(msgs, chiefnudge.Msg{Role: "user", Text: m.Content,
			ID: chiefnudge.ChiefChatIDPrefix + strconv.FormatInt(m.ID, 10), At: m.CreatedAt})
	}
	// The same filter as a session's (the window, a fence, muster's own
	// openings): typed by the owner already, so this only narrows.
	out := chiefnudge.OwnerMessages(msgs, nil, nudgeInjectedPrefixes, since, false)
	for i := range out {
		out[i].Text = redact.Secrets(chiefnudge.Cut(out[i].Text, chiefnudge.MaxOwnerMessageRunes))
	}
	if out == nil {
		out = []chiefnudge.Msg{}
	}
	return out
}

// checkNudgeQuote is the quote guard over the sources this nudge may quote:
// with session state on, the source the chief named (the session's own owner
// messages, or chat, the owner's typed Chief-chat turns); with it off, the
// session alone, exactly as before step 3b-1 (source ""). It returns the
// message quoted and the source it was checked in.
func checkNudgeQuote(prop chiefnudge.Proposal, owner, chat []chiefnudge.Msg, notify bool) (chiefnudge.Msg, string, error) {
	if !notify {
		m, err := chiefnudge.CheckQuote(prop, owner)
		return m, "", err
	}
	return chiefnudge.CheckQuoteFrom(prop, owner, chat)
}
