package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// THE MIRRORED SESSIONS' MCP DOOR: POST /mirror/mcp.
//
// A Claude Code session running in a mirrored tmux window is no agent row and
// holds no muster credential. Its host's muster-mirror serves a loopback-only
// MCP endpoint (internal/mirrorhost's MCPProxy) that forwards each call here
// with the HOST's mirror token and the calling session's Claude session id
// (X-Muster-Claude-Session) — so the session gets the "all agents" tools:
// attention_ask/wait/cancel, and the task tools over the tasks THAT SESSION
// created (taskScopeOf) — plus attention_answer, the operator answering one of
// their asks through their own session, and connector_requests_list /
// connector_request_decide, the operator deciding one of their agents'
// connector requests through it (connector_decide_tools.go), and
// plugin_drafts_list / plugin_draft_decide, the operator approving or
// rejecting a plugin reply draft through it (plugin_draft_tools.go), which no
// dispatched agent gets — and, with session routing on, route_reply: the
// answer to a message the chief routed to THAT session (route_reply_tool.go).
//
// 🔴 A MIRROR TOKEN NOW OPENS A SECOND ROUTE, AND THIS ONE IS NARROW. Until this
// door, mmt_ opened /mirror/connect and nothing else. Here it opens exactly
// mirrorTools and nothing else: no chief tool (they are not listed, and
// serveMCP refuses an unlisted name), no agent connector tool, no flow. So a LEAKED mmt_
// reaches, beyond what /mirror/connect already gives, the asks of that host's
// sessions (file, wait, cancel), the tasks those sessions created, and — 🔴
// through attention_answer — an answer AS THE OPERATOR to any of the owner's
// open asks not raised by a mirrored session on that host (the chief's
// included — and, by operator decision, as agent_dispatch instructions too:
// a leaked mmt_ can write a dispatched agent's prompt; the kickoff and audit
// row name the host it came from) — and 🔴 through connector_request_decide,
// an APPROVAL AS THE OPERATOR of any of the owner's agents' pending connector
// requests (operator decision "Build connector_approve MCP"; the grant and the
// audit line name the host it came from) — and 🔴 through plugin_drafts_list
// and plugin_draft_decide, a READ of every pending reply draft of the owner's
// (the inbound sender, subject and text, and the reply) and an APPROVAL AS THE
// OPERATOR of any of them, which SENDS that text to an outside party, with no
// fresh sign-in (operator decision; the audit row names the host; any of the
// owner's sessions may approve, the one a message was routed to included) —
// and 🔴 through agent_destroy
// (agent_destroy_tool.go, operator decision), the teardown of any agent of
// the owner's but the chief, with NO audit row (the server log names the host) — and 🔴
// through chief_restart (chief_restart_tool.go, operator ask "wire the mcp so this can be
// done more easily"), a Stop then Start of the owner's OWN chief (refused mid-turn unless
// force, which cuts the turn off as the browser's Stop does), again with no audit row — and 🔴
// through dispatch_request / dispatch_approve (dispatch_hold_tools.go, operator decision
// "dispatch and then another verb to approve"), with the chief's dispatch on, the START OF A
// NEW AGENT as the owner with a prompt the caller writes, within the chief's caps (an audit
// row per request and per approval names the host and session; each start pushes the owner).
// Never another task, another host's session, another owner's ask
// or request, a secret, or a chief tool. The SESSION a call names is the caller's claim (its pane);
// only the HOST is proven, which is why attention_answer's rule is per host.
//
// 🔴 A HOST ACTS ONLY AS ITS OWN SESSIONS. The token names its host; the
// session id is the caller's claim, and it is believed only when THAT host is
// connected now and lists a window running that Claude session in any pane
// (mirrorSessionOf). It is checked at each tools/call (handleMirrorMCP), not
// at connect: another host's session, a session no window shows, or a token
// of another owner gets a tool error and the tool does not run, so one host
// cannot ask, wait or read as another host's session. initialize and
// tools/list need only the token: they say nothing session-specific.
//
// With session routing on, plugin_messages_list and plugin_message_route
// (plugin_message_route_tools.go) widen what an mmt_ reaches: docs/auth.md
// ("Mirror tokens and session routing").
//
// 🔴 A MIRRORED OPENCODE SESSION GETS A NARROWER SET (openCodeMirrorTools;
// docs/attention.md, "Mirrored opencode sessions"). Its host forwards its call
// with the KEY of the opencode session it bound the call to
// (X-Muster-Agent-Session, "opencode:<id>") and NO Claude session, and marks
// all its requests X-Muster-Client: opencode. The call is believed only when
// that host lists exactly ONE window showing that session in its active pane
// AND sends to it (mirror.Window.RunsMCPCaller). Such a caller is offered the
// attention tools and the task tools, nothing else: every operator power
// (attention_answer, the decide tools, agent_destroy, chief_restart, the
// dispatch verbs, plugin routing) and every tool built on a Claude Code
// session (chief_ask, route_reply, agent_report_done) is refused by name, at
// the call, whatever was listed.
// ---------------------------------------------------------------------------

