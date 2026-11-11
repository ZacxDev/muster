package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE PLUGIN INTERFACE (internal/plugins): out-of-process connectors that bring
// inbound messages to their owner's chief agent. OFF unless MUSTER_PLUGINS=1
// (Extensions.Plugins nil): then every route below answers 404 — before any
// credential is looked at — and /plugins is not drawn.
//
// TWO DOORS, AND WHICH CREDENTIAL OPENS WHICH IS THE DESIGN:
//
//   - /plugins (the page and its forms): a browser SESSION, and the forms
//     requireOperatorWrite. Minting a plugin token is a person's act; the token
//     is shown once and only its sha256 is stored.
//   - /plugin/v1/*: a PLUGIN token (mpl_…), through requirePluginToken, and
//     nothing else. It acts as its owner (authz.ViaPlugin), so row-level security
//     confines it to that owner's plugin rows. 🔴 THE SHARED HOOK AND SERVICE
//     TOKENS, A PERSONAL TOKEN, A RUNNER TOKEN AND AN AGENT'S TOKEN ARE ALL
//     REFUSED BY SHAPE before any lookup: a plugin is a process on someone
//     else's terms, and nothing but its own narrow credential may speak for it.
//
// 🔴 AND THE CONVERSE: AN mpl_ TOKEN IS REFUSED ON EVERY OTHER ROUTE. admit — the
// one function every authentication wrapper hands a request on through —
// refuses a request carrying a plugin-shaped credential unless the wrapper that
// admitted it is the plugin door (TestAPluginTokenIsRefusedOnEveryOtherRoute).

const (
	pluginsPath      = "/plugins"
	pluginAPIPrefix  = "/plugin/v1"
	pluginSelfPath   = pluginAPIPrefix + "/self"
	pluginsNotServed = "this server does not serve plugins (MUSTER_PLUGINS is off)"
)

// pluginCapAny is the capability of a route every live plugin token opens,
// whatever it carries: GET /plugin/v1/self only. Deliberately not a value
// plugins.capabilities admits (0102's CHECK), so no row can hold it.
const pluginCapAny plugins.Capability = "(any live token)"

// PluginCapabilityRefusal is the 403 body when a live plugin token lacks the
// capability its route needs. It names the capability, not the token's own set.
func PluginCapabilityRefusal(c plugins.Capability) string {
	return "this plugin token does not have the \"" + string(c) + "\" capability this route needs"
}

// PluginTokenRefusal is the 401 body for every refused credential on the plugin
// door — another kind of token, a malformed one, an unknown or revoked one — so
// a refusal says nothing about which.
const PluginTokenRefusal = "this route takes a plugin token (" + plugins.TokenPrefix + "…), minted at " + pluginsPath

// PluginTokenElsewhere is the 401 body when a plugin token is presented on any
// route that is not the plugin door.
const PluginTokenElsewhere = "a plugin token (" + plugins.TokenPrefix + "…) opens " + pluginAPIPrefix + "/* only"

