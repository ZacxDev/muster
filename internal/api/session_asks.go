package api

import (
	"context"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// A SESSION'S ASKS IN ITS OPEN PANEL (operator ask: "When an ask is
// associated with a session, we should display a way to view them from the
// session page the same way we already do for pages and previews"): the
// session view's Open button counts the session's OPEN asks with its previews
// and pages, and its panel's Asks tab lists the session's asks, open ones
// first, then the answered ones with their answer, each linking to its own
// page (/attention/asks/{id}).
//
// 🔴 A TAB OF THE OPEN PANEL, NOT A BLOCK ON THE PAGE (attention_asker.go's
// operator decision, NO BLOCK ON THE SESSION PAGE): nothing is drawn on the
// session page until the panel is opened, and nothing in the tab answers an
// ask: an ask is answered on its own page, its card or the Asks page.
//
// WHOSE: an agent's page lists the asks whose asker IS that agent; a window
// lists the asks of every MCP caller key it runs now (mirror.Window.
// MCPCallerKeys, the rule pages use: windowPagesRef), on its host. The
// store names the owner (attention.SessionAsks): a team's ask, readable to
// a member through row-level security, is never listed on the member's own
// window. A TEAM host's window lists none, as it lists no pages.
// ---------------------------------------------------------------------------

// windowAsksQuery is what a mirrored window's asks are listed by: its host and
// every MCP caller key it runs now, read from h's report of the window. None
// on a team host's window (e.Owner != 0), and none for a window that runs no
// session muster can name.
func windowAsksQuery(h mirror.HostView, e mirror.SessionView) attention.SessionAsksQuery {
	if e.Owner != 0 || e.Window == "" {
		return attention.SessionAsksQuery{}
	}
	for _, w := range h.Windows {
		if w.ID == e.Window && w.Session == e.Session {
			return attention.SessionAsksQuery{MirrorHost: h.Host, MirrorSessions: w.MCPCallerKeys()}
		}
	}
	return attention.SessionAsksQuery{}
}

// asksOn reports whether the session views list asks: the queue is served.
func (s *Server) asksOn() bool { return s.ext.Attention != nil }

// sessionAsks is ctx's principal's own asks raised by the session q names,
// open first; nil with the queue off, for a q that names no session, or on a
// store error (logged: the tab then says the session asked nothing, and the
// button counts none).
func (s *Server) sessionAsks(ctx context.Context, q attention.SessionAsksQuery) []attention.Ask {
	if !s.asksOn() || !q.Valid() {
		return nil
	}
	list, err := s.ext.Attention.SessionAsks(ctx, q)
	if err != nil {
		s.logger.Printf("attention: a session's asks: %v", err)
		return nil
	}
	return list
}

// openAskCount is how many of list are open.
func openAskCount(list []attention.Ask) int {
	n := 0
	for _, a := range list {
		if a.Status == attention.Open {
			n++
		}
	}
	return n
}

// sessionAsksView is list as the Asks tab draws it: each ask linked to its
// own page where ctx's principal may open it (askHasPage, the rule every
// card that links one keeps), asked when in words (ui.PlainWhen).
func sessionAsksView(ctx context.Context, list []attention.Ask) []ui.SessionAsk {
	now, viewer := time.Now(), requestOwner(ctx)
	out := make([]ui.SessionAsk, 0, len(list))
	for _, a := range list {
		v := ui.SessionAsk{ID: a.ID, Question: a.Question, Open: a.Status == attention.Open, Answer: a.Answer, When: ui.PlainWhen(a.CreatedAt, now)}
		if askHasPage(a, viewer) {
			v.Page = askPagePath(a.ID)
		}
		out = append(out, v)
	}
	return out
}

// handleSessionAsks serves GET ui.SessionAsksPath: a mirrored window's Asks
// tab, the viewer's own asks of the sessions it runs. An unknown window, a
// team host's, and the queue off each answer the empty list.
func (s *Server) handleSessionAsks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	q := r.URL.Query()
	var list []attention.Ask
	if h, e, listed, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window")); ok && listed {
		list = s.sessionAsks(r.Context(), windowAsksQuery(h, e))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.SessionAsksList(sessionAsksView(r.Context(), list)).Render(w); err != nil {
		s.logger.Printf("session asks: render: %v", err)
	}
}

// handleAgentSessionAsks serves GET ui.AgentSessionAsksPath: an agent page's
// Asks tab, the agent's asks; 404 for an agent the viewer does not see
// (another owner's reads as a missing one).
func (s *Server) handleAgentSessionAsks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	a, ok := s.agentOfPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	list := s.sessionAsks(r.Context(), attention.SessionAsksQuery{AgentID: a})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.SessionAsksList(sessionAsksView(r.Context(), list)).Render(w); err != nil {
		s.logger.Printf("session asks: render: %v", err)
	}
}