// mirrorSessionHeader carries the calling Claude Code session's id.
const mirrorSessionHeader = "X-Muster-Claude-Session"

// mirrorAgentSessionHeader carries, INSTEAD, the calling opencode session's
// key (mirror.OpenCodeCallerKey); mirrorClientHeader: mirrorClientOpenCode
// marks every request of an opencode caller (mirrorhost's MCPClientHeader).
const (
	mirrorAgentSessionHeader = "X-Muster-Agent-Session"
	mirrorClientHeader       = "X-Muster-Client"
	mirrorClientOpenCode     = "opencode"
)

// openCodeMirrorTools is the tools the door offers a mirrored opencode
// session, by name: the attention tools — its answer is left for its
// attention_wait, as every mirrored session's is (never typed into its pane)
// — and the task tools, over the tasks that session created and its own host
// dispatch's task (taskScopeOf). muster_help is serveMCP's own.
var openCodeMirrorTools = map[string]bool{
	attentionAskTool: true, attentionWaitTool: true, attentionCancelTool: true,
	tasksListTool: true, taskGetTool: true, taskCommentTool: true, taskSetStatusTool: true,
}

// openCodeToolRefusal is the door's answer — a sentence, for a tool error —
// to an opencode session naming a tool of the door it is not offered.
func openCodeToolRefusal(name string) string {
	return name + " is not offered to a mirrored opencode session: it gets attention_ask, attention_wait, attention_cancel and the task tools; " +
		"use one of your owner's Claude Code sessions for it"
}

// mirrorHostCtxKey carries the authenticated host token through the request.
type mirrorHostCtxKey struct{}

func (s *Server) registerMirrorMCPRoutes(mux Mux) {
	mux.HandleFunc("POST /mirror/mcp", s.requireMirrorHostToken(s.handleMirrorMCP))
}

// requireMirrorHostToken admits a mirror host's token (Bearer or
// X-Muster-Token) as its owner, through authz.ViaMirrorHost. No other
// credential opens it.
func (s *Server) requireMirrorHostToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.MirrorTokens == nil {
			http.NotFound(w, r)
			return
		}
		// The failure limit, checked BEFORE the lookup: only FAILED token
		// checks count, per (client address, presented token), and that pair
		// past the limit gets 429 without touching the database until its
		// window (mirrorMCPFailWindow) has passed. 🔴 PER TOKEN, NOT PER
		// ADDRESS: hosts behind one NAT or one Cloudflare edge share an
		// address, so a stale token on one host must never refuse another
		// host's valid token.
		tok := hookTokenFromRequest(r)
		key := mirrorMCPFailKey(clientIP(r), tok)
		if s.mirrorMCPFails.count(key, s.now(), mirrorMCPFailWindow) >= mirrorMCPFailLimit {
			s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed checks of this mirror token from this address; wait a minute"})
			return
		}
		refuse := func() {
			s.mirrorMCPFails.add(key, s.now(), mirrorMCPFailWindow)
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "this route takes a mirror host token (mmt_…)"})
		}
		if !mirror.TokenShapeOK(tok) {
			refuse()
			return
		}
		// Looked up on EVERY call: a revoked token is refused at its next call,
		// and a wait already in flight is bounded by attentionMaxWait.
		t, err := s.mirrorHostFor(r.Context(), tok)
		switch {
		case err == nil:
		case errors.Is(err, mirror.ErrBadToken):
			refuse()
			return
		default:
			s.logger.Printf("mirror mcp: token lookup failed: %v", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the mirror token"})
			return
		}
		ctx := context.WithValue(r.Context(), mirrorHostCtxKey{}, t)
		s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), t.OwnerID, authz.ViaMirrorHost))
	}
}