func (s *Server) registerPluginRoutes(mux Mux) {
	mux.HandleFunc("GET "+pluginsPath, s.requirePluginsOn(s.requireSession(s.handlePluginsPage)))
	mux.HandleFunc("POST "+pluginsPath, s.requirePluginsOn(s.requireOperatorWrite(s.handlePluginCreate)))
	mux.HandleFunc("POST "+pluginsPath+"/{id}/revoke", s.requirePluginsOn(s.requireOperatorWrite(s.handlePluginRevoke)))
	// Checks replies on or off for an existing plugin (draft_checks), with the
	// same token: revoke's gate — a session and a same-origin form, no step-up.
	mux.HandleFunc("POST "+pluginsPath+"/{id}/draft-checks", s.requirePluginsOn(s.requireOperatorWrite(s.handlePluginDraftChecksToggle)))

	// Reply drafts (plugin_drafts.go): the owner decides; Approve also needs a
	// fresh sign-in, checked in the handler. Reject (and withdrawing an approval,
	// which is a reject) does not.
	mux.HandleFunc("POST "+pluginsPath+"/drafts/{id}/approve", s.requirePluginsOn(s.requireOperatorWrite(s.handleDraftApprove)))
	mux.HandleFunc("POST "+pluginsPath+"/drafts/{id}/reject", s.requirePluginsOn(s.requireOperatorWrite(s.handleDraftReject)))

	// Retrying a message the chief never confirmed (plugin_retry.go).
	s.registerPluginRetryRoutes(mux)

	// Viewer hand-off codes (plugin_handoffs.go): the owner's browser mints,
	// the viewer plugin's token redeems. Off until MUSTER_PLUGIN_ORIGINS is set.
	s.registerPluginHandoffRoutes(mux)

	// 🔴 EVERY /plugin/v1/* ROUTE NAMES THE ONE CAPABILITY ITS TOKEN NEEDS
	// (plugins.capabilities, 0102), and TestEveryPluginRouteNamesOneCapability
	// is its ledger: a route added, dropped or moved to another capability turns
	// it red. CapMessages is every route a token opened before capabilities.
	mux.HandleFunc("GET "+pluginSelfPath, s.requirePluginToken(s.handlePluginSelf, pluginCapAny))
	mux.HandleFunc("POST "+pluginAPIPrefix+"/messages", s.requirePluginToken(s.handlePluginMessage, plugins.CapMessages))
	mux.HandleFunc("POST "+pluginAPIPrefix+"/drafts/claim", s.requirePluginToken(s.handlePluginDraftClaim, plugins.CapMessages))
	mux.HandleFunc("POST "+pluginAPIPrefix+"/drafts/{id}/result", s.requirePluginToken(s.handlePluginDraftResult, plugins.CapMessages))
	// The plugin's check of a pending draft before its owner may approve it
	// (plugin_drafts.go): only a plugin minted with draft_checks.
	mux.HandleFunc("GET "+pluginAPIPrefix+"/drafts/checks", s.requirePluginToken(s.handlePluginDraftChecks, plugins.CapDraftChecks))
	mux.HandleFunc("POST "+pluginAPIPrefix+"/drafts/{id}/check", s.requirePluginToken(s.handlePluginDraftCheck, plugins.CapDraftChecks))
	// Deterministic upstream notices → one task per external key (plugin_tasks.go).
	mux.HandleFunc("POST "+pluginTasksPath, s.requirePluginToken(s.handlePluginTask, plugins.CapMessages))
	// Plugin task REFERENCES (plugin_task_refs.go): the reconcile listing, and
	// the writes people and agents queued on this plugin's references.
	mux.HandleFunc("GET "+pluginTasksPath, s.requirePluginToken(s.handlePluginTaskList, plugins.CapMessages))
	mux.HandleFunc("POST "+pluginTaskWritesPath+"/claim", s.requirePluginToken(s.handlePluginTaskWritesClaim, plugins.CapMessages))
	mux.HandleFunc("POST "+pluginTaskWritesPath+"/{id}/result", s.requirePluginToken(s.handlePluginTaskWriteResult, plugins.CapMessages))
	// A reply sent directly, stamped with the message it answers
	// (owner_replies.go): its waiting route ends owner_answered. 404 with routing off.
	mux.HandleFunc("POST "+pluginOwnerAnsweredPath, s.requirePluginToken(s.handlePluginOwnerAnswered, plugins.CapMessages))
	// Session routing (session_routes.go): a MIRRORED session's reply_to_chief,
	// with its host's own muster credential acting as the owner (a personal
	// token, or the armed hook token). 404 with routing off. Fail-closed on an
	// unarmed hook tier: it steers what an outside party is sent.
	mux.HandleFunc("POST /api/routes/{id}/reply", s.requireArmedHookToken(s.handleRouteReply))
	// A thread a mirrored session started (thread_routes.go): its PostToolUse
	// hook, with route reply's credential. 404 with routing off.
	mux.HandleFunc("POST "+pluginThreadsPath, s.requireArmedHookToken(s.handlePluginThreadBind))
	// A pull request a session created or a branch it pushed (pr_routes.go): its
	// PostToolUse hook, with route reply's credential. 404 with PR routing off.
	mux.HandleFunc("POST "+prSessionsPath, s.requireArmedHookToken(s.handlePRSessionBind))
}

