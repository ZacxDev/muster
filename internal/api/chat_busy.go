package api

import (
	"context"
	"fmt"
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

// chatBusyGaveUpFormat is the error a message that waited out chatBusyWait
// returns (%s: how long it waited).
const chatBusyGaveUpFormat = "It was still busy with another turn after %s, so this message was not sent. It is kept above — send it again once the agent is free."

// chatBusyError is a message that waited out the bound: Error() is the person's
// words; Unwrap is the runtime's last refusal, so agentgateway.Unsent still
// reads it as "nothing was sent".
type chatBusyError struct {
	waited time.Duration
	cause  error
}

func (e *chatBusyError) Error() string {
	return fmt.Sprintf(chatBusyGaveUpFormat, e.waited.Round(time.Second))
}
func (e *chatBusyError) Unwrap() error { return e.cause }

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
// with a typed `busy` (agentgateway.TurnInFlight), up to chatBusyWait. Each time
// the reason changes the person is told what it is waiting for, as a "queued"
// StreamEvent (emit is nil-safe). Any other result — a reply, any other failure,
// the context ending — returns at once.
func (s *Server) chatTurnWhenFree(ctx context.Context, a agents.Agent, sessionKey, text string, emit agents.StreamEmit) (string, error) {
	wait, poll := s.chatBusyBounds()
	start := time.Now()
	told := ""
	for {
		reply, err := s.chatTurn(ctx, a, sessionKey, text, emit)
		code, busy := agentgateway.TurnInFlight(err)
		if !busy {
			return reply, err
		}
		waited := time.Since(start)
		if waited >= wait {
			s.logger.Printf("agents: a message to %s waited %s for the session's turn in flight and was not sent: %v",
				a.Name, waited.Round(time.Second), err)
			return "", &chatBusyError{waited: waited, cause: err}
		}
		notice := ChatQueuedNotice
		if code == agentgateway.CodeAwaitingApproval {
			notice = ChatQueuedForDialogNotice
		}
		if notice != told {
			told = notice
			s.logger.Printf("agents: %s's session is busy (%s); the message waits for it", a.Name, err)
			if emit != nil {
				emit(agents.StreamEvent{Kind: "queued", Text: notice})
			}
		}
		t := time.NewTimer(min(poll, wait-waited))
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
	}
}
