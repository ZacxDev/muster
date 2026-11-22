package api

import (
	"context"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SESSION VIEWS' PREVIEW BUTTON (docs/plugins.md#the-session-view-chip;
// docs/design/previews.md seam (b); ui/plugin_chip.go): on a mirrored window's
// two session views (/mirror/view and /mirror/terminal), a header button that
// opens the owner's live viewer plugin in a panel — drawn only while the
// window HAS SOMETHING TO PREVIEW (its host's mirror.WindowPreviews). Its
// panel and New tab mint a hand-off (POST /plugin-handoffs) at click time;
// nothing is minted to draw it.
//
// 🔴 TWO GATES, BOTH REQUIRED. (1) viewerPlugins — the token rows, only while
// they would mint: a revoked token, one without viewer, and one whose origin
// is no longer in MUSTER_PLUGIN_ORIGINS (the mint's own re-check,
// ValidViewerLink) give nothing; with MUSTER_PLUGIN_ORIGINS unset, or plugins
// off, nothing is read at all: no slot, no poll and no frame-src. (2) the
// window's counts, which only NARROW (1): the slot and the frame-src follow
// (1) alone, so a button that appears after the page loaded can frame its
// plugin; the button itself is drawn only while the counts are above zero.
// viewerPlugins is the principal's live viewer plugins whose link the CURRENT
// MUSTER_PLUGIN_ORIGINS still allows, newest first (the store's List order);
// nil when the seam is off or the window has no tmux id a hand-off could name.
func (s *Server) viewerPlugins(ctx context.Context, window string) []plugins.Plugin {
	if mirror.ValidWindowID(window) != nil {
		return nil
	}
	return s.liveViewerPlugins(ctx)
}

// liveViewerPlugins is viewerPlugins for no particular window: the one gate
// the session views (through viewerPlugins) and the sidebar's preview mark
// (mirrorCards) share. With plugins off or MUSTER_PLUGIN_ORIGINS unset nothing
// is read.
//
// They are ctx's principal's OWN (the store's one owner filter,
// plugins.ownedByPrincipal): a person's under their session, a team's under
// the team acting principal (teamViewerPlugins) — never a team's on a
// member's own window.
func (s *Server) liveViewerPlugins(ctx context.Context) []plugins.Plugin {
	if s.ext.Plugins == nil || len(s.ext.PluginOrigins) == 0 {
		return nil
	}
	all, err := s.ext.Plugins.List(ctx)
	if err != nil {
		s.logger.Printf("plugins: list for the session-view chip: %v", err)
		return nil
	}
	var out []plugins.Plugin
	for _, p := range all {
		if p.RevokedAt != nil || !p.Has(plugins.CapViewer) || p.Origin == nil || p.EntryURL == nil {
			continue
		}
		if _, err := plugins.ValidViewerLink(s.ext.PluginOrigins, *p.Origin, *p.EntryURL); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// viewerFrameSrc is a session view's frame-src: 'self' (the Rendered |
// Terminal switch frames the other view of this origin, ui/view_swap.go) and
// each plugin's origin once. "" for no plugins, so the page's CSP is exactly
// what it was.
func viewerFrameSrc(ps []plugins.Plugin) string {
	if len(ps) == 0 {
		return ""
	}
	src := []string{"'self'"}
	seen := map[string]bool{}
	for _, p := range ps {
		if o := *p.Origin; !seen[o] {
			seen[o] = true
			src = append(src, o)
		}
	}
	return strings.Join(src, " ")
}

// pluginChips is what the page draws for ps, aimed at e's window, with its
// counts now, pages — the window's session's pages (sessionPages) — for a
// pages feature that is on (pagesOn), and asks — the window's sessions' asks
// (sessionAsks, session_asks.go) — while the queue is served (asksOn). nil
// when there is neither a plugin to open nor pages or asks to list.
func pluginChips(ps []plugins.Plugin, e mirror.SessionView, pagesOn bool, pages int, asksOn bool, openAsks, asks int, viewer int64) *ui.PluginChips {
	if len(ps) == 0 && !pagesOn && !asksOn {
		return nil
	}
	c := &ui.PluginChips{MintPath: pluginHandoffsPath, Owner: e.Owner, Host: e.Host, Session: e.Session, Window: e.Window, Previews: e.Previews,
		PagesOn: pagesOn, Pages: pages, AsksOn: asksOn, Asks: openAsks, AsksListed: asks, Viewer: viewer}
	for _, p := range ps {
		label := p.Label
		if label == "" {
			label = p.Name
		}
		c.Chips = append(c.Chips, ui.PluginChip{ID: p.ID, Label: label})
	}
	return c
}

// sessionOpenChips is a mirrored window's Open control (pluginChips): its
// viewer plugins vp, its counts, and its session's pages.
func (s *Server) sessionOpenChips(ctx context.Context, vp []plugins.Plugin, h mirror.HostView, e mirror.SessionView) *ui.PluginChips {
	// A team host's window lists no pages (windowPagesRef): its control is
	// its team's previews alone, as before.
	on := s.pagesOn() && e.Owner == 0
	n := 0
	if on {
		n = len(s.sessionPages(ctx, windowPagesRef(h, e)))
	}
	// Nor asks (windowAsksQuery): a team window's asks are the team's.
	asksOn := s.asksOn() && e.Owner == 0
	open, all := s.sessionAskCounts(ctx, windowAsksQuery(h, e))
	return pluginChips(vp, e, on, n, asksOn, open, all, draftUser(ctx))
}

// windowPagesRef is what a mirrored window's pages are listed by: its host,
// the window itself (its tmux session and window id — the window a page was
// made in, pages.Creator.MirrorWindow: so a new Claude session in it keeps
// its earlier pages) and every MCP caller key it runs now
// (mirror.Window.MCPCallerKeys — pages.Creator.MirrorSession), read from h's
// report of the window.
//
// 🔴 NONE ON A TEAM HOST'S WINDOW (e.Owner != 0): a team window's pages are
// the TEAM's rows, which row-level security never shows a member (the pages
// store runs as the viewer, and no team door reads pages), and the viewer's
// OWN rows are keyed by host NAME — a page from the viewer's own host of the
// same name is not this window's. So nothing is read, and the tab says the
// session made none.
func windowPagesRef(h mirror.HostView, e mirror.SessionView) pages.SessionRef {
	if e.Owner != 0 || e.Window == "" {
		return pages.SessionRef{}
	}
	for _, w := range h.Windows {
		if w.ID == e.Window && w.Session == e.Session {
			return pages.SessionRef{MirrorHost: h.Host, MirrorTmuxSession: w.Session, MirrorWindow: w.ID, MirrorSessions: w.MCPCallerKeys()}
		}
	}
	return pages.SessionRef{}
}

// sessionPages is the pages session x made that ctx's principal may read
// (the store's row-level security: the viewer's own), newest first; nil with
// pages off, for an x that names no session, or on a store error (logged).
func (s *Server) sessionPages(ctx context.Context, x pages.SessionRef) []pages.Summary {
	if !s.pagesOn() || !x.Valid() {
		return nil
	}
	list, err := s.ext.Pages.SessionPages(ctx, x)
	if err != nil {
		s.logger.Printf("pages: a session's pages: %v", err)
		return nil
	}
	return list
}

// sessionPagesView is list as the Pages tab draws it: each page's current
// version, when it was made — its day in words (ui.PlainWhen, the Pages
// screens' rule: no timestamp in the top layer) — and whether it is shared,
// a live link of any kind (pages.Summary.LiveLinks), so someone without an
// account can open it.
func sessionPagesView(list []pages.Summary) []ui.SessionPage {
	now := time.Now()
	out := make([]ui.SessionPage, 0, len(list))
	for _, p := range list {
		out = append(out, ui.SessionPage{ID: p.ID, Title: p.Title, Version: p.CurrentVersion, Shared: p.LiveLinks > 0, Made: ui.PlainWhen(p.CreatedAt, now)})
	}
	return out
}

// handlePreviewButton serves GET ui.PreviewButtonPath: a mirrored window's
// Open button for its counts NOW (ui.OpenButton) — its previews while the
// owner has a live viewer plugin, plus its session's pages — nothing when it
// has nothing to open. The page's slot polls it.
//
// 🔴 THE SAME GATES AS THE PAGE: the previews count only with viewerPlugins
// (a token revoked since the page loaded takes them away at the next poll);
// the pages are the viewer's own rows (sessionPages, windowPagesRef — none on
// a team host's). An unknown window answers empty, never an error the slot
// would keep its old button through.
func (s *Server) handlePreviewButton(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	q := r.URL.Query()
	h, e, listed, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok || !listed || e.Window == "" {
		return
	}
	var o ui.OpenCounts
	if onTeamHost(r) {
		// A TEAM host's window (team sharing 2b.3b): the TEAM's plugins,
		// never the viewer's; with none, the sentence that says so.
		if e.Previews.IsZero() {
			return
		}
		ps, member := s.teamViewerPlugins(r.Context(), e.Owner, e.Window)
		if !member {
			return
		}
		if len(ps) == 0 {
			_, _ = w.Write([]byte(`<p class="text-sm text-fg2" data-team-no-viewer-plugin>` + html.EscapeString(TeamNoViewerPlugin) + `</p>`))
			return
		}
		o.Previews = e.Previews
	} else {
		if !e.Previews.IsZero() && len(s.viewerPlugins(r.Context(), e.Window)) > 0 {
			o.Previews = e.Previews
		}
		o.Pages = len(s.sessionPages(r.Context(), windowPagesRef(h, e)))
		o.Asks, o.AsksListed = s.sessionAskCounts(r.Context(), windowAsksQuery(h, e))
	}
	if err := ui.RenderOpenButton(w, o); err != nil {
		s.logger.Printf("open button: render: %v", err)
	}
}

// handleSessionPages serves GET ui.SessionPagesPath: a mirrored window's
// Pages tab (ui.SessionPagesList) — its session's pages, the viewer's own,
// newest first. An unknown window, a team host's, and pages off each answer
// the empty list.
func (s *Server) handleSessionPages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	q := r.URL.Query()
	var list []pages.Summary
	if h, e, listed, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window")); ok && listed {
		list = s.sessionPages(r.Context(), windowPagesRef(h, e))
	}
	if err := ui.SessionPagesList(sessionPagesView(list)).Render(w); err != nil {
		s.logger.Printf("session pages: render: %v", err)
	}
}

// agentOfPath is the id of the agent named by r's {name} — the viewer's own
// (Agents.GetByName runs under their principal); ok false for none.
func (s *Server) agentOfPath(r *http.Request) (int64, bool) {
	if s.ext.Agents == nil {
		return 0, false
	}
	a, err := s.ext.Agents.GetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		return 0, false
	}
	return a.ID, true
}

