package api

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE COMMAND PALETTE'S SERVER SEARCH (owner answer: "Recent in-page + full on
// typing"). The palette matches what reaches the page at once — names, the
// current focus title, each session's recent previous titles — and, once the
// operator pauses on a query of focus.MinTitleQueryRunes or more, asks here
// for the rest: every focus title in the retention (focus.Store.SearchTitles).
//
// 🔴 OWNER-SCOPED THREE WAYS. The live set is the caller's own entries — what
// the sidebar draws for them (agentsListEntries) — the read runs under the
// caller's principal (row-level security) and names the owner in the query.
// A hit is answered as the HREF of the caller's entry it belongs to, so the
// palette can only ever show an entry already on the caller's page.
// ---------------------------------------------------------------------------

// paletteHit is one session the server search found: its entry's href (the
// one its sidebar row and switcher pill link to) and the title that matched.
type paletteHit struct {
	Href  string `json:"href"`
	Title string `json:"title"`
}

func (s *Server) registerPaletteRoutes(mux Mux) {
	mux.HandleFunc("GET "+ui.PaletteHistoryRoute, s.requireSession(s.handlePaletteHistory))
}

// handlePaletteHistory is GET /ui/palette/history?q=…: {"hits": [...]}, newest
// matching title first. A short query, no focus or agent store, or nothing
// listed answers no hits.
func (s *Server) handlePaletteHistory(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := struct {
		Hits []paletteHit `json:"hits"`
	}{Hits: []paletteHit{}}
	if s.ext.Focus == nil || s.ext.Agents == nil || utf8.RuneCountInString(q) < focus.MinTitleQueryRunes {
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	ctx := r.Context()
	cards, mirrors, err := s.agentsListEntries(ctx)
	if err != nil {
		s.logger.Printf("palette: list: %v", err)
		http.Error(w, "could not search", http.StatusInternalServerError)
		return
	}
	var live focus.Live
	href := map[focus.Subject]string{}
	for _, c := range cards {
		live.Agents = append(live.Agents, c.ID)
		href[focus.AgentSubject(c.ID)] = c.PageHref()
	}
	for _, m := range mirrors {
		if m.FocusKey != "" {
			live.Mirrors = append(live.Mirrors, m.FocusKey)
			href[focus.MirrorSubject(m.FocusKey)] = ui.MirrorCardHref(m)
		}
	}
	if len(href) == 0 {
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	hits, err := s.ext.Focus.SearchTitles(ctx, q, live, s.now().Add(-focus.DefaultHistoryRetention), focus.MaxSearchResults)
	if err != nil {
		s.logger.Printf("palette: search titles: %v", err)
		http.Error(w, "could not search", http.StatusInternalServerError)
		return
	}
	for _, h := range hits {
		if u, ok := href[h.Subject]; ok {
			out.Hits = append(out.Hits, paletteHit{Href: u, Title: h.Title})
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}
