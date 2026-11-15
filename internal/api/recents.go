package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE "RECENTLY VIEWED" STRIP's server half (internal/recents, migration 0018).
//
// A visit is recorded by the three pages an entry opens — an agent's detail
// page, a mirrored window's rendered view and its terminal page — once the
// request has resolved to the page actually rendered (never on a redirect, and
// never for an entry the viewer cannot see: those answered 404 before here).
// There is no route of its own.
//
// 🔴 BEST-EFFORT, AND IT NEVER FAILS THE PAGE. A visit that cannot be stored is
// logged with the ids only and the page renders anyway.

// recordAgentVisit records that the viewer opened agent a.
//
// 🔴 NOT FOR A TEAM'S AGENT OPENED BY A MEMBER (memberOnTeamAgent). A recents
// row is the viewer's own, and its owner must own the agent it names (0018's
// trigger): a member's visit to the team chief was refused by Postgres on every
// page load (SQLSTATE 23514) and logged as an error each time. The row is not
// written under the team either — the team cannot sign in, so it has no strip
// to show it in, and a row there would record which member looked at what in a
// table every member's door can reach. The team agent stays in the member's
// sidebar under its team; it is simply not in their "recently viewed".
func (s *Server) recordAgentVisit(ctx context.Context, a agents.Agent) {
	if s.ext.Recents == nil || memberOnTeamAgent(ctx, a) {
		return
	}
	if err := s.ext.Recents.VisitAgent(ctx, a.ID); err != nil {
		s.logger.Printf("recents: visit agent %d: %v", a.ID, err)
	}
}

// recordMirrorVisit records that the viewer opened the mirrored entry at
// (host, session, window) — the address its sidebar row links by.
func (s *Server) recordMirrorVisit(ctx context.Context, host, session, window string) {
	if s.ext.Recents == nil {
		return
	}
	if err := s.ext.Recents.VisitMirror(ctx, pins.MirrorKey{Host: host, Session: session, Window: window}); err != nil {
		s.logger.Printf("recents: visit mirror host_len=%d session_len=%d window=%q: %v", len(host), len(session), window, err)
	}
}

// recentRefs is the viewer's recently opened entries, newest first, for the
// session switcher's Recent view; ui.NewSwitcher drops those that are gone and
// keeps its cap. Every stored entry is read so that dropping cannot leave the
// view short while older entries exist. No store, or a failed read (logged),
// is "nothing recent".
func (s *Server) recentRefs(ctx context.Context) []ui.RecentRef {
	if s.ext.Recents == nil {
		return nil
	}
	es, err := s.ext.Recents.List(ctx)
	if err != nil {
		s.logger.Printf("recents: list: %v", err)
		return nil
	}
	out := make([]ui.RecentRef, 0, len(es))
	for _, e := range es {
		if e.IsMirror() {
			out = append(out, ui.RecentRef{Mirror: ui.MirrorRef{Host: e.Mirror.Host, Session: e.Mirror.Session, Window: e.Mirror.Window}})
		} else {
			out = append(out, ui.RecentRef{AgentID: e.AgentID})
		}
	}
	return out
}

// switcher is the viewer's session switcher for the page of cur (ui.Switcher):
// the entries the sidebar lists, with no live instance read (the bar shows
// names, not status) — and the viewer's recently viewed refs, resolved by
// ui.NewSwitcher. nil — no bar — when
// there is no agent store, or the list cannot be read (logged): the page
// renders without it rather than failing.
func (s *Server) switcher(ctx context.Context, cur ui.SwitcherCurrent) *ui.Switcher {
	if s.ext.Agents == nil {
		return nil
	}
	cards, mirrors, err := s.agentsListEntries(ctx)
	if err != nil {
		s.logger.Printf("switcher: list: %v", err)
		return nil
	}
	cur.Now = s.now() // one clock for the focus ages (ui.FocusAgeOf)
	return ui.NewSwitcher(cards, mirrors, s.recentRefs(ctx), cur)
}
