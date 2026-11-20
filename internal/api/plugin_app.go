package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE APP TAB (docs/design/meetings.md, "The tab", "The page", "The frame,
// exactly", "When the token is revoked"; core PR 1b). An `app` plugin (1a)
// gets an entry in its owner's sidebar, a page at /apps/{id} that frames the
// plugin's entry URL, and a poll that takes the frame away once the token is
// no longer live.
//
// 🔴 ONE PREDICATE, THREE CONSUMERS. The sidebar's entries (shellFeatures →
// appTabs), the page and the alive poll all ask liveAppPlugins, so a plugin
// that is not drawn as a tab is also a 404 page and a 404 poll: revoked, not
// holding `app`, another owner's, a team's, or an origin and entry URL the
// CURRENT MUSTER_PLUGIN_ORIGINS no longer allows (the mint's own re-check,
// ValidViewerLink). With plugins off or MUSTER_PLUGIN_ORIGINS unset nothing is
// read at all. A disabled (or signed-out) owner is refused by the session
// door: no page or tab is drawn, and an /apps/{id} page already OPEN gets the
// alive poll's 401, on which its script removes the frame as on a 404
// (TestADisabledOwnersAppPageAndPollAreRefused).
//
// 🔴 THE PAGE'S HEADERS WIDEN ONLY HERE, AND ONLY TO THE ENTRY URL'S EXACT
// ORIGIN (appOrigin), never the plugin's origin PATTERN: frame-src <O> (no
// 'self'), and a Permissions-Policy delegating camera, microphone,
// display-capture, fullscreen, autoplay and clipboard-write to `self` and <O>.
// 🔴 `self` IS KEPT ON EVERY FEATURE (spike S5): Chromium delegates only a
// feature the parent holds, and under camera=("<O>") the frame's getUserMedia
// returned audio with no video track and no error. frame-ancestors 'none' and
// X-Frame-Options DENY stay. Every other response keeps securityHeaders'
// defaults; TestOnlyTheAppPageWidensItsHeaders walks every GET route.

const (
	appPagePrefix  = "/apps/"
	appAlivePrefix = "/ui/apps/"
	appAliveSuffix = "/alive"
	// AppPageNotFound is /apps/{id}'s one 404, whatever the reason.
	AppPageNotFound = "There is no such app."
)

func (s *Server) registerPluginAppRoutes(mux Mux) {
	mux.HandleFunc("GET "+appPagePrefix+"{id}", s.requireSession(s.handleAppPage))
	mux.HandleFunc("GET "+appAlivePrefix+"{id}"+appAliveSuffix, s.requireSession(s.handleAppAlive))
}

// liveAppPlugins is ctx's principal's live app plugins, oldest first: not
// revoked, holding `app`, PERSON-owned by the principal itself, with a label
// and icon, and an origin and entry URL the current MUSTER_PLUGIN_ORIGINS
// still allows. nil with plugins off or the variable unset, before any read.
func (s *Server) liveAppPlugins(ctx context.Context) []plugins.Plugin {
	if s.ext.Plugins == nil || len(s.ext.PluginOrigins) == 0 {
		return nil
	}
	// The page already read it for this request (handleAppPage): the sidebar
	// reuses that one list rather than reading the store again.
	if ps, ok := ctx.Value(liveAppsKey{}).([]plugins.Plugin); ok {
		return ps
	}
	owner := requestOwner(ctx)
	if owner <= 0 {
		return nil
	}
	all, err := s.ext.Plugins.List(ctx)
	if err != nil {
		s.logger.Printf("plugins: list for the app tabs: %v", err)
		return nil
	}
	var out []plugins.Plugin
	for _, p := range all {
		if p.RevokedAt != nil || !p.Has(plugins.CapApp) || p.OwnerTeam || p.OwnerID != owner ||
			p.AppLabel == nil || p.AppIcon == nil || p.Origin == nil || p.EntryURL == nil {
			continue
		}
		if _, err := plugins.ValidViewerLink(s.ext.PluginOrigins, *p.Origin, *p.EntryURL); err != nil {
			continue
		}
		if appOrigin(*p.EntryURL) == "" {
			continue
		}
		out = append(out, p)
	}
	// The store lists newest first; the sidebar is oldest first.
	slices.Reverse(out)
	return out
}

// liveAppsKey carries one request's liveAppPlugins (withLiveApps).
type liveAppsKey struct{}

// withLiveApps is ctx carrying its liveAppPlugins, read once, for a handler
// that needs them twice (the page and its sidebar).
func (s *Server) withLiveApps(ctx context.Context) context.Context {
	return context.WithValue(ctx, liveAppsKey{}, s.liveAppPlugins(ctx))
}

// liveAppPlugin is the one of liveAppPlugins with id raw, or false.
func (s *Server) liveAppPlugin(ctx context.Context, raw string) (plugins.Plugin, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		return plugins.Plugin{}, false
	}
	for _, p := range s.liveAppPlugins(ctx) {
		if p.ID == id {
			return p, true
		}
	}
	return plugins.Plugin{}, false
}