// agentPagesOf is the agent named by r's {name} (agentOfPath) and its pages;
// ok false for none.
func (s *Server) agentPagesOf(r *http.Request) ([]pages.Summary, bool) {
	id, ok := s.agentOfPath(r)
	if !ok {
		return nil, false
	}
	return s.sessionPages(r.Context(), pages.SessionRef{AgentID: id}), true
}

// handleAgentOpenButton serves GET ui.AgentOpenButtonPath: an agent page's
// Open button — the agent's pages and asks (an agent has no previews) —
// nothing when it has neither; 404 for an agent the viewer does not see
// (another owner's reads as a missing one).
func (s *Server) handleAgentOpenButton(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	id, ok := s.agentOfPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	list := s.sessionPages(r.Context(), pages.SessionRef{AgentID: id})
	open, all := s.sessionAskCounts(r.Context(), attention.SessionAsksQuery{AgentID: id})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderOpenButton(w, ui.OpenCounts{Pages: len(list), Asks: open, AsksListed: all}); err != nil {
		s.logger.Printf("open button: render: %v", err)
	}
}

// handleAgentSessionPages serves GET ui.AgentSessionPagesPath: an agent
// page's Pages tab, the agent's pages newest first; 404 for an agent the
// viewer does not see.
func (s *Server) handleAgentSessionPages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	list, ok := s.agentPagesOf(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.SessionPagesList(sessionPagesView(list)).Render(w); err != nil {
		s.logger.Printf("session pages: render: %v", err)
	}
}

