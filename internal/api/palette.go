package api

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE COMMAND PALETTE'S SERVER SEARCH (owner answer: "Recent in-page + full on
// typing"). The palette matches what reaches the page at once — names, the
// current focus title, each session's recent previous titles — and, once the
// operator pauses on a query of focus.MinTitleQueryRunes or more, asks here
// for the rest: every focus title in the retention (focus.Store.SearchTitles).
//
// 🔴 OWNER-SCOPED THREE WAYS. The live set is the caller's own entries — the
// agents and mirrored sessions the sidebar lists for them (paletteTargets) —
// the read runs under the caller's principal (row-level security) and names
// the owner in the query. A hit is answered as the HREF of the caller's entry
// it belongs to, so the palette can only ever show an entry already on the
// caller's page.
// ---------------------------------------------------------------------------

// paletteHit is one entry the server search found: its href (the one its
// sidebar row and switcher pill link to) and the title that matched.
type paletteHit struct {
	Href  string `json:"href"`
	Title string `json:"title"`
}

func (s *Server) registerPaletteRoutes(mux Mux) {
	mux.HandleFunc("GET "+ui.PaletteHistoryRoute, s.requireSession(s.handlePaletteHistory))
}

// handlePaletteHistory is GET /ui/palette/history?q=…: {"hits": [...]}, newest
// matching title first. A query shorter than focus.MinTitleQueryRunes or
// longer than focus.MaxTitleQueryRunes (no stored title is that long), no
// focus or agent store, or nothing listed answers no hits — the first two
// without reading anything.
func (s *Server) handlePaletteHistory(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := struct {
		Hits []paletteHit `json:"hits"`
	}{Hits: []paletteHit{}}
	if n := utf8.RuneCountInString(q); s.ext.Focus == nil || s.ext.Agents == nil || n < focus.MinTitleQueryRunes || n > focus.MaxTitleQueryRunes {
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	ctx := r.Context()
	live, hrefs, err := s.paletteTargets(ctx)
	if err != nil {
		s.logger.Printf("palette: list: %v", err)
		http.Error(w, "could not search", http.StatusInternalServerError)
		return
	}
	if len(hrefs) == 0 {
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	hits, err := s.ext.Focus.SearchTitles(ctx, q, live, s.now().Add(-focus.DefaultHistoryRetention), focus.MaxSearchResults)
	if err != nil {
		s.logger.Printf("palette: search titles: %v", err)
		http.Error(w, "could not search", http.StatusInternalServerError)
		return
	}
	// A subject can be more than one entry: two mirrored windows showing one
	// Claude session each carry its titles, so a hit names every one.
	for _, h := range hits {
		for _, u := range hrefs[h.Subject] {
			out.Hits = append(out.Hits, paletteHit{Href: u, Title: h.Title})
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

// paletteTargets is what the server search needs of the caller's entries, and
// nothing more: the live set (agent ids, mirrored sessions' focus keys) and
// each subject's entry hrefs. It reads what agentsListEntries reads for them —
// the agents (less a chief its owner turned off), their latest sessions (an
// agent's href names it), the mirror hub's windows — and none of the rest
// (pins, focus, attention, transcripts, previous titles).
//
// 🔴 THE HREFS MUST BE THE PAGE'S, byte for byte: the palette finds a hit's
// entry by its href. An agent's is ui.AgentListHref of its name and latest
// session, as cardViewIndexed composes it; a window's is ui.MirrorCardHref
// of its view (mirrorCards). TestThePaletteServerSearchFindsATitleOlderThanThePage
// compares a hit's href with the sidebar row's.
func (s *Server) paletteTargets(ctx context.Context) (focus.Live, map[focus.Subject][]string, error) {
	var live focus.Live
	hrefs := map[focus.Subject][]string{}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return live, nil, err
	}
	// A host officer instance's row is never provisioned and is seen as its
	// window (agents.ProvisionedOnly; the hostRowConsumers ledger).
	list = agents.ProvisionedOnly(list)
	list = s.withoutOffChief(ctx, list)
	ids := make([]int64, 0, len(list))
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	latest := s.latestSessionIDs(ctx, ids)
	for _, a := range list {
		live.Agents = append(live.Agents, a.ID)
		sub := focus.AgentSubject(a.ID)
		hrefs[sub] = append(hrefs[sub], ui.AgentListHref(a.Name, latest[a.ID]))
	}
	for _, m := range s.mirrorCards(ctx, pins.Set{}, nil) {
		if m.FocusKey == "" {
			continue
		}
		sub := focus.MirrorSubject(m.FocusKey)
		if len(hrefs[sub]) == 0 {
			live.Mirrors = append(live.Mirrors, m.FocusKey)
		}
		hrefs[sub] = append(hrefs[sub], ui.MirrorCardHref(m))
	}
	return live, hrefs, nil
}
