package api

import (
	"context"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SESSION-VIEW CHIP (docs/plugins.md#the-session-view-chip;
// docs/design/previews.md seam (b)): one static chip per live viewer plugin of
// the owner, on a mirrored window's two session views (/mirror/view and
// /mirror/terminal). Its Open and Show here mint a hand-off (POST
// /plugin-handoffs) at click time; nothing is minted to draw it.
//
// 🔴 DRAWN FROM THE TOKEN ROWS ALONE, and only while they would mint: a
// revoked token, one without viewer, and one whose origin is no longer in
// MUSTER_PLUGIN_ORIGINS (the mint's own re-check, ValidViewerLink) draw no
// chip. With MUSTER_PLUGIN_ORIGINS unset, or plugins off, nothing is read at
// all: no chip and no frame-src.

// viewerPlugins is the principal's live viewer plugins whose link the CURRENT
// MUSTER_PLUGIN_ORIGINS still allows, newest first (the store's List order);
// nil when the seam is off or the window has no tmux id a hand-off could name.
func (s *Server) viewerPlugins(ctx context.Context, window string) []plugins.Plugin {
	if s.ext.Plugins == nil || len(s.ext.PluginOrigins) == 0 || mirror.ValidWindowID(window) != nil {
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

// pluginChips is what the page draws for ps, aimed at (host, window).
func pluginChips(ps []plugins.Plugin, host, window string) *ui.PluginChips {
	if len(ps) == 0 {
		return nil
	}
	c := &ui.PluginChips{MintPath: pluginHandoffsPath, Host: host, Window: window}
	for _, p := range ps {
		label := p.Label
		if label == "" {
			label = p.Name
		}
		c.Chips = append(c.Chips, ui.PluginChip{ID: p.ID, Label: label})
	}
	return c
}