// requirePluginsOn answers 404 for a plugin route on a server with plugins off,
// before any credential is examined — the typed feature_disabled 404
// (writeFeatureDisabled), which names the feature and how to turn it on rather
// than reading as a route this server never had. Whether plugins are on is no
// secret: the Features page lists it.
func (s *Server) requirePluginsOn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Plugins == nil {
			s.writeFeatureDisabled(w, r, features.Plugins)
			return
		}
		next(w, r)
	}
}

// --- the plugin door --------------------------------------------------------

type pluginCtxKey struct{}

// requirePluginToken admits a PLUGIN token and nothing else, acting as its owner
// through authz.ViaPlugin, and only when the token carries capability need
// (pluginCapAny: any live token). See the file header.
//
// The handler is the FIRST argument, as every wrapper's is: the route walks
// (provisioner_seam_test.go's innermostRouteHandler) follow Args[0].
//
// The capability is checked AFTER the token is found and BEFORE the request is
// admitted, so a token without it reaches no handler and no store call but
// the lookup (TestAViewerOnlyTokenIsRefusedOnTheMessagesRoutes).
func (s *Server) requirePluginToken(next http.HandlerFunc, need plugins.Capability) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Plugins == nil {
			http.NotFound(w, r)
			return
		}
		tok := hookTokenFromRequest(r)
		if !plugins.TokenShapeOK(tok) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PluginTokenRefusal})
			return
		}
		p, err := s.pluginFor(r.Context(), tok)
		switch {
		case err == nil:
		case errors.Is(err, plugins.ErrBadToken):
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PluginTokenRefusal})
			return
		default:
			s.logger.Printf("plugins: token lookup failed: %v", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the plugin token"})
			return
		}
		if need != pluginCapAny && !p.Has(need) {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": PluginCapabilityRefusal(need)})
			return
		}
		ctx := context.WithValue(r.Context(), pluginCtxKey{}, p)
		s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), p.OwnerID, authz.ViaPlugin))
	}
}

// pluginFor resolves a plugin token to its record.
//
// 🔴 IT RUNS UNDER authz.System FOR THE REASON runnerTokenFor DOES: it is how the
// request's owner is LEARNED, and plugins is under FORCE row-level security. The
// System context is used for this one store call and never handed on; the
// handler gets the request's context with a principal built from the row's
// owner_id. System-ledger entry.
func (s *Server) pluginFor(ctx context.Context, tok string) (plugins.Plugin, error) {
	return s.ext.Plugins.Authenticate(authz.System(ctx), tok)
}

func pluginFromContext(ctx context.Context) plugins.Plugin {
	p, _ := ctx.Value(pluginCtxKey{}).(plugins.Plugin)
	return p
}

// refusePluginTokenElsewhere is admit's half of "mpl_ opens /plugin/v1/* only":
// true (and the request answered) when a request admitted by any door but the
// plugin door carries a plugin-shaped credential.
func (s *Server) refusePluginTokenElsewhere(w http.ResponseWriter, r *http.Request, via authz.Via) bool {
	if via == authz.ViaPlugin || !plugins.LooksLikeToken(hookTokenFromRequest(r)) {
		return false
	}
	s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PluginTokenElsewhere})
	return true
}

// pluginSelf is GET /plugin/v1/self's answer: who the presented token is.
type pluginSelf struct {
	PluginID     int64                `json:"plugin_id"`
	PluginName   string               `json:"plugin_name"`
	OwnerID      int64                `json:"owner_id"`
	OwnerLogin   string               `json:"owner_login"`
	Capabilities []plugins.Capability `json:"capabilities"`
}

// handlePluginSelf is GET /plugin/v1/self: the presented token's plugin id and
// name, its owner (id, and the login a person names them by) and its
// capabilities. Any live token may ask; a revoked token or a disabled owner is
// the door's 401, which is how a plugin learns either within one call.
func (s *Server) handlePluginSelf(w http.ResponseWriter, r *http.Request) {
	p := pluginFromContext(r.Context())
	s.writeJSON(w, http.StatusOK, pluginSelf{PluginID: p.ID, PluginName: p.Name, OwnerID: p.OwnerID,
		OwnerLogin: p.OwnerLogin, Capabilities: p.Capabilities})
}