// appTabs is liveAppPlugins as the sidebar draws them.
func (s *Server) appTabs(ctx context.Context) []ui.AppTab {
	var out []ui.AppTab
	for _, p := range s.liveAppPlugins(ctx) {
		icon, _ := plugins.AppIconEmoji(*p.AppIcon)
		out = append(out, ui.AppTab{ID: p.ID, Label: *p.AppLabel, Icon: icon})
	}
	return out
}

// appOrigin is an entry URL's exact origin (https://host, as ValidViewerLink
// allows it: no port, no user), "" for one that does not parse as that.
func appOrigin(entry string) string {
	u, err := url.Parse(entry)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(u.Host, ` "'`) {
		return ""
	}
	return "https://" + u.Host
}

// appFrameFeatures are the features the frame is delegated, in the order the
// page's Permissions-Policy and the frame's allow attribute name them.
var appFrameFeatures = []string{"microphone", "camera", "display-capture", "fullscreen", "autoplay", "clipboard-write"}

// appPermissionsPolicy is /apps/{id}'s Permissions-Policy for origin:
// each of appFrameFeatures for `self` and origin, nothing else.
func appPermissionsPolicy(origin string) string {
	parts := make([]string, 0, len(appFrameFeatures))
	for _, f := range appFrameFeatures {
		parts = append(parts, f+`=(self "`+origin+`")`)
	}
	return strings.Join(parts, ", ")
}

// handleAppPage is GET /apps/{id}: the owner's own live app plugin's page, or
// the one 404 (AppPageNotFound) for anything else.
func (s *Server) handleAppPage(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(s.withLiveApps(r.Context()))
	p, ok := s.liveAppPlugin(r.Context(), r.PathValue("id"))
	if !ok {
		s.writeNavDocument(w, r, http.StatusNotFound, "Not found", "", false, `<p class="text-sm text-fg2">`+AppPageNotFound+`</p>`)
		return
	}
	origin := appOrigin(*p.EntryURL)
	ref := r.URL.Query().Get("ref")
	if !plugins.ValidAppRef(ref) {
		ref = ""
	}
	v := ui.AppPageView{
		Nav:      ui.NavChrome{Feat: s.shellFeatures(r.Context())},
		ID:       p.ID,
		Label:    *p.AppLabel,
		Origin:   origin,
		Ref:      ref,
		MintPath: pluginHandoffsPath,
	}
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy("'none'", origin))
	h.Set("Permissions-Policy", appPermissionsPolicy(origin))
	writeDocumentHeaders(w, http.StatusOK)
	if err := ui.AppPage(v).Render(w); err != nil {
		s.logger.Printf("render the app page %d: %v", p.ID, err)
	}
}

// handleAppAlive is GET /ui/apps/{id}/alive: 204 while the plugin would still
// be drawn as a tab, 404 once it would not. The open page polls it and
// removes its frame on a 404 (ui.AppPage).
func (s *Server) handleAppAlive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.liveAppPlugin(r.Context(), r.PathValue("id")); !ok {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
