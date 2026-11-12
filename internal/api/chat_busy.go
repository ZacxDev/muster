package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// A MESSAGE TYPED WHILE ANOTHER TURN HOLDS THE AGENT'S SESSION WAITS FOR IT.
//
// ccd drives one turn at a time per session and refuses a second with a typed
// 409 `busy`, BEFORE it types anything (cmd/ccd runTurn). The commonest way an
// owner meets it: the chief has just been turned on and is running its kickoff
// turn in its default chat session, and the owner's first message lands on it.
// Before this, that refusal went straight to the chat as
// "[error] responses HTTP 409: {…busy…}" — the message was stored but never
// sent, and the only thing on screen was a raw HTTP error.
//
// 🔴 ONE RULE, ONE PLACE: runEnteredTurn, the one function every door for a
// line the owner (or muster's route hand-back) wants answered goes through —
// the chat socket, the composer POST, the route hand-back. It is the same for a
// Kubernetes agent and a Compose chief: both are ccd behind the same gateway,
// so the refusal and this wait are identical.
//
// ⚠ NOT chatTurn: the attention deliverer (runDeliveredTurn) also calls chatTurn
// and must see the refusal AT ONCE, so it can put the answer back for its own
// sweep (agentgateway.Unsent). Waiting there would hold the delivery claim.
//
// Bounded (chatBusyWait): a session refusing for that long is not finishing a
// turn, so the person is told — in words, not a status code — and the message,
// already stored in the conversation, is theirs to send again.
// ---------------------------------------------------------------------------

const (
	// defaultChatBusyWait bounds how long a message waits for the session's
	// in-flight turn. A chief's kickoff is the turn it usually waits on, and that
	// is minutes, not tens of them.
	defaultChatBusyWait = 10 * time.Minute
	// defaultChatBusyPoll is how often it asks again. Each ask is a refusal ccd
	// answers before typing anything, so it costs one round trip.
	defaultChatBusyPoll = 3 * time.Second
)

// ChatQueuedNotice is what the chat shows while a message waits for the turn in
// flight (the WS "queued" message).
const ChatQueuedNotice = "Still finishing its previous turn — your message is queued and will be sent as soon as it is free."

// ChatQueuedForDialogNotice is the queued notice when the session is held by a
// dialog in the agent's terminal rather than by a turn (code awaiting_approval).
const ChatQueuedForDialogNotice = "Its terminal is showing a prompt that needs an answer — your message is queued and will be sent once it is answered."

// The words a message that waited and was never sent ends with: Error() of a
// chatBusyError. %s: how long it waited.
//
// The hand-back forms are for a route hand-back (session_routes.go), which is
// muster's own text, not something the owner typed — "send it again" is not an
// action the owner can take there (asHandBack).
const (
	chatBusyGaveUpFormat     = "It was still busy with another turn after %s, so this message was not sent. It is kept above — send it again once the agent is free."
	chatBusyReplaced         = "The agent was replaced or restarted while this message waited, so it was not sent. It is kept above — send it again once the agent is up."
	handBackBusyGaveUpFormat = "The chief was still busy with another turn after %s, so this hand-back was not delivered to it."
	handBackBusyReplaced     = "The chief was replaced or restarted while this hand-back waited, so it was not delivered to it."
)

// chatBusyError is a message that waited for the turn in flight and was never
// sent: it waited out the bound, or the agent was replaced or restarted while it
// waited (replaced). Error() is the person's words; Unwrap is the cause (the
// runtime's last refusal, so agentgateway.Unsent still reads it as "nothing was
// sent"; or ErrAgentBeingReplaced).
type chatBusyError struct {
	waited   time.Duration
	cause    error
	replaced bool
	handBack bool
}

func (e *chatBusyError) Error() string {
	switch {
	case e.handBack && e.replaced:
		return handBackBusyReplaced
	case e.handBack:
		return fmt.Sprintf(handBackBusyGaveUpFormat, e.waited.Round(time.Second))
	case e.replaced:
		return chatBusyReplaced
	}
	return fmt.Sprintf(chatBusyGaveUpFormat, e.waited.Round(time.Second))
}
func (e *chatBusyError) Unwrap() error { return e.cause }

// asHandBack is err worded for the route hand-back's door when it is a
// chatBusyError, else err unchanged.
func asHandBack(err error) error {
	var b *chatBusyError
	if !errors.As(err, &b) {
		return err
	}
	c := *b
	c.handBack = true
	return &c
}

func (s *Server) chatBusyBounds() (wait, poll time.Duration) {
	wait, poll = s.chatBusyWait, s.chatBusyPoll
	if wait <= 0 {
		wait = defaultChatBusyWait
	}
	if poll <= 0 {
		poll = defaultChatBusyPoll
	}
	return wait, poll
}