// handlePluginMessage is POST /plugin/v1/messages: store one inbound message
// (202 {id, status:"queued"}), or answer a repeat of its external_id with the
// stored row (200 {id, status}). A NEW message is then routed to the session
// that started its thread (routeByThread) or, as any other, handed to the
// owner's chief in the background; the response does not wait for the turn.
func (s *Server) handlePluginMessage(w http.ResponseWriter, r *http.Request) {
	var in plugins.MessageIn
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, plugins.MaxRequestBytes))
	if err := dec.Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the body is {\"external_id\",\"sender\",\"subject\",\"body\",\"received_at\" (RFC 3339),\"no_reply\"}"})
		return
	}
	p := pluginFromContext(r.Context())
	m, created, err := s.ext.Plugins.Submit(r.Context(), p.ID, in)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
		return
	case errors.Is(err, plugins.ErrRateLimited), errors.Is(err, plugins.ErrQueueFull):
		w.Header().Set("Retry-After", "60")
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": strings.TrimPrefix(err.Error(), "plugins: ")})
		return
	case errors.Is(err, plugins.ErrNotFound):
		// Revoked between the token check and the insert.
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PluginTokenRefusal})
		return
	default:
		s.logger.Printf("plugins: plugin %d submit: %v", p.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the message could not be stored"})
		return
	}
	if !created {
		s.writeJSON(w, http.StatusOK, map[string]any{"id": m.ID, "status": string(m.Status)})
		return
	}
	s.dispatchPluginMessage(r, p, m)
	s.writeJSON(w, http.StatusAccepted, map[string]any{"id": m.ID, "status": string(m.Status)})
}

// dispatchPluginMessage hands a NEW message m on: answered by muster itself
// when it is a fleet-report request from a listed, verified sender
// (answerFleetReport), else to the session that started its thread
// (routeByThread), else — a review of a pull request already merged or
// closed — settled by muster (settleClosedPRReview), else to the session that
// authored the pull request it reviews (routeByPR), else — when that session's window is gone
// and the gate allows it — to that session RESUMED (startMailResume, which
// falls back to the chief itself), else by the first approved routing rule
// that matches it (applyRoutingRules: routing_rules.go says why it comes
// last), or else to the owner's chief. A route is
// made detached from the request, as the chief's delivery is: a client that
// hangs up once the route has taken the message must not cut off its
// injection or its record in the chief's conversation, or the message is the
// chief's while the chief never sees it.
//
// Who handled it is recorded for the usage page (chief_usage.go): the path's
// name, the rule, or the chief — with the rule that matched and could not act.
func (s *Server) dispatchPluginMessage(r *http.Request, p plugins.Plugin, m plugins.Message) {
	ctx := context.WithoutCancel(r.Context())
	for _, path := range []struct {
		handler string
		take    func(context.Context, plugins.Plugin, plugins.Message) bool
	}{
		{chiefusage.HandlerFleetReport, s.answerFleetReport},
		{chiefusage.HandlerThread, s.routeByThread},
		{chiefusage.HandlerClosedPR, s.settleClosedPRReview},
		{chiefusage.HandlerPRRoute, s.routeByPR},
		{chiefusage.HandlerMailResume, s.startMailResume},
	} {
		if path.take(ctx, p, m) {
			s.recordHandling(ctx, p, m, path.handler, 0, 0)
			return
		}
	}
	handled, rule, fallback := s.applyRoutingRules(ctx, p, m)
	if handled {
		s.recordHandling(ctx, p, m, chiefusage.HandlerRule, rule, 0)
		return
	}
	s.recordHandling(ctx, p, m, chiefusage.HandlerChief, 0, fallback)
	s.deliverPluginMessage(r, p, m)
}