// mirrorHostFor resolves a mirror token to its row.
//
// 🔴 IT RUNS UNDER authz.System FOR THE REASON pluginFor DOES: it is how the
// request's owner is LEARNED, and mirror_tokens is under FORCE row-level
// security. The System context is used for this one store call and never
// handed on. System-ledger entry.
func (s *Server) mirrorHostFor(ctx context.Context, tok string) (mirror.Token, error) {
	return s.ext.MirrorTokens.Authenticate(authz.System(ctx), tok)
}

// The /mirror/mcp failure limit: past this many failed checks of one token
// from one address within the window, that token from that address is
// answered 429.
const (
	mirrorMCPFailLimit  = 10
	mirrorMCPFailWindow = time.Minute
)

// mirrorMCPFailKey is the failure counter's key: the client address and a
// hash of the presented token (never the token itself, so the map holds no
// credential). The map stays capped (boundedWindow).
func mirrorMCPFailKey(addr, tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return addr + " " + hex.EncodeToString(sum[:16])
}

// mirrorSessionOf is the window of the principal's host `host` running the
// mirrored session sid names now (mirror.Window.RunsMCPCaller: a Claude
// session in its active pane or another pane of it — a split — or an opencode
// caller's key in its active pane, sent to), and whether there is one: the
// host must be online and list it. 🔴 An opencode key two windows show is
// none: which one called is unknown.
func (s *Server) mirrorSessionOf(ctx context.Context, host, sid string) (mirror.SessionView, bool) {
	if s.ext.Mirror == nil || !mirror.ValidMCPCaller(sid) {
		return mirror.SessionView{}, false
	}
	hosts := s.ext.Mirror.Hosts(ctx)
	var window string
	found := 0
	for _, h := range hosts {
		if h.Host != host || !h.Online {
			continue
		}
		for _, w := range h.Windows {
			if w.RunsMCPCaller(sid) {
				window = h.Host + "\x00" + w.Session + "\x00" + w.ID
				found++
			}
		}
	}
	if found == 0 || (found > 1 && mirror.OpenCodeMCPCaller(sid)) {
		return mirror.SessionView{}, false
	}
	for _, e := range mirror.Ordered(hosts) {
		if e.Host+"\x00"+e.Session+"\x00"+e.Window == window {
			return e, true
		}
	}
	return mirror.SessionView{}, false
}