// chatTurnWhenFree is chatTurn, sent again for as long as the runtime refuses it
// with a typed `busy` (agentgateway.TurnInFlight), up to chatBusyWait. Any
// other result — a reply, any other failure, the context ending — returns at
// once. The caller has entered a's turn gate (runEnteredTurn's contract).
//
//   - IN ORDER: every turn joins a's waiter queue (turn_gate.go) and only its
//     oldest asks the runtime, so two messages waiting from different doors (two
//     pages, a page and the composer, a hand-back) are sent in the order they
//     arrived, not whichever poll fires first.
//   - TOLD: while it waits the person is told why, as a "queued" StreamEvent
//     (once per reason), and the notification panel shows "queued"; the moment
//     the runtime takes the text (its 103, or the first stream event) an
//     "accepted" event and the "thinking" phase replace them.
//   - NOT HOLDING THE GATE: asleep between asks it does not block closeFor, so a
//     chief replacement or restart is not refused for up to chatBusyWait; a
//     close wakes it and it gives up (replaced), its "not sent" line stored.
func (s *Server) chatTurnWhenFree(ctx context.Context, a agents.Agent, sessionKey, text string, emit agents.StreamEmit) (string, error) {
	wait, poll := s.chatBusyBounds()
	start := time.Now()
	// 🔴 THE CONVERSATION IS WORKING FOR THE WHOLE WAIT, not only while an ask
	// is open: chatTurn counts its own ask (session_turns.go), and between two
	// asks that count is zero — a page reloaded then would draw no "working…"
	// though a reply is coming. The count nests (one per turn), so this and
	// chatTurn's own are one turn's two holds of the same rule, not two rules.
	defer s.countSessionTurn(a, sessionKey)()
	w := s.turns.join(a.ID)
	defer s.turns.quit(a.ID, w)
	told := ""
	tell := func(notice string) {
		if notice == told {
			return
		}
		told = notice
		s.markAgentActive(requestOwner(ctx), a.Name, "queued")
		if emit != nil {
			emit(agents.StreamEvent{Kind: "queued", Text: notice})
		}
	}
	var lastBusy error
	for {
		if s.turns.first(a.ID, w) {
			actx, aemit := ctx, emit
			if told != "" {
				actx, aemit = s.acceptedAfterQueued(ctx, a, emit)
			}
			reply, err := s.chatTurn(actx, a, sessionKey, text, aemit)
			code, busy := agentgateway.TurnInFlight(err)
			if !busy {
				return reply, err
			}
			lastBusy = err
			if told == "" {
				s.logger.Printf("agents: %s's session is busy (%s); the message waits for it", a.Name, err)
			}
			notice := ChatQueuedNotice
			if code == agentgateway.CodeAwaitingApproval {
				notice = ChatQueuedForDialogNotice
			}
			tell(notice)
		} else {
			// An older message is waiting for the same session: this one waits
			// behind it without asking.
			tell(ChatQueuedNotice)
		}
		waited := time.Since(start)
		if waited >= wait {
			s.logger.Printf("agents: a message to %s waited %s for the session's turn in flight and was not sent: %v",
				a.Name, waited.Round(time.Second), lastBusy)
			return "", &chatBusyError{waited: waited, cause: lastBusy}
		}
		woken := s.turns.sleep(a.ID, w)
		t := time.NewTimer(min(poll, wait-waited))
		select {
		case <-ctx.Done():
		case <-woken:
		case <-t.C:
		}
		t.Stop()
		if s.turns.wakeUp(a.ID, w) {
			s.logger.Printf("agents: %s was closed for a replacement or restart while a message to it waited; not sent", a.Name)
			return "", &chatBusyError{waited: time.Since(start), cause: ErrAgentBeingReplaced, replaced: true}
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
}

// acceptedAfterQueued is ctx and emit for an ask made after the person was told
// the message is queued: the first sign the runtime took the text — ccd's 103
// (agents.WithTurnAccepted, chained to any callback ctx already carries) or the
// turn's first stream event — emits "accepted" and puts the panel back on
// "thinking", once.
func (s *Server) acceptedAfterQueued(ctx context.Context, a agents.Agent, emit agents.StreamEmit) (context.Context, agents.StreamEmit) {
	owner := requestOwner(ctx)
	var once sync.Once
	accepted := func() {
		once.Do(func() {
			s.markAgentActive(owner, a.Name, "thinking")
			if emit != nil {
				emit(agents.StreamEvent{Kind: "accepted"})
			}
		})
	}
	actx := agents.WithTurnAccepted(ctx, func(acc agents.TurnAcceptance) {
		agents.MarkTurnAccepted(ctx, acc) // the callback ctx already carried (markOwnerTurn)
		accepted()
	})
	return actx, func(ev agents.StreamEvent) {
		accepted()
		if emit != nil {
			emit(ev)
		}
	}
}

// storeChatBusyGiveUp stores a busy give-up (chatBusyError) in the conversation
// the way every other door stores a failed turn (turnFailedParts), and does
// nothing for any other error.
//
// 🔴 FOR THE CHAT SOCKET, WHICH OTHERWISE STORES NO FAILURE: it only writes the
// error to the socket. A give-up comes after up to chatBusyWait of waiting, and
// the wait outlives the page (closing the tab does not end it: the socket's
// handler keeps running the turn, TestAQueuedMessageIsDeliveredAfterItsPageIsClosed),
// so the page that sent it is often gone — without this, a reload shows the
// user's line with no answer and nothing saying it was never sent. The composer
// POST and the route hand-back store every failure already, through the same
// turnFailedParts.
//
// ⚠ ONLY THE GIVE-UP, DELIBERATELY: the socket's other failures were never
// stored, and that is a separate decision from this one.
func (s *Server) storeChatBusyGiveUp(ctx context.Context, a agents.Agent, session agents.ChatSession, err error) {
	var busy *chatBusyError
	if !errors.As(err, &busy) {
		return
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	s.persistTurn(sctx, a, session, turnFailedParts(err))
}