// deliverPluginMessage starts the delivery of a just-stored message to the
// owner's chief, detached from the request (the turn can take minutes) but
// keeping its principal, and ended by EndStreams on shutdown so a cut-off turn
// is recorded (unconfirmed) rather than lost. WaitForPluginDeliveries waits for it.
//
// ⚠ NO DELIVERER (no agent gateway wired) LEAVES THE MESSAGE QUEUED. That is the
// honest state — nothing was handed to anyone — and the retry seam (a later
// caller passing a queued id to plugins.Deliverer.Deliver) picks it up once a
// gateway exists.
func (s *Server) deliverPluginMessage(r *http.Request, p plugins.Plugin, m plugins.Message) {
	s.deliverPluginMessageCtx(context.WithoutCancel(r.Context()), p, m)
}

// deliverPluginMessageCtx is deliverPluginMessage from a context that already
// carries the request's principal and no cancellation (a mail resume's
// fallback, which runs after the request has ended).
func (s *Server) deliverPluginMessageCtx(parent context.Context, p plugins.Plugin, m plugins.Message) {
	if s.pluginDeliverer == nil {
		return
	}
	ctx, cancel := s.streamContext(parent)
	s.pluginInFlight.Add(1)
	safeGo(s.logger, "plugin delivery", func() {
		defer s.pluginInFlight.Done()
		defer cancel()
		chief, err := s.chiefAgent(ctx)
		if err != nil && !errors.Is(err, ErrNoChief) {
			s.logger.Printf("plugins: plugin %d message %d: resolve the chief: %v", p.ID, m.ID, err)
		}
		s.pluginDeliverer.Deliver(ctx, p.ID, m.ID, chief)
	})
}

// WaitForPluginDeliveries blocks until every in-flight plugin delivery has
// recorded its outcome. Call during graceful shutdown, after EndStreams.
func (s *Server) WaitForPluginDeliveries() { s.pluginInFlight.Wait() }

// --- the page (session) -------------------------------------------------------

func (s *Server) handlePluginsPage(w http.ResponseWriter, r *http.Request) {
	s.renderPluginsPage(w, r, http.StatusOK, "", "")
}

func (s *Server) handlePluginCreate(w http.ResponseWriter, r *http.Request) {
	in, problem := s.pluginCreateInput(r)
	if problem != "" {
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", problem)
		return
	}
	tok, _, err := s.ext.Plugins.CreatePlugin(r.Context(), in)
	switch {
	case errors.Is(err, plugins.ErrInvalid):
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
	case err != nil:
		s.logger.Printf("plugins: create: %v", err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "The plugin could not be created.")
	default:
		// 200, NOT a redirect: the token is shown in THIS response only.
		s.renderPluginsPage(w, r, http.StatusOK, tok, "")
	}
}

func (s *Server) handlePluginRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	found := false
	if err == nil && id > 0 {
		found, err = s.ext.Plugins.Revoke(r.Context(), id)
		if err != nil {
			s.logger.Printf("plugins: revoke %d: %v", id, err)
			s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "The plugin could not be revoked.")
			return
		}
	}
	if !found {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	}
	http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
}

// handlePluginDraftChecksToggle is POST /plugins/{id}/draft-checks {on: "1"
// turns it on, anything else off}: the owner turns Checks replies (plugins.CapDraftChecks) on or off
// for one of their LIVE plugins, keeping its token and its row
// (plugins.PGStore.SetDraftChecks). Gated as revoke is: a signed-in session
// and a same-origin form (requireOperatorWrite), no fresh sign-in — it widens
// nothing an outside party is sent; the owner still approves every reply.
// Another owner's plugin, a revoked one or none is 404, as revoke's. Turning
// it off asks the owner about each draft that was awaiting a check (now an
// ordinary pending draft), with the usual push.
func (s *Server) handlePluginDraftChecksToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	on := r.PostFormValue("on")
	if err != nil || id <= 0 {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	}
	p, changed, lapsed, err := s.ext.Plugins.SetDraftChecks(r.Context(), id, on == "1")
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	case errors.Is(err, plugins.ErrInvalid):
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
		return
	default:
		s.logger.Printf("plugins: draft_checks %s for plugin %d: %v", on, id, err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "Checks replies could not be changed.")
		return
	}
	if changed {
		s.logger.Printf("plugins: plugin %d: draft_checks turned %v by its owner; %d awaiting draft(s) lapsed", id, on == "1", len(lapsed))
	}
	for _, d := range lapsed {
		m, merr := s.ext.Plugins.Message(r.Context(), d.MessageID)
		if merr != nil {
			m = plugins.Message{}
		}
		s.notifyDraftAwaitingNoting(r.Context(), p.OwnerID, d, m, DraftChecksOffNote)
		s.draftFiled(p.OwnerID, d)
	}
	http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
}

