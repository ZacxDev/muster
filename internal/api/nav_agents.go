package api

import (
	"net/http"

	"github.com/ZacxDev/muster/internal/ui"
)

// THE SIDEBAR'S AGENT LIST (internal/ui/nav_agents.go) and HOME, which opens
// the chief's chat.

// handleNavAgents serves GET ui.NavAgentsPath: the sidebar's rows, built the
// way the session switcher builds its entries (agentsListCardsLive with no
// live instance read — the rows show names, recency and attention, never a
// pod's status, so no request waits on the apiserver). The row of the page the
// request came from is marked: htmx names that page in HX-Current-URL.
//
// No agent store: the empty list, not an error — the sidebar is on every
// page, and a deployment without agents has nothing to list. A failed read is
// a 500 the slot keeps its previous rows through (htmx swaps no error).
func (s *Server) handleNavAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// It is this user's list, read on every load: no shared or stored copy.
	w.Header().Set("Cache-Control", "private, no-store")
	if s.ext.Agents == nil {
		if err := ui.RenderNavAgents(w, nil, nil, ""); err != nil {
			s.logger.Printf("nav agents: render: %v", err)
		}
		return
	}
	cards, mirrors, err := s.agentsListCardsLive(r.Context(), 0, false)
	if err != nil {
		s.logger.Printf("nav agents: list: %v", err)
		http.Error(w, "could not load agents", http.StatusInternalServerError)
		return
	}
	if err := ui.RenderNavAgents(w, cards, mirrors, r.Header.Get("HX-Current-URL")); err != nil {
		s.logger.Printf("nav agents: render: %v", err)
	}
}

// homeTarget is where `/` sends the viewer: the chief agent's chat (its page
// on its latest session — the link a list draws, agentPageHref), or the Agents
// list when the viewer has no chief (no agent store, no chief, or a read that
// failed: logged).
//
// 🔴 THE CHIEF IS primaryChief — THE SIDEBAR'S, NOT chiefAgent's. chiefAgent
// knows only the "chief" label; the sidebar pins a relabelled reserved slug
// too (chiefPredicate). `/` used chiefAgent, so it opened the Agents list while
// the sidebar's first row was that chief. TestHomeAndTheSidebarAgreeOnTheChief.
// As chiefAgent does, a principal-less or scope-all context has no chief.
//
// 🔴 `/` IS NOT THE TASKS BOARD ANY MORE (operator decision: home opens the
// chief's chat). The board stays at /tasks, and `/?shell=1` — the service
// worker's offline copy (offlineShellParam) — still serves the shell, which
// draws the Tasks tab there (defaultTab).
func (s *Server) homeTarget(r *http.Request) string {
	ctx := r.Context()
	if s.ext.Agents == nil || requestOwner(ctx) <= 0 {
		return "/agents"
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		s.logger.Printf("home: agent list: %v (opening the Agents list)", err)
		return "/agents"
	}
	chief, ok := primaryChief(list)
	if !ok {
		return "/agents"
	}
	return s.agentPageHref(ctx, chief)
}

// redirectHome answers a request for `/` with homeTarget: a 302 to a browser,
// and — for an htmx request (a boosted link to `/`, the header's wordmark) —
// a 200 carrying HX-Redirect, because htmx follows a 3xx inside its XHR and
// would swap the target's body under a `/` left in the address bar.
func (s *Server) redirectHome(w http.ResponseWriter, r *http.Request) {
	to := s.homeTarget(r)
	// Where home goes depends on who the chief is NOW: no copy may be kept.
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}
