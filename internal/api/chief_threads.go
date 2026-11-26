package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE CHIEF'S MESSAGE SEARCH (the Sessions tab's "Matches in messages", ui
// chiefSessionsTab), on the chief page and in the chief panel. It was the chief
// panel's thread-list search; the list was folded into the Sessions tab (an
// operator decision), which filters its rows by title in the browser and asks
// this route only for the words in the chats' MESSAGES.
//
// 🔴 OPERATOR-ONLY, AND THAT IS THE WHOLE SECURITY STATEMENT OF THIS FILE. The
// response carries MESSAGE CONTENT — a snippet cut out of a chat_messages body —
// which is the exact sensitivity a neighbouring PR is being held over. So it is
// registered behind requireSession (a signed operator session cookie) and behind
// NOTHING ELSE: not the shared hook token, not a per-agent hooks token. The two
// non-operator credentials that reach other routes in this package must get a
// 401 here, and api.TestChiefThreadSearchIsOperatorOnly asserts both of them.
//
// 🔴 IT SEARCHES kind='text' ONLY. Store.SearchSessions carries the reason at
// length; the short version is that tool_result rows are file contents and
// command output, so including them would make every query match noise AND would
// paste raw file bytes on screen as a snippet.
//
// ⚠ IT IS SCOPED TO THE CHIEF'S OWN THREADS. The tab lists one agent's chats, so
// the search takes that agent's id and no caller can widen it — a route keyed on
// an arbitrary agent is what got GET /api/agents/{name}/messages narrowed.
// ---------------------------------------------------------------------------

// handleChiefThreadSearch serves GET /ui/chief/threads/search: the chief's chats
// whose MESSAGES hold ?q= (ui.RenderChiefMessageMatches), linked the way
// ?surface= switches (chief-panel: the panel's body; anything else: the chief's
// page). A blank query answers nothing: the tab's rows are the list.
func (s *Server) handleChiefThreadSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	surface := ui.ChatSurfacePage
	if r.URL.Query().Get(ui.ChatSurfaceQueryParam) == string(ui.ChatSurfaceChiefPanel) {
		surface = ui.ChatSurfaceChiefPanel
	}
	write := func(v ui.ChiefMessageMatchesView) {
		v.Surface = surface
		if err := ui.RenderChiefMessageMatches(w, v); err != nil {
			s.logger.Printf("chief: render message matches: %v", err)
		}
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		return
	}

	// 🔴 EVERY FAILURE RENDERS AS A SENTENCE, NEVER AS AN HTTP ERROR — htmx does not
	// swap a non-2xx body, so a 500 would leave the previous matches on screen as
	// though they were the answer — AND THE SENTENCE SAYS IT IS A FAILURE: a
	// Postgres error (a NUL byte in `q` is enough) or a chief gone missing drawn as
	// "no message matched" would be a verdict on a search nothing performed. The
	// rendered answer names its state in an attribute, so the guard asserts the
	// STATE rather than the words.
	failed := func() { write(ui.ChiefMessageMatchesView{Failed: true}) }

	// 🔴 A NO-DATABASE BOOT IS NOT A READ THAT MIGHT WORK NEXT TIME, AND THE DEFAULT
	// FAILURE SENTENCE ENDS "Try again." There is no store to ask, so the sentence
	// says so instead. ⚠ Reachable only by hitting this route directly: a
	// no-database chief page or panel draws no Sessions tab to search from.
	if s.ext.Agents == nil {
		write(ui.ChiefMessageMatchesView{Failed: true,
			Reason: "This muster is running without a database, so it has no messages to search."})
		return
	}
	a, err := s.chiefAgent(ctx)
	if err != nil {
		if !errors.Is(err, ErrNoChief) {
			s.logger.Printf("chief: message search resolve agent: %v", err)
		}
		// ⚠ ErrNoChief IS A FAILURE *HERE*: this route answers a keystroke in a tab
		// that has ALREADY resolved a chief and drawn its chats.
		failed()
		return
	}

	// 🔴 THE CHAT ON SCREEN IS READ FROM THE QUERY STRING AND resolveSession IS NOT
	// CALLED, DELIBERATELY. resolveSession ends in LatestOrCreateSession, which
	// CREATES a chat_sessions row when the agent has none — so wiring it in here
	// would make a keystroke in a search box a write.
	activeID, _ := strconv.ParseInt(r.URL.Query().Get("session"), 10, 64)

	matches, err := s.ext.Agents.SearchSessions(ctx, a.ID, q, agents.SearchSessionsMaxLimit)
	if err != nil {
		s.logger.Printf("chief: message search %q: %v", q, err)
		failed()
		return
	}
	found := make([]agents.ChatSession, 0, len(matches))
	for _, m := range matches {
		found = append(found, m.ChatSession)
	}
	views := s.sessionViews(ctx, a, found, activeID)
	for i, m := range matches {
		views[i].Snippet = m.Snippet
	}
	write(ui.ChiefMessageMatchesView{AgentName: a.Name, Matches: views})
}
