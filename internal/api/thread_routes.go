package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A reply on a thread a session started: docs/plugins.md#a-reply-on-a-thread-a-session-started.

const pluginThreadsPath = "/api/plugin-threads"

// handlePluginThreadBind is POST /api/plugin-threads {thread, claude_session}.
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
	if _, ok := s.mirrorEntryForSession(r.Context(), sub.Key); !ok {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "no online mirrored window shows Claude session " + sub.Key + " now, so the thread is not bound"})
		return
	}
	created, err := s.ext.Routes.BindThread(r.Context(), in.Thread, sub.Key)
	if err != nil {
		s.logger.Printf("threads: bind %q: %v", in.Thread, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the thread could not be bound"})
		return
	}
	if !created {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.logger.Printf("threads: thread %q bound to %s", in.Thread, sub.Key)
	w.WriteHeader(http.StatusCreated)
}

// routeByThread routes a NEW message m of plugin p — agent mail, on a thread
// one of the owner's sessions started — to that session: the chief's
// route (routeItem), made by muster, so the chief decides nothing. It reports
// whether m is taken care of; false leaves m queued for the ordinary delivery
// to the chief, as for any message — which is what happens when the route
// cannot be made (no online mirrored window shows the session, say), or there
// is no chief to own the route.
func (s *Server) routeByThread(ctx context.Context, p plugins.Plugin, m plugins.Message) bool {
	if !routingOn(s.ext) || s.pluginDeliverer == nil || p.Name != plugins.AgentMailPlugin || m.Thread == nil {
		return false
	}
	thread := *m.Thread
	session, err := s.ext.Routes.BoundThread(ctx, thread)
	if err != nil {
		if !errors.Is(err, plugins.ErrNotFound) {
			s.logger.Printf("threads: message %d: read thread %q: %v", m.ID, thread, err)
		}
		return false
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil {
		return false
	}
	sub := focus.Subject{Kind: focus.KindMirror, Key: session}
	origin := plugins.RouteOrigin{Via: plugins.RouteViaThread, Session: session, Thread: thread}
	r, _, refused := s.routeItem(ctx, chief.ID, agentOwnerID(chief), routedItem{msg: m}, sub, origin)
	item := "plugin message_id " + strconv.FormatInt(m.ID, 10)
	var note string
	switch {
	case refused == "":
		note = routeNote(r, "routed by muster to the session that started thread "+thread+"; its answer is relayed as usual. Nothing to do unless it comes back to you.")
	case refusalCode(refused) == RefusedInjectFailed:
		// The route was made (the message is the chief's) and failed, owing the
		// chief its hand-back.
		note = "[muster] " + item + ", a reply on thread " + thread + ", could not be put into the session that started the thread; it comes back to you."
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
