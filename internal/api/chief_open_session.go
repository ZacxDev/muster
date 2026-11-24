package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
)

// WHERE THE CHIEF OPENS (docs/chief.md#which-session-the-chief-opens-on).
//
// 🔴 ON THE CHAT ITS OWNER WAS ON LAST, ON EITHER SURFACE. The chief page
// (/agents/<chief>, its sidebar row, home `/`) and the chief panel used three
// different rules: a bare page opened the PRIMARY, the sidebar row and home
// named the chat with the latest updated_at — moved by every message, so by
// every delivery to the primary — and the panel kept its own memory in
// localStorage that the page never read. An owner reading a side chat who left
// and came back by any of them was put somewhere else. Now one record,
// chat_sessions.viewed_at (agents.SessionViewStore), is written where a chat is
// drawn for its owner (markSessionViewed) and read here, by every way in that
// does not name a chat itself.
//
// 🔴 A ?session= THAT NAMES A CHAT STILL WINS (a notification's link, a row
// tapped in the drawer): resolveSessions honours a valid one before it asks
// this. A LIST link (ui.AgentListHref, &from=list) names whatever was latest
// when the list was drawn, so for the chief it is re-resolved here
// (handleAgentDetail).
//
// The memory is SERVER-SIDE, per chief (and so per owner: a chief is its
// owner's, and a team's chief is one shared room), not per browser: the phone
// and the desktop open on the same chat, and no surface keeps a copy of its
// own to disagree with.

// sessionViewStore is the store's 0187 half, when it has one.
func (s *Server) sessionViewStore() (agents.SessionViewStore, bool) {
	st, ok := s.ext.Agents.(agents.SessionViewStore)
	return st, ok
}

// markSessionViewed records that chat sid was drawn for its owner — and read,
// as MarkSessionRead does. A store without the 0187 half only marks it read.
func (s *Server) markSessionViewed(ctx context.Context, sid int64) {
	var err error
	if st, ok := s.sessionViewStore(); ok {
		err = st.MarkSessionViewed(ctx, sid)
	} else {
		err = s.ext.Agents.MarkSessionRead(ctx, sid)
	}
	if err != nil {
		s.logger.Printf("agents: mark session %d viewed: %v", sid, err)
	}
}

// chiefOpenSession is the chat the chief a opens on when nothing names one:
// the session its owner viewed last, unless that one is archived — then, as
// for a chief nobody has opened yet or a failed read, its primary.
//
// ⚠ A DELETED SESSION has no row, so the latest of the rest is the answer: the
// chat viewed before it, which still exists.
func (s *Server) chiefOpenSession(ctx context.Context, a agents.Agent) (agents.ChatSession, error) {
	if st, ok := s.sessionViewStore(); ok {
		sess, found, err := st.LastViewedSession(ctx, a.ID)
		if err != nil {
			s.logger.Printf("agents: last viewed session of %d: %v (opening the primary)", a.ID, err)
		}
		if err == nil && found && sess.AgentID == a.ID && sess.ArchivedAt == nil {
			return sess, nil
		}
	}
	return s.primarySession(ctx, a)
}
