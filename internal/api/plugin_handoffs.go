package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
)

// VIEWER HAND-OFF (docs/plugins.md#viewer-hand-off; docs/design/previews.md
// seam (a)). Two doors, one for each side:
//
//   - POST /plugin-handoffs: the owner's BROWSER, signed in on muster's own
//     origin (requireOperatorWrite: a session, same-origin), mints a code for
//     one of its own live viewer plugins and a mirrored window it is viewing.
//     The window is checked against the owner's own hosts as the hub lists
//     them; whether the sign-in's step-up is fresh is read from the session,
//     never from the request. Answers the plugin's entry URL with the code.
//   - POST /plugin/v1/handoffs/redeem: the PLUGIN's own token (capability
//     viewer) spends the code once and learns who and what it was for.
//
// Both answer the typed feature_disabled 404 while MUSTER_PLUGIN_ORIGINS is
// unset (the plugin-viewers feature) or plugins are off.

const (
	pluginHandoffsPath      = "/plugin-handoffs"
	pluginHandoffRedeemPath = pluginAPIPrefix + "/handoffs/redeem"

	// PluginHandoffNotFound is redeem's one refusal: unknown, used, expired or
	// another plugin's code.
	PluginHandoffNotFound = "no such hand-off code"
	// PluginHandoffNoPlugin and PluginHandoffNoWindow are the mint's 404s.
	PluginHandoffNoPlugin = "no such viewer plugin"
	PluginHandoffNoWindow = "no such window on your hosts"
	// PluginHandoffOriginRetired is the mint's 403 for a token whose origin the
	// admin has since removed from MUSTER_PLUGIN_ORIGINS.
	PluginHandoffOriginRetired = "this plugin's origin is no longer in this server's MUSTER_PLUGIN_ORIGINS; revoke it and mint another"
)

func (s *Server) registerPluginHandoffRoutes(mux Mux) {
	mux.HandleFunc("POST "+pluginHandoffsPath, s.requirePluginViewersOn(s.requireOperatorWrite(s.handlePluginHandoffMint)))
	mux.HandleFunc("POST "+pluginHandoffRedeemPath, s.requirePluginViewersOn(s.requirePluginToken(s.handlePluginHandoffRedeem, plugins.CapViewer)))
}

// requirePluginViewersOn answers the typed feature_disabled 404 unless plugins
// are on AND MUSTER_PLUGIN_ORIGINS is set, before any credential is examined.
func (s *Server) requirePluginViewersOn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case s.ext.Plugins == nil:
			s.writeFeatureDisabled(w, r, features.Plugins)
		case len(s.ext.PluginOrigins) == 0:
			s.writeFeatureDisabled(w, r, features.PluginViewers)
		default:
			next(w, r)
		}
	}
}

// pluginHandoffMintIn is POST /plugin-handoffs' body. Only the host and the
// window are read from the target: the session is the hub's.
type pluginHandoffMintIn struct {
	Plugin int64 `json:"plugin"`
	Target struct {
		Mirror *struct {
			Host   string `json:"host"`
			Window string `json:"window"`
		} `json:"mirror"`
	} `json:"target"`
}

// handlePluginHandoffMint is POST /plugin-handoffs {plugin, target:{mirror:
// {host, window}}} → 200 {url}: the plugin's entry URL with
// muster_code and muster_plugin.
func (s *Server) handlePluginHandoffMint(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in pluginHandoffMintIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Plugin <= 0 || in.Target.Mirror == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"plugin": <id>, "target": {"mirror": {"host", "window"}}}`})
		return
	}
	refused := func(code int, msg string) {
		metrics.PluginHandoffs.WithLabelValues(plugins.PurposeView, "refused").Inc()
		s.writeJSON(w, code, map[string]any{"error": msg})
	}
	target, ok := s.handoffMirrorTarget(r, in.Target.Mirror.Host, in.Target.Mirror.Window)
	if !ok {
		refused(http.StatusNotFound, PluginHandoffNoWindow)
		return
	}
	code, p, err := s.ext.Plugins.MintHandoff(r.Context(), in.Plugin, plugins.HandoffIn{
		Target:  plugins.HandoffTarget{Mirror: &target},
		Purpose: plugins.PurposeView,
		// 🔴 SERVER-DERIVED: the session's own sign-in time, never a field.
		SteppedUp: stepup.Fresh(r.Context(), s.stepUpNow()),
		// The plugin's link is re-checked against the CURRENT allowlist.
		Origins: s.ext.PluginOrigins,
	}, s.now())
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		refused(http.StatusNotFound, PluginHandoffNoPlugin)
		return
	case errors.Is(err, plugins.ErrViewerOriginRetired):
		refused(http.StatusForbidden, PluginHandoffOriginRetired)
		return
	case errors.Is(err, plugins.ErrHandoffRateLimited):
		metrics.PluginHandoffs.WithLabelValues(plugins.PurposeView, "rate_limited").Inc()
		w.Header().Set("Retry-After", "60")
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many hand-offs this minute"})
		return
	default:
		s.logger.Printf("plugins: hand-off mint for plugin %d: %v", in.Plugin, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the hand-off could not be made"})
		return
	}
	metrics.PluginHandoffs.WithLabelValues(plugins.PurposeView, "minted").Inc()
	// The entry URL was validated at mint to carry no query; build it anyway.
	u, err := url.Parse(*p.EntryURL)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the plugin's entry URL does not parse"})
		return
	}
	u.RawQuery = url.Values{"muster_code": {code}, "muster_plugin": {strconv.FormatInt(p.ID, 10)}}.Encode()
	s.writeJSON(w, http.StatusOK, map[string]any{"url": u.String()})
}

// handoffMirrorTarget resolves (host, window) against the principal's own
// hosts as the hub lists them now: the host must be the principal's and list a
// window with exactly that tmux id. The session is the listing's.
func (s *Server) handoffMirrorTarget(r *http.Request, host, window string) (plugins.MirrorTarget, bool) {
	if s.ext.Mirror == nil || mirror.ValidHost(host) != nil || mirror.ValidWindowID(window) != nil {
		return plugins.MirrorTarget{}, false
	}
	for _, h := range s.ext.Mirror.Hosts(r.Context()) {
		if h.Host != host {
			continue
		}
		for _, e := range h.Entries() {
			if e.Window == window {
				return plugins.MirrorTarget{Host: h.Host, Session: e.Session, Window: e.Window}, true
			}
		}
	}
	return plugins.MirrorTarget{}, false
}

// handlePluginHandoffRedeem is POST /plugin/v1/handoffs/redeem {code} → 200
// {owner_id, target, purpose, stepped_up, minted_at}, or one 404 for every
// code it cannot spend.
func (s *Server) handlePluginHandoffRedeem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"code": "<muster_code>"}`})
		return
	}
	p := pluginFromContext(r.Context())
	h, err := s.ext.Plugins.RedeemHandoff(r.Context(), p.ID, strings.TrimSpace(in.Code), s.now())
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		metrics.PluginHandoffs.WithLabelValues("unknown", "not_found").Inc()
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": PluginHandoffNotFound})
		return
	default:
		s.logger.Printf("plugins: hand-off redeem for plugin %d: %v", p.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the hand-off could not be redeemed"})
		return
	}
	metrics.PluginHandoffs.WithLabelValues(h.Purpose, "redeemed").Inc()
	s.writeJSON(w, http.StatusOK, h)
}
