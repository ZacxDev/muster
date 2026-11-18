package api

import (
	"sync"

	"github.com/ZacxDev/muster/internal/agents"
)

// sessionTurnCount counts the turns running for each chat session, keyed by the
// session's key (agents.ChatSession.SessionKey).
//
// 🔴 IT IS WHAT A PAGE OPENED MID-TURN READS. The "working…" row in #chat-log
// used to exist only in the browser that sent the message: shown on send,
// hidden on the socket's "done". A reload, or opening the chief panel on another
// device, drew it hidden while the turn was still running, and the reply then
// arrived out of nowhere. The page now draws the row from this count
// (ui.AgentDetailView.TurnInFlight), and the chat.reply refetch that brings the
// reply redraws it from the count again, which is zero by then.
//
// ⚠ WHY NOT turnGate OR activeAgents. turnGate counts per AGENT, so a turn in
// one thread would draw "working…" in every other thread of the same agent;
// activeAgents is keyed by name and outlives the turn by design (the notification
// panel's "Xm ago"). Neither answers "is THIS conversation waiting on a reply".
//
// ⚠ IN-PROCESS ONLY, like turnGate: muster runs one replica, and a turn does not
// outlive the process that runs it, so a restart losing the count loses nothing
// that was still true.
type sessionTurnCount struct {
	mu      sync.Mutex
	running map[string]int
}

// begin counts one turn for key and returns its end, which is safe to call more
// than once. An empty key counts nothing.
func (c *sessionTurnCount) begin(key string) (end func()) {
	_, end2 := c.beginEdge(key)
	return func() { end2() }
}

// beginEdge is begin, also saying whether this turn is the FIRST running for
// key (first) and, from its end, whether it was the LAST (last): the two moments
// a conversation starts and stops working, which open pages are told
// (Server.countSessionTurn). end reports last true at most once, from the call
// that actually ended the turn.
func (c *sessionTurnCount) beginEdge(key string) (first bool, end func() (last bool)) {
	if key == "" {
		return false, func() bool { return false }
	}
	c.mu.Lock()
	if c.running == nil {
		c.running = map[string]int{}
	}
	c.running[key]++
	first = c.running[key] == 1
	c.mu.Unlock()
	var once sync.Once
	return first, func() (last bool) {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.running[key] <= 1 {
				delete(c.running, key)
				last = true
				return
			}
			c.running[key]--
		})
		return last
	}
}

// inFlight reports whether a turn is running for key.
func (c *sessionTurnCount) inFlight(key string) bool {
	if key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running[key] > 0
}

// countSessionTurn counts one turn running in agent a's session sessionKey
// (sessionTurns) and returns its end, safe to call more than once.
//
// 🔴 THE FIRST TURN TO START IN A CONVERSATION AND THE LAST TO END EACH TELL THE
// OWNER'S OPEN PAGES (EventChatTurn), and that is what makes "working…" show on a
// chat that did not start the turn: another tab, `muster chief ask` (the machine
// door), a plugin delivery, a hand-back. Each #chat-log of a's refetches on it
// (ui.chatLogTrigger) and draws the row from this count — showing on the start,
// hidden (with whatever was stored) on the end. Before, only the page that sent
// the message drew the row; every other open chat learned of the turn only from
// its reply.
//
// 🔴 EVERY DOOR THAT RUNS A TURN IN A STORED SESSION COUNTS THROUGH HERE: chatTurn,
// chatTurnWhenFree, startAgentTurn, the chat socket (from the moment it reads the
// message) and gatewayTurn (the machine door). TestEveryTurnDoorCountsTheSession
// pins the set.
func (s *Server) countSessionTurn(a agents.Agent, sessionKey string) (end func()) {
	first, last := s.sessionTurns.beginEdge(sessionKey)
	if first {
		s.broadcastTo(a.OwnerID, EventChatTurn, a.Name)
	}
	return func() {
		if last() {
			s.broadcastTo(a.OwnerID, EventChatTurn, a.Name)
		}
	}
}