// agentOpenChips is an agent page's Open control: the agent's pages, while
// the pages feature is on, and its asks, while the queue is served; nil with
// neither.
func (s *Server) agentOpenChips(ctx context.Context, a agents.Agent) *ui.PluginChips {
	if !s.pagesOn() && !s.asksOn() {
		return nil
	}
	n := len(s.sessionPages(ctx, pages.SessionRef{AgentID: a.ID}))
	open, all := s.sessionAskCounts(ctx, attention.SessionAsksQuery{AgentID: a.ID})
	// AsksCoverBadge: the button's asks (the viewer's OWN, attention.
	// SessionAsks) are every ask the header's badge counts only on a
	// personal agent of the viewer's. A team agent's badge counts the team's
	// asks waiting on the viewer (askerAsksBadge, waitingOnReader), which the
	// button does not, so there the badge stays at every width.
	cover := a.OwnerID != 0 && a.OwnerID == requestOwner(ctx)
	return &ui.PluginChips{PagesOn: s.pagesOn(), Pages: n, AsksOn: s.asksOn(), Asks: open, AsksListed: all, AsksCoverBadge: cover,
		Agent: a.Name, Viewer: draftUser(ctx)}
}

// TeamNoViewerPlugin is what a team session's Preview says when the team has
// no live viewer plugin (team sharing 2b.3b): it never falls back to the
// viewer's own.
const TeamNoViewerPlugin = "This team has no viewer plugin set up, so its previews do not open here. A manager of the team adds one on the team's plugins page."

// teamViewerPlugins is team's live viewer plugins for window, read AS THE
// TEAM (teamPreviewPrincipal) after a FRESH read that ctx's person is still
// a viewer of it (teamMemberNow) — member false, and nothing, otherwise.
func (s *Server) teamViewerPlugins(ctx context.Context, team int64, window string) ([]plugins.Plugin, bool) {
	if mirror.ValidWindowID(window) != nil || !s.teamMemberNow(ctx, requestOwner(ctx), team, authz.TeamViewer) {
		return nil, false
	}
	return s.liveViewerPlugins(s.teamPreviewPrincipal(ctx, team)), true
}

// teamPreviewPrincipal is ctx acting as team through authz.ViaPlugin, the
// team plugin door: how a member's preview reads the team's viewer plugins
// and writes its hand-off (plugins is read at operator, plugin_handoffs
// written at manager — the orchestrator's sign-off on the operator's ruling
// that team previews run on the team's plugins: a viewer opening a preview
// writes it as the team). Called only after teamMemberNow. internal/authz's
// withPrincipalLedger lists it.
func (s *Server) teamPreviewPrincipal(ctx context.Context, team int64) context.Context {
	return authz.WithPrincipal(ctx, teamActingPrincipal(team, authz.ViaPlugin))
}
