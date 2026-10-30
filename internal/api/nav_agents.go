package api

import (
	"fmt"
	"net/http"

	"github.com/ZacxDev/muster/internal/ui"
)

// THE SIDEBAR'S AGENT LIST (internal/ui/nav_agents.go) and HOME, which opens
// the chief's chat.

// handleNavAgents serves GET ui.NavAgentsPath: the sidebar's rows, built the
// way the session switcher builds its entries (agentsListEntries with no
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
	cards, mirrors, err := s.agentsListEntries(r.Context())
	if err != nil {
		s.logger.Printf("nav agents: list: %v", err)
		http.Error(w, "could not load agents", http.StatusInternalServerError)
		return
	}
	if err := ui.RenderNavAgents(w, cards, mirrors, r.Header.Get("HX-Current-URL")); err != nil {
		s.logger.Printf("nav agents: render: %v", err)
	}
}

// homeTarget is where home — `/`, and GET /agents — sends the viewer, "" for
// nowhere (the onboarding page, handleAgentsHome):
//
//  1. the chief agent's chat (its page on its latest session — the link a list
//     draws, agentPageHref), when the viewer has a chief and it is on (chiefOn);
//  2. otherwise the session the viewer opened last that is still listed, else
//     the sidebar's first row (ui.HomeHref, over the entries the sidebar and
//     the switcher are built from);
//  3. "" when there is nothing to open — no agent store, no principal, no
//     entry at all.
//
// except leaves one session out (homeExcept's zero value: none): the page the
// viewer is leaving (`?leave…`, ui.HomeLeavingPath / HomeLeavingMirrorPath),
// or the agent a DELETE just started tearing down, whose row outlives the
// response by a moment — home must not be where the leaving page lands.
//
// The error is a failed read; the caller decides what to draw for it (the
// redirect falls back to /agents, which reports it).
//
// 🔴 THE CHIEF IS primaryChief — THE SIDEBAR'S, NOT chiefAgent's. chiefAgent
// knows only the "chief" label; the sidebar pins a relabelled reserved slug
// too (chiefPredicate). `/` used chiefAgent, so it opened the Agents list while
// the sidebar's first row was that chief. TestHomeAndTheSidebarAgreeOnTheChief.
// As chiefAgent does, a principal-less or scope-all context has no chief.
//
// 🔴 HOME IS A CONVERSATION, NOT A LIST (operator decision: the Agents list
// page is retired, and "the default view open[s] to the chief"). The board
// stays at /tasks, and `/?shell=1` — the service worker's offline copy
// (offlineShellParam) — still serves the shell, which draws the Tasks tab
// there (defaultTab).
func (s *Server) homeTarget(r *http.Request, except homeExcept) (string, error) {
	ctx := r.Context()
	if s.ext.Agents == nil || requestOwner(ctx) <= 0 {
		return "", nil
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return "", fmt.Errorf("agent list: %w", err)
	}
	if chief, ok := primaryChief(list); ok && chief.ID != except.agent {
		// A chief its owner turned off is not home (chief_toggle.go).
		on, err := s.chiefOn(ctx, requestOwner(ctx), list)
		if err != nil {
			s.logger.Printf("home: read the chief toggle: %v (opening the last session instead)", err)
		}
		if on {
			return s.agentPageHref(ctx, chief), nil
		}
	}
	cards, mirrors, err := s.agentsListEntries(ctx)
	if err != nil {
		return "", fmt.Errorf("entries: %w", err)
	}
	if except.agent != 0 {
		kept := cards[:0]
		for _, c := range cards {
			if c.ID != except.agent {
				kept = append(kept, c)
			}
		}
		cards = kept
	}
	if except.mirror.Host != "" {
		kept := mirrors[:0]
		for _, m := range mirrors {
			if (ui.MirrorRef{Host: m.Host, Session: m.Session, Window: m.Window}) != except.mirror {
				kept = append(kept, m)
			}
		}
		mirrors = kept
	}
	return ui.HomeHref(cards, mirrors, s.recentRefs(ctx)), nil
}

// redirectHome answers a request for `/` with homeTarget — or /agents, which
// draws the onboarding page, when there is nowhere to go or the read failed
// (handleAgentsHome reads again and says which): a 302 to a browser, and — for
// an htmx request (a boosted link to `/`, the header's wordmark) — a 200
// carrying HX-Redirect, because htmx follows a 3xx inside its XHR and would
// swap the target's body under a `/` left in the address bar.
func (s *Server) redirectHome(w http.ResponseWriter, r *http.Request) {
	to, err := s.homeTarget(r, s.leaving(r))
	if err != nil {
		s.logger.Printf("home: %v (opening /agents)", err)
	}
	if to == "" {
		// The onboarding page's URL, carrying the `leave…` query along: it
		// computes home again, and must leave the same session out.
		to = ui.HomeFallbackPath
		if r.URL.RawQuery != "" && s.leaving(r) != (homeExcept{}) {
			to += "?" + r.URL.RawQuery
		}
	}
	s.redirectTo(w, r, to)
}

// homeExcept is the one session home leaves out (homeTarget): an agent by id,
// or a mirrored window by its address. The zero value leaves nothing out.
type homeExcept struct {
	agent  int64
	mirror ui.MirrorRef
}

// leaving is the session `/?leave…` asks home to leave out — the page the
// viewer is leaving: an agent by name (ui.HomeLeavingPath; none when no agent
// of that name is readable, or the read fails) or a mirrored window by its
// address (ui.HomeLeavingMirrorPath).
func (s *Server) leaving(r *http.Request) homeExcept {
	q := r.URL.Query()
	if host := q.Get(ui.HomeLeaveHostParam); host != "" {
		return homeExcept{mirror: ui.MirrorRef{Host: host, Session: q.Get(ui.HomeLeaveSessionParam), Window: q.Get(ui.HomeLeaveWindowParam)}}
	}
	name := q.Get(ui.HomeLeaveParam)
	if name == "" || s.ext.Agents == nil {
		return homeExcept{}
	}
	a, err := s.ext.Agents.GetByName(r.Context(), name)
	if err != nil {
		return homeExcept{}
	}
	return homeExcept{agent: a.ID}
}

// redirectTo is redirectHome's answer for a destination: no copy kept (where
// home goes depends on who the chief is NOW), HX-Redirect for htmx, else 302.
func (s *Server) redirectTo(w http.ResponseWriter, r *http.Request, to string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// handleAgentsHome serves GET /agents, the retired Agents list page's URL:
// every link to it — an old bookmark, a PWA shortcut on a phone, a
// notification, a `?filter=` link the list once wrote — lands where home does
// (homeTarget), its query dropped; with nowhere to go it draws the onboarding
// page (ui.RenderAgentsHome) instead of redirecting, so `/` → /agents ends
// here rather than looping. A failed read is a 500 that says so, never the
// "No agents yet" page over agents that exist.
func (s *Server) handleAgentsHome(w http.ResponseWriter, r *http.Request) {
	to, err := s.homeTarget(r, s.leaving(r))
	if to != "" {
		s.redirectTo(w, r, to)
		return
	}
	if err != nil {
		s.logger.Printf("agents home: %v", err)
		http.Error(w, "could not load your agents", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderAgentsHome(w, s.shellFeatures(r.Context())); err != nil {
		s.logger.Printf("agents home: render: %v", err)
	}
}
