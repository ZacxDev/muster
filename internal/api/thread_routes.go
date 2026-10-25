package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A REPLY ON A THREAD A SESSION STARTED (internal/plugins/threads.go, migration
// 0054): docs/plugins.md#a-reply-on-a-thread-a-session-started.
//
//	POST /api/plugin-threads (the session's PostToolUse hook, after a send that
//	   started a thread) ──▶ plugin_threads: thread → that session, first report wins
//	POST /plugin/v1/messages with that thread (agent mail only) ──▶ routeByThread:
//	   the CHIEF'S ROUTE (routeItem), made by muster, to that session — or, when
//	   no online window shows it, the ordinary delivery to the chief

const pluginThreadsPath = "/api/plugin-threads"

// handlePluginThreadBind is POST /api/plugin-threads {thread, claude_session}:
// the session that started thread, with its host's muster credential (a
// personal token, or the hook token — route reply's door), acting as the
// owner. The host is the one the mirror shows the session on, never the
// caller's word. 201: bound. 200: already bound, the stored binding unchanged.
// 409: no online mirrored window shows the session, so nothing is bound.
func (s *Server) handlePluginThreadBind(w http.ResponseWriter, r *http.Request) {
	if !routingOn(s.ext) {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Thread        string `json:"thread"`
		ClaudeSession string `json:"claude_session"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || !plugins.ValidThread(in.Thread) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"thread": "<thread id>", "claude_session": "<claude session id>"}`})
		return
	}
	sub, err := focus.ParseSubject(string(focus.KindMirror) + ":" + in.ClaudeSession)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "claude_session is not a Claude session id"})
		return
	}
	e, ok := s.mirrorEntryForSession(r.Context(), sub.Key)
	if !ok {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "no online mirrored window shows Claude session " + sub.Key + " now, so the thread is not bound"})
		return
	}
	b, created, err := s.ext.Routes.BindThread(r.Context(), in.Thread, e.Host, sub.Key)
	switch {
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
		return
	case err != nil:
		s.logger.Printf("threads: bind %q: %v", in.Thread, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the thread could not be bound"})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.logger.Printf("threads: thread %q bound to %s/%s", b.Thread, b.Host, b.ClaudeSession)
	}
	s.writeJSON(w, status, map[string]any{"thread": b.Thread, "host": b.Host, "claude_session": b.ClaudeSession,
		"created_at": b.CreatedAt.UTC().Format(time.RFC3339), "bound": created})
}

// routeByThread routes a NEW message m of plugin p — agent mail, replyable, on
// a thread one of the owner's sessions started — to that session: the chief's
// route (routeItem), made by muster, so the chief decides nothing. It reports
// whether m is taken care of; false leaves m queued for the ordinary delivery
// to the chief, as for any message — which is what happens when no online
// mirrored window shows the session on the host it was bound from, or there is
// no chief to own the route.
func (s *Server) routeByThread(ctx context.Context, p plugins.Plugin, m plugins.Message) bool {
	if !routingOn(s.ext) || s.pluginDeliverer == nil || p.Name != plugins.AgentMailPlugin || m.Thread == nil || m.NoReply {
		return false
	}
	b, err := s.ext.Routes.BoundThread(ctx, *m.Thread)
	if err != nil {
		if !errors.Is(err, plugins.ErrNotFound) {
			s.logger.Printf("threads: message %d: read thread %q: %v", m.ID, *m.Thread, err)
		}
		return false
	}
	if e, ok := s.mirrorEntryForSession(ctx, b.ClaudeSession); !ok || e.Host != b.Host {
		s.logger.Printf("threads: message %d: thread %q's session %s/%s is not online; delivering it to the chief", m.ID, b.Thread, b.Host, b.ClaudeSession)
		return false
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil {
		return false
	}
	sub := focus.Subject{Kind: focus.KindMirror, Key: b.ClaudeSession}
	origin := plugins.RouteOrigin{Via: plugins.RouteViaThread, Host: b.Host, Session: b.ClaudeSession, Thread: b.Thread}
	r, _, refused := s.routeItem(ctx, chief.ID, agentOwnerID(chief), routedItem{msg: m}, sub, origin)
	item := "plugin message_id " + strconv.FormatInt(m.ID, 10)
	var note string
	switch {
	case refused == "":
		note = routeNote(r, "routed by muster to the session that started thread "+b.Thread+"; its answer is relayed as usual. Nothing to do unless it comes back to you.")
	case refusalCode(refused) == RefusedInjectFailed:
		// The route was made (the message is the chief's) and failed, owing the
		// chief its hand-back.
		note = "[muster] " + item + ", a reply on thread " + b.Thread + ", could not be put into the session that started the thread; it comes back to you."
	default:
		cur, err := s.ext.Plugins.Message(ctx, m.ID)
		if err == nil && cur.Status != plugins.StatusQueued {
			// Another delivery took it meanwhile: it is the chief's already.
			return true
		}
		s.logger.Printf("threads: message %d: not routed (%s); delivering it to the chief", m.ID, refused)
		return false
	}
	cur, err := s.ext.Plugins.Message(ctx, m.ID)
	if err != nil {
		cur = m
	}
	if err := s.pluginDeliverer.Handed(ctx, p, chief, cur, note); err != nil {
		s.logger.Printf("threads: message %d: record it in the chief's conversation: %v", m.ID, err)
	}
	if refused != "" {
		s.kickRouteSweep()
	}
	return true
}

// refusalCode is the `refused` code of a routeRefusal JSON ("" for none).
func refusalCode(text string) string {
	var v struct {
		Refused string `json:"refused"`
	}
	_ = json.Unmarshal([]byte(text), &v)
	return v.Refused
}
