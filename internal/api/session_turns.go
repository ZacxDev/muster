package api

import "sync"

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
	if key == "" {
		return func() {}
	}
	c.mu.Lock()
	if c.running == nil {
		c.running = map[string]int{}
	}
	c.running[key]++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.running[key] <= 1 {
				delete(c.running, key)
				return
			}
			c.running[key]--
		})
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
