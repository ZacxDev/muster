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

// THE HAND-OFF (docs/plugins.md#viewer-hand-off; docs/design/previews.md
// seam (a); docs/design/meetings.md "The hand-off, extended"). Two doors, one
// for each side:
//
//   - POST /plugin-handoffs: the owner's BROWSER, signed in on muster's own
//     origin (requireOperatorWrite: a session, same-origin), mints a code for
//     EXACTLY ONE target: a mirrored window it is viewing, for one of its own
//     live viewer plugins, or an app tab's page (`{"app": {}}`, with an
//     optional `ref`), for one of its own live app plugins. The window is
//     checked against the owner's own hosts as the hub lists them; whether
//     the sign-in's step-up is fresh is read from the session, never from the
//     request. Answers the plugin's entry URL with the code.
//   - POST /plugin/v1/handoffs/redeem: the PLUGIN's own token (capability
//     viewer OR app) spends the code once and learns who and what it was for.
//
// 🔴 THE PURPOSE IS THE TARGET'S, NEVER THE REQUEST'S: a mirror target mints
// `view`, an app target `app` (plugins.HandoffTarget.purpose). A body naming a
// purpose is not read. The capability is the target's too: the store mints an
// app code only for a plugin holding `app`, a mirror code only for one holding
// `viewer`.
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
	mux.HandleFunc("POST "+pluginHandoffsPath, s.requirePluginOriginsOn(s.requireOperatorWrite(s.handlePluginHandoffMint)))
	mux.HandleFunc("POST "+pluginHandoffRedeemPath, s.requirePluginOriginsOn(s.requirePluginToken(s.handlePluginHandoffRedeem, pluginCapHandoff)))
}