// mirrorTools is the mirrored sessions' tool set: the all-agents attention and
// task tools, and attention_answer — the operator answering an ask through
// their own session — with the attention store; and connector_requests_list /
// connector_request_decide — the operator deciding a connector request through
// their own session — with the connector store; plugin_drafts_list /
// plugin_draft_decide — the operator deciding a reply draft — with the plugin
// store; and agent_destroy — the
// operator tearing down one of their agents — with the attention tools; and
// dispatch_request / dispatch_approve — the operator starting an agent in two
// verbs — with the chief's dispatch on. No
// dispatched agent is offered any operator tool, and their handlers refuse any caller but a mirrored
// session.
//
// chief_ask is listed whether or not the owner has a chief that is on, and
// that is deliberate: Claude Code reads a tools list once, at connect, so a
// listing that followed the switch would be stale for every session that
// connected before it moved. The CALL decides instead (toolChiefAsk): with
// the chief off or absent it answers, as a tool error the model reads, to ask
// the owner with attention_ask.
func (s *Server) mirrorTools() []agents.ToolDef {
	var out []agents.ToolDef
	if s.attentionToolsOn() {
		out = append(append(append(append(out, attentionToolDefs()...), taskToolDefs()...), attentionAnswerToolDef()), agentDestroyToolDef())
		// chief_ask — a session's question to the owner's chief
		// (chief_ask_tool.go) — when there are agents to find the chief among.
		if s.chiefAskOn() {
			out = append(out, chiefAskToolDef())
		}
	}
	// chief_restart — the operator restarting their own chief through their own
	// session (chief_restart_tool.go) — with the agent store and a provisioner.
	if s.chiefRestartOn() && !s.featureOff(features.Dispatch) {
		out = append(out, chiefRestartToolDef())
	}
	if s.connectorDecideToolsOn() && !s.featureOff(features.Connectors) {
		out = append(out, connectorDecideToolDefs()...)
	}
	// plugin_drafts_list / plugin_draft_decide — the operator deciding a reply
	// draft through their own session — with the plugin store.
	if s.pluginDraftToolsOn() && !s.featureOff(features.Plugins) {
		out = append(out, pluginDraftToolDefs()...)
	}
	// route_reply — a routed session's answer (route_reply_tool.go) — with
	// session routing on.
	if routesOn(s.ext) && !s.featureOff(features.SessionRouting) {
		out = append(out, routeReplyToolDef())
	}
	// plugin_messages_list / plugin_message_route — the owner routing a plugin
	// message from their own session (plugin_message_route_tools.go) — with
	// session routing on.
	if s.pluginMessageRouteToolsOn() && !s.featureOff(features.SessionRouting) {
		out = append(out, pluginMessageRouteToolDefs()...)
	}
	// dispatch_request / dispatch_approve — the owner starting an agent from
	// their own session in two verbs (dispatch_hold_tools.go) — with the
	// chief's dispatch on: the same opt-in, ledger and caps. Gated on
	// features.TaskRouting, as agent_dispatch is on the agent door, and NOT on
	// features.Dispatch as chief_restart is: chief_restart needs only a
	// provisioner (features.Dispatch is "a provisioner is wired"), while these
	// need the chief's dispatch ledger and owner list, which exist only with
	// task routing on (chiefDispatchOn). A missing provisioner is refused at the
	// call (chiefDispatchDeploymentRefusal), with its own sentence.
	if s.dispatchHoldToolsOn() && !s.featureOff(features.TaskRouting) {
		out = append(out, dispatchHoldToolDefs()...)
	}
	// agent_report_done — a HOST-SPAWNED agent saying it finished
	// (host_dispatch_done.go) — with the host-dispatch store. Listed for every
	// session (tools/list carries no session); the call binds it to the
	// caller's own dispatch and refuses any other session.
	if s.hostDispatches() != nil && !s.featureOff(features.TaskRouting) {
		out = append(out, agentReportDoneToolDef())
	}
	return out
}

