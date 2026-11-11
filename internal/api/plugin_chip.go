package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
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
// counts now.
func pluginChips(ps []plugins.Plugin, e mirror.SessionView) *ui.PluginChips {
	if len(ps) == 0 {
		return nil
	}
	c := &ui.PluginChips{MintPath: pluginHandoffsPath, Host: e.Host, Session: e.Session, Window: e.Window, Previews: e.Previews}
	for _, p := range ps {
		label := p.Label
		if label == "" {
			label = p.Name
		}
		c.Chips = append(c.Chips, ui.PluginChip{ID: p.ID, Label: label})
	}
	return c
}

// handlePreviewButton serves GET ui.PreviewButtonPath: the session view's
// Preview button for the window's counts NOW (ui.PreviewButton), drawn for
// the slot's `place` (the header row, or the second row below `sm`), nothing
// when it has nothing to preview. The page's slots poll it.
//
// 🔴 THE SAME TWO GATES AS THE PAGE: the window's counts first (in memory —
// a window with nothing answers empty without reading the plugin store), then
// viewerPlugins, so a token revoked since the page loaded takes the button
// away at the next poll. An unknown window answers empty, never an error the
// slot would keep its old button through.
func (s *Server) handlePreviewButton(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	q := r.URL.Query()
	_, e, listed, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok || !listed || e.Window == "" || e.Previews.IsZero() || len(s.viewerPlugins(r.Context(), e.Window)) == 0 {
		return
	}
	if err := ui.RenderPreviewButton(w, e.Previews, ui.PreviewPlaceOf(q.Get("place"))); err != nil {
		s.logger.Printf("preview button: render: %v", err)
	}
}