// DraftChecksOffNote follows a draft push's body when its plugin's Checks
// replies was turned off while the draft awaited the check.
const DraftChecksOffNote = " · the plugin no longer checks replies"

// pluginDraftChecksToggleHTML is a live messages plugin's Checks replies
// control on its /plugins card: one form posting the other state.
func pluginDraftChecksToggleHTML(p plugins.Plugin) string {
	if !p.Has(plugins.CapMessages) {
		return ""
	}
	on, label := "1", "Turn on Checks replies"
	if p.Has(plugins.CapDraftChecks) {
		on, label = "0", "Turn off Checks replies"
	}
	return `<form method="POST" action="` + pluginsPath + `/` + strconv.FormatInt(p.ID, 10) + `/draft-checks" data-plugin-draft-checks>
<input type="hidden" name="on" value="` + on + `"><button type="submit" class="` + flowsQuietButton + `">` + label + `</button></form>`
}

// renderPluginsPage writes /plugins. minted, when set, is a plugin token just
// created — the only time it is ever rendered.
func (s *Server) renderPluginsPage(w http.ResponseWriter, r *http.Request, code int, minted, problem string) {
	list, err := s.ext.Plugins.List(r.Context())
	if err != nil {
		s.logger.Printf("plugins: page list: %v", err)
		problem = "The plugin list could not be read."
	}
	queued, err := s.ext.Plugins.QueuedCounts(r.Context())
	if err != nil {
		s.logger.Printf("plugins: page queued counts: %v", err)
		problem = "The plugin list could not be read."
	}
	retryable, err := s.ext.Plugins.Retryable(r.Context(), pluginRetryShown)
	if err != nil {
		s.logger.Printf("plugins: page retryable: %v", err)
		problem = "The unconfirmed messages could not be read."
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="plugins-page">
<h1 class="text-balance text-base font-semibold text-fg">Plugins</h1>
<p class="text-sm text-fg2">A plugin is a separate process that brings messages from another system to your chief agent. It holds a plugin token, which can post messages here and do nothing else. Each message reaches your chief once, marked as untrusted data.</p>
`)
	b.WriteString(problemNode(problem))
	if minted != "" {
		b.WriteString(`<div class="flex flex-col gap-2 rounded-md bg-bg px-3 py-3 ring-1 ring-inset ring-edge" data-testid="minted-plugin-token">
<p class="text-sm font-semibold text-fg">Copy it now — it will not be shown again. It can only post this plugin's messages.</p>
<input id="minted-plugin-token" readonly value="` + esc(minted) + `" aria-label="New plugin token"
 class="min-h-[44px] w-full rounded-md bg-s1 px-3 py-2 font-mono text-sm text-fg ring-1 ring-inset ring-edge" onclick="this.select()">
</div>
`)
	}
	b.WriteString(`<form method="POST" action="` + pluginsPath + `" class="flex flex-col gap-2" data-testid="plugin-create">
<label for="plugin-name" class="text-sm font-medium text-fg">Name <span class="text-xs text-fg2">lower-case letters, digits and hyphens</span></label>
<input id="plugin-name" name="name" maxlength="40" placeholder="example-inbox" class="` + flowsInputClass + `">
<label for="plugin-label" class="text-sm font-medium text-fg">Label <span class="text-xs text-fg2">what your chief's conversation for it is called</span></label>
<input id="plugin-label" name="label" maxlength="` + strconv.Itoa(plugins.MaxLabelRunes) + `" placeholder="Example inbox" class="` + flowsInputClass + `">
<label for="plugin-scope" class="text-sm font-medium text-fg">Data scope <span class="text-xs text-fg2">where your chief is told to record anything durable from these messages</span></label>
<input id="plugin-scope" name="data_scope" maxlength="64" placeholder="example-inbox" class="` + flowsInputClass + `">
` + pluginViewerFieldsHTML(s.ext.PluginOrigins) + pluginDraftChecksFieldHTML + `<button type="submit" class="` + flowsButtonClass + `">Create plugin token</button>
</form>
<ul class="flex flex-col gap-2" data-testid="plugins">
`)
	if len(list) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No plugins yet.</li>`)
	}
	for _, p := range list {
		seen := "never seen"
		if p.LastSeenAt != nil {
			seen = "last seen " + p.LastSeenAt.UTC().Format("2006-01-02 15:04") + " UTC"
		}
		id := strconv.FormatInt(p.ID, 10)
		b.WriteString(`<li class="` + flowsCardClass + `" data-plugin-id="` + id + `">
<span class="break-all text-sm font-semibold text-fg">` + esc(p.Label) + ` <span class="font-mono text-xs text-fg2">` + esc(p.Name) + `</span></span>
<span class="text-xs text-fg2">scope ` + esc(p.DataScope) + ` · created ` + p.CreatedAt.UTC().Format("2006-01-02 15:04") + ` UTC</span>
<span class="text-xs text-fg2" data-plugin-capabilities>` + esc(pluginCapabilitiesText(p)) + `</span>
<span class="text-xs text-fg2"><span data-plugin-seen>` + seen + `</span> · <span data-plugin-queued="` + strconv.Itoa(queued[p.ID]) + `">` +
			queuedText(queued[p.ID]) + `</span></span>
`)
		if p.RevokedAt != nil {
			b.WriteString(`<span class="text-xs font-semibold text-fg2">revoked</span>`)
		} else {
			b.WriteString(pluginDraftChecksToggleHTML(p))
			b.WriteString(`<form method="POST" action="` + pluginsPath + `/` + id + `/revoke">
<button type="submit" class="` + flowsQuietButton + `">Revoke</button></form>`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n")
	b.WriteString(retryableSectionHTML(retryable, s.retryHasNoChief(r.Context(), retryable)))
	// Reply drafts moved to the Asks page (asks_drafts.go). A link to one of
	// them as it was (/plugins#draft-<id>, on a phone's older notification)
	// follows it there — REMOVABLE one DraftTTL (7 days) after the deploy
	// that moved the drafts: no draft notification older than that can still
	// be pending. The fragment never reached this server, so only the
	// page can.
	b.WriteString(`<p class="text-sm text-fg2" data-testid="plugins-rules-usage">Messages your chief would place the same way every time can skip it: <a href="` +
		routingRulesPath + `" class="text-accent underline">Routing rules</a>. What your chief costs: <a href="` + usagePath + `" class="text-accent underline">Usage</a>.</p>
`)
	b.WriteString(`<p class="text-sm text-fg2" data-testid="plugins-drafts-moved">Reply drafts are decided on the <a href="` + attentionPath + `?` +
		asksTabParam + `=` + asksTabDrafts + `" class="text-accent underline">Asks page</a>, under Reply drafts.</p>
<a href="/" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to muster</a>
</section>
<script>
(function () {
  if (/^#draft-[0-9]+$/.test(window.location.hash)) window.location.replace('` + attentionPath + `?` + asksTabParam + `=` + asksTabDrafts + `' + window.location.hash);
})();
</script>`)
	s.writeNavDocument(w, r, code, "Plugins", "", true, b.String())
}

// queuedText is a plugin's undelivered-message count in words.
func queuedText(n int) string {
	switch n {
	case 0:
		return "nothing waiting for your chief"
	case 1:
		return "1 message waiting for your chief"
	}
	return strconv.Itoa(n) + " messages waiting for your chief"
}

// pluginCreateInput reads the create form. With no `capability` field (the
// form while MUSTER_PLUGIN_ORIGINS is unset, and any older client) the token
// gets messages alone, as every token did before capabilities. `viewer` is
// accepted only while MUSTER_PLUGIN_ORIGINS is set, with an origin from it and
// an entry URL under that origin (plugins.ValidViewerLink). The set is fixed
// at mint, but for draft_checks (handlePluginDraftChecksToggle).
func (s *Server) pluginCreateInput(r *http.Request) (plugins.NewPlugin, string) {
	in := plugins.NewPlugin{Name: strings.TrimSpace(r.PostFormValue("name")), Label: r.PostFormValue("label"),
		DataScope: strings.TrimSpace(r.PostFormValue("data_scope"))}
	for _, c := range r.PostForm["capability"] {
		in.Capabilities = append(in.Capabilities, plugins.Capability(c))
	}
	if len(in.Capabilities) == 0 {
		in.Capabilities = []plugins.Capability{plugins.CapMessages}
	}
	// draft_checks is its own checkbox, drawn whatever MUSTER_PLUGIN_ORIGINS
	// says (pluginDraftChecksFieldHTML): a messages plugin that checks each
	// reply draft before its owner may approve it.
	if r.PostFormValue("draft_checks") == "1" && !slices.Contains(in.Capabilities, plugins.CapDraftChecks) {
		in.Capabilities = append(in.Capabilities, plugins.CapDraftChecks)
	}
	if !slices.Contains(in.Capabilities, plugins.CapViewer) {
		return in, ""
	}
	if len(s.ext.PluginOrigins) == 0 {
		return in, "This server does not offer viewer plugins (MUSTER_PLUGIN_ORIGINS is unset)."
	}
	origin, err := plugins.ValidViewerLink(s.ext.PluginOrigins, r.PostFormValue("origin"), r.PostFormValue("entry_url"))
	if err != nil {
		return in, strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")
	}
	in.Origin, in.EntryURL = origin, strings.TrimSpace(r.PostFormValue("entry_url"))
	return in, ""
}

// pluginViewerFieldsHTML is the create form's capability choice: drawn only
// while MUSTER_PLUGIN_ORIGINS is set, with the origin select offering exactly
// its patterns. Unset, the form is unchanged and mints messages tokens.
func pluginViewerFieldsHTML(origins []plugins.OriginPattern) string {
	if len(origins) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<fieldset class="flex flex-col gap-2" data-testid="plugin-capabilities">
<legend class="text-sm font-medium text-fg">What it may do <span class="text-xs text-fg2">fixed once created</span></legend>
<label class="flex min-h-[44px] items-center gap-2 text-sm text-fg"><input type="checkbox" name="capability" value="messages" checked> Messages: bring messages to your chief</label>
<label class="flex min-h-[44px] items-center gap-2 text-sm text-fg"><input type="checkbox" name="capability" value="viewer"> Viewer: a Preview button on your session views that opens it as you</label>
<label for="plugin-origin" class="text-sm font-medium text-fg">Viewer origin</label>
<select id="plugin-origin" name="origin" class="` + flowsInputClass + `">`)
	for _, o := range origins {
		b.WriteString(`<option value="` + html.EscapeString(o.String()) + `">` + html.EscapeString(o.String()) + `</option>`)
	}
	b.WriteString(`</select>
<label for="plugin-entry" class="text-sm font-medium text-fg">Viewer entry URL <span class="text-xs text-fg2">under the origin; where a hand-off is sent</span></label>
<input id="plugin-entry" name="entry_url" maxlength="2048" inputmode="url" class="` + flowsInputClass + `">
</fieldset>
`)
	return b.String()
}

// pluginDraftChecksFieldHTML is the create form's draft_checks choice
// (plugins.CapDraftChecks). Unlike the other capabilities it can change after
// the mint, on the plugin's card (handlePluginDraftChecksToggle). Unticked,
// the token is as every token before it.
const pluginDraftChecksFieldHTML = `<label class="flex min-h-[44px] items-center gap-2 text-sm text-fg" data-testid="plugin-draft-checks"><input type="checkbox" name="draft_checks" value="1"> Checks replies: the plugin checks each reply draft before you can approve it <span class="text-xs text-fg2">you can change it later on the plugin's card</span></label>
`

// pluginCapabilitiesText is a plugin's capabilities as its card shows them.
func pluginCapabilitiesText(p plugins.Plugin) string {
	parts := make([]string, 0, len(p.Capabilities))
	for _, c := range p.Capabilities {
		parts = append(parts, string(c))
	}
	out := "can: " + strings.Join(parts, ", ")
	if p.Origin != nil && p.EntryURL != nil {
		out += " · viewer at " + *p.EntryURL
	}
	return out
}