func (s *Server) handleMirrorMCP(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	t, _ := r.Context().Value(mirrorHostCtxKey{}).(mirror.Token)
	disabled, off := s.mcpFeatureHooks(mirrorDoor)
	sid := strings.TrimSpace(r.Header.Get(mirrorSessionHeader))
	// An opencode caller (its host marks it, and names its session by KEY):
	// the narrower set, refused by name at the call (openCodeMirrorTools).
	agentKey := strings.TrimSpace(r.Header.Get(mirrorAgentSessionHeader))
	openCode := agentKey != "" || strings.EqualFold(strings.TrimSpace(r.Header.Get(mirrorClientHeader)), mirrorClientOpenCode)
	tools := s.mirrorTools
	instructions := "muster's tools for a mirrored Claude Code session."
	if openCode {
		tools = func() []agents.ToolDef {
			var out []agents.ToolDef
			for _, d := range s.mirrorTools() {
				if openCodeMirrorTools[d.Name] {
					out = append(out, d)
				}
			}
			return out
		}
		all := disabled
		disabled = func(name string) string {
			if slices.ContainsFunc(s.mirrorTools(), func(d agents.ToolDef) bool { return d.Name == name }) {
				return openCodeToolRefusal(name)
			}
			if all == nil {
				return ""
			}
			return all(name)
		}
		instructions = "muster's tools for a mirrored opencode session."
	}
	if s.attentionToolsOn() {
		instructions += " " + mirrorAttentionInstruction
	}
	// 🔴 THE SESSION IS BOUND AT tools/call, NOT AT CONNECT. initialize,
	// tools/list, ping and notifications say nothing session-specific — the
	// host's token (checked above) is all they need. Claude Code connects at
	// startup, before its session has a transcript, so no host lists it yet;
	// binding there failed the connection, and Claude Code never retries one.
	// A call is bound to a window this host lists running that session — or
	// refused as a TOOL error the model sees.
	s.serveMCP(w, r, mcpSurface{
		tools: tools,
		call: func(name, args string) string {
			if openCode {
				// 🔴 FAILS CLOSED, BY NAME: whatever was listed, an opencode
				// caller runs only its own set, and only bound by its key.
				if !openCodeMirrorTools[name] {
					return toolErr("%s", openCodeToolRefusal(name))
				}
				if sid != "" || !mirror.OpenCodeMCPCaller(agentKey) {
					return toolErr("an opencode caller is bound by its opencode session (its MCP endpoint sends %s: opencode:<id>), never by a Claude session", mirrorAgentSessionHeader)
				}
				e, ok := s.awaitMirrorSession(r.Context(), t.Host, agentKey)
				if !ok {
					return toolErr("host %s lists no one window running opencode session %q that it sends to now; a host acts only as its own sessions",
						t.Host, mirror.SessionIDOf(agentKey))
				}
				c := toolCaller{asker: attention.Asker{MirrorHost: t.Host, MirrorSession: agentKey, Label: s.mirrorLabel(r.Context(), e)}, mirrorWindow: e.Window}
				if text, ok := s.dispatchCallerTool(r.Context(), c, name, args); ok {
					return text
				}
				return toolErr("unknown tool %q", name)
			}
			e, ok := s.awaitMirrorSession(r.Context(), t.Host, sid)
			if !ok {
				return toolErr("host %s lists no window running Claude session %q now; a host acts only as its own sessions (its MCP endpoint sends %s)",
					t.Host, sid, mirrorSessionHeader)
			}
			c := toolCaller{asker: attention.Asker{MirrorHost: t.Host, MirrorSession: sid, Label: s.mirrorLabel(r.Context(), e)}, mirrorWindow: e.Window}
			// chief_ask is this door's alone, like route_reply below (listed only
			// with chiefAskOn, and serveMCP refuses an unlisted name).
			if name == chiefAskTool {
				return s.toolChiefAsk(r.Context(), c, args)
			}
			// So is chief_restart (listed only with chiefRestartOn): it is not in
			// dispatchCallerTool, so no gateway agent can name it.
			if name == chiefRestartTool {
				return s.toolChiefRestart(r.Context(), c, args)
			}
			// route_reply is this door's alone: it is not in dispatchCallerTool,
			// which a gateway agent reaches naming any tool.
			if name == routeReplyToolName {
				return s.toolRouteReply(r.Context(), c, args)
			}
			// So are plugin_messages_list / plugin_message_route.
			if text, ok := s.dispatchPluginMessageRouteTool(r.Context(), c, name, args); ok {
				return text
			}
			// And dispatch_request / dispatch_approve (listed only with
			// dispatchHoldToolsOn): not in dispatchCallerTool, so no dispatched
			// agent and no chief can name them; each refuses any caller but a
			// mirrored session again inside.
			if text, ok := s.dispatchHoldTool(r.Context(), c, name, args); ok {
				return text
			}
			// agent_report_done here is the host-dispatch form (listed only with
			// the host-dispatch store): bound to THIS session's own dispatch.
			if name == agentReportDoneTool {
				return s.toolHostReportDone(r.Context(), t.Host, sid, e.Window, args)
			}
			if text, ok := s.dispatchCallerTool(r.Context(), c, name, args); ok {
				return text
			}
			return toolErr("unknown tool %q", name)
		},
		instructions: instructions,
		disabled:     disabled,
		offFeatures:  off,
	})
}

// mirrorBindWait bounds how long a tools/call waits for its host's listing to
// show the calling session (a var so a test can shorten it). The host pushes
// a fresh listing right before it forwards a call (mirrorhost.MCPProxy's
// Refresh), so this covers only that message and this request crossing.
var mirrorBindWait = 3 * time.Second

// awaitMirrorSession is mirrorSessionOf, re-read until it holds or
// mirrorBindWait (or the request) ends. A malformed or empty session id is
// refused at once: no listing can ever show it.
func (s *Server) awaitMirrorSession(ctx context.Context, host, sid string) (mirror.SessionView, bool) {
	if !mirror.ValidMCPCaller(sid) {
		return mirror.SessionView{}, false
	}
	deadline := time.Now().Add(mirrorBindWait)
	for {
		if e, ok := s.mirrorSessionOf(ctx, host, sid); ok {
			return e, true
		}
		if !time.Now().Before(deadline) {
			return mirror.SessionView{}, false
		}
		select {
		case <-ctx.Done():
			return mirror.SessionView{}, false
		case <-time.After(50 * time.Millisecond):
		}
	}
}