// requirePluginOriginsOn answers the typed feature_disabled 404 unless plugins
// are on AND MUSTER_PLUGIN_ORIGINS is set, before any credential is examined.
// (It was requirePluginViewersOn: the same check, which now gates the app
// tab's hand-offs as well as the viewers'.)
func (s *Server) requirePluginOriginsOn(next http.HandlerFunc) http.HandlerFunc {
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
// window are read from a mirror target (the session is the hub's), and only
// the ref from an app target. No purpose is read: it is the target's.
type pluginHandoffMintIn struct {
	Plugin int64 `json:"plugin"`
	// Owner, when set and not the principal's own, is the TEAM whose host the
	// window is on (team sharing 2b.3b): the plugin must be that team's.
	Owner  int64 `json:"owner"`
	Target struct {
		Mirror *struct {
			Host   string `json:"host"`
			Window string `json:"window"`
		} `json:"mirror"`
		App *struct {
			Ref string `json:"ref"`
		} `json:"app"`
	} `json:"target"`
}

// PluginHandoffMintBody is the mint's 400: the two shapes it takes.
const PluginHandoffMintBody = `the body is {"plugin": <id>, "target": {"mirror": {"host", "window"}}} or {"plugin": <id>, "target": {"app": {}}}`

// handlePluginHandoffMint is POST /plugin-handoffs {plugin, target:{mirror:
// {host, window}}} or {plugin, target:{app:{ref?}}} → 200 {url}: the plugin's
// entry URL with muster_code and muster_plugin.
func (s *Server) handlePluginHandoffMint(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in pluginHandoffMintIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Plugin <= 0 ||
		(in.Target.Mirror == nil) == (in.Target.App == nil) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": PluginHandoffMintBody})
		return
	}
	purpose := plugins.PurposeView
	if in.Target.App != nil {
		purpose = plugins.PurposeApp
	}
	refused := func(code int, msg string) {
		metrics.PluginHandoffs.WithLabelValues(purpose, "refused").Inc()
		s.writeJSON(w, code, map[string]any{"error": msg})
	}
	// 🔴 WHOSE PLUGIN, ON WHOSE WINDOW (team sharing 2b.3b). The principal's
	// own window takes the principal's own plugin (the store's one owner
	// filter: never a team's, which a member's session can also read). A TEAM
	// host's window takes the
	// TEAM's plugin, never the viewer's: after a fresh viewer read
	// (teamViewerPlugins), the window is resolved over the team's hosts and
	// the hand-off minted AS THE TEAM (teamPreviewPrincipal; the orchestrator's
	// sign-off on the operator's ruling), and the member who opened it is
	// recorded in the team's audit (team.plugin.handoff). A team with no
	// viewer plugin is refused with TeamNoViewerPlugin.
	mctx := r.Context()
	var target plugins.MirrorTarget
	var handoff plugins.HandoffTarget
	ok := false
	switch {
	case in.Target.App != nil:
		// An app tab is its PERSON's (person-owned only, decision 14): never
		// a team's, so no owner other than the principal's own is taken.
		if in.Owner > 0 && in.Owner != requestOwner(r.Context()) {
			refused(http.StatusNotFound, PluginHandoffNoPlugin)
			return
		}
		handoff, ok = plugins.HandoffTarget{App: &plugins.AppTarget{Ref: appRef(in.Target.App.Ref)}}, true
	case in.Owner > 0 && in.Owner != requestOwner(r.Context()):
		ps, member := s.teamViewerPlugins(r.Context(), in.Owner, in.Target.Mirror.Window)
		if !member {
			refused(http.StatusNotFound, PluginHandoffNoWindow)
			return
		}
		if len(ps) == 0 {
			refused(http.StatusNotFound, TeamNoViewerPlugin)
			return
		}
		target, ok = handoffTargetIn(s.teamHostsOf(r.Context(), in.Owner), in.Target.Mirror.Host, in.Target.Mirror.Window)
		mctx = s.teamPreviewPrincipal(r.Context(), in.Owner)
		handoff = plugins.HandoffTarget{Mirror: &target}
	default:
		target, ok = s.handoffMirrorTarget(r, in.Target.Mirror.Host, in.Target.Mirror.Window)
		handoff = plugins.HandoffTarget{Mirror: &target}
	}
	if !ok {
		refused(http.StatusNotFound, PluginHandoffNoWindow)
		return
	}
	code, p, err := s.ext.Plugins.MintHandoff(mctx, in.Plugin, plugins.HandoffIn{
		Target: handoff,
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
		metrics.PluginHandoffs.WithLabelValues(purpose, "rate_limited").Inc()
		w.Header().Set("Retry-After", "60")
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many hand-offs this minute"})
		return
	default:
		s.logger.Printf("plugins: hand-off mint for plugin %d: %v", in.Plugin, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the hand-off could not be made"})
		return
	}
	metrics.PluginHandoffs.WithLabelValues(purpose, "minted").Inc()
	if in.Owner > 0 && in.Owner != requestOwner(r.Context()) {
		// The member who opened the TEAM's hand-off (#696 round 0, (c)): the
		// row is the team's, so the team's audit names the person.
		s.auditTeam(r.Context(), actorOf(r), in.Owner, "team.plugin.handoff", "plugin:"+strconv.FormatInt(p.ID, 10),
			map[string]any{"host": target.Host, "window": target.Window})
	}
	// The entry URL was validated at mint to carry no query; build it anyway.
	u, err := url.Parse(*p.EntryURL)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the plugin's entry URL does not parse"})
		return
	}
	u.RawQuery = url.Values{"muster_code": {code}, "muster_plugin": {strconv.FormatInt(p.ID, 10)}}.Encode()
	s.writeJSON(w, http.StatusOK, map[string]any{"url": u.String()})
}

// appRef is an app target's ref as the mint stores it: ref when
// plugins.ValidAppRef accepts it, "" (dropped, not refused) otherwise. muster
// passes it through and never interprets it.
func appRef(ref string) string {
	if plugins.ValidAppRef(ref) {
		return ref
	}
	return ""
}

// handoffMirrorTarget resolves (host, window) against the principal's own
// hosts as the hub lists them now: the host must be the principal's and list a
// window with exactly that tmux id. The session is the listing's.
func (s *Server) handoffMirrorTarget(r *http.Request, host, window string) (plugins.MirrorTarget, bool) {
	if s.ext.Mirror == nil {
		return plugins.MirrorTarget{}, false
	}
	return handoffTargetIn(s.ext.Mirror.Hosts(r.Context()), host, window)
}

// handoffTargetIn resolves (host, window) against hosts: the host must list a
// window with exactly that tmux id. The session is the listing's.
func handoffTargetIn(hosts []mirror.HostView, host, window string) (plugins.MirrorTarget, bool) {
	if mirror.ValidHost(host) != nil || mirror.ValidWindowID(window) != nil {
		return plugins.MirrorTarget{}, false
	}
	for _, h := range hosts {
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
