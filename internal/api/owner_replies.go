package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// THE OWNER ANSWERED IT DIRECTLY: docs/plugins.md#when-the-owner-answers-it-directly.
//
// POST /plugin/v1/messages/{id}/owner-answered is a plugin's report that a
// reply to its message {id} was sent DIRECTLY — by the plugin's own sending
// tool, in a send stamped with that message — and not through route_reply.
// The message's route, if it is still waiting (routed), ends owner_answered:
// a paste still queued is cancelled and the chief is told in a note.
//
// 🔴 THE SIGNAL IS A STAMP ON A SEND, NEVER AN INFERENCE. muster settles the
// one message the stamp names. A reply typed by hand carries no stamp and
// settles nothing; muster reads no message text for who wrote what.
//
// 🔴 ONE PLUGIN, ONE OWNER. The plugin token acts as its owner (row-level
// security), and only a message the SAME plugin delivered is settled: another
// plugin's message, another owner's and a missing one are all 404.
//
// The answers:
//   - 200 {message_id, route_id, state: "owner_answered", settled: true}: the
//     route was waiting and is settled now;
//   - 200 {…, settled: false, state: "owner_answered"}: already settled (a
//     repeat of the stamp changes nothing);
//   - 409 {…, settled: false, state, error}: the route is not waiting —
//     answered, relayed, fallback, timed_out or failed, or "none" when the
//     message was never routed. Nothing changes: a hand-back already owed or
//     delivered stands. Final; a plugin does not retry it;
//   - 404 for a message that is not this plugin's, and with routing off.

const pluginOwnerAnsweredPath = pluginAPIPrefix + "/messages/{id}/owner-answered"

// routeStateNone is the state the owner-answered door reports for a message
// that was never routed.
const routeStateNone = "none"

func (s *Server) handlePluginOwnerAnswered(w http.ResponseWriter, r *http.Request) {
	if !routingOn(s.ext) {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such message"})
		return
	}
	p := pluginFromContext(r.Context())
	route, settled, err := s.ext.Routes.SettleOwnerAnswered(r.Context(), p.ID, id)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such message"})
		return
	default:
		s.logger.Printf("routes: plugin %d: owner answered message %d: %v", p.ID, id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the stamp could not be recorded"})
		return
	}
	state := string(route.State)
	if route.RouteID == "" {
		state = routeStateNone
	}
	out := map[string]any{"message_id": id, "settled": settled, "state": state}
	if route.RouteID != "" {
		out["route_id"] = route.RouteID
	}
	switch {
	case settled:
		s.endOwnerAnswered(context.WithoutCancel(r.Context()), route)
		s.writeJSON(w, http.StatusOK, out)
	case route.State == plugins.RouteOwnerAnswered:
		s.writeJSON(w, http.StatusOK, out)
	default:
		out["error"] = "the message's route is not waiting for an answer (" + state + "), so nothing was changed"
		s.writeJSON(w, http.StatusConflict, out)
	}
}

// ownerAnsweredNote is the chief's note for a route that ended owner_answered.
const ownerAnsweredNote = "its reply was sent to the sender directly (a send stamped with this message), " +
	"so it needs nothing more."

// endOwnerAnswered finishes a route SettleOwnerAnswered just ended: its paste,
// if still queued, is cancelled (were the cancel to fail, the route's gate
// refuses the paste: sendQueueRoutedGate takes only a routed route), and the
// chief is told in a note — never a turn: there is nothing for it to do.
func (s *Server) endOwnerAnswered(ctx context.Context, r plugins.Route) {
	s.logger.Printf("routes: %s: answered directly (the plugin's stamp on message %d)", r.RouteID, r.MessageID)
	if r.SendQueueID != nil && s.ext.SendQueue != nil {
		switch _, err := s.ext.SendQueue.Cancel(ctx, *r.SendQueueID, s.sendQueueNow()); {
		case err == nil:
			s.logger.Printf("routes: %s: its queued message %d was cancelled", r.RouteID, *r.SendQueueID)
		case errors.Is(err, sendqueue.ErrNotCancellable), errors.Is(err, sendqueue.ErrNotFound):
			// Already pasted (the session has it, and a late route_reply is
			// refused: the route is closed), or deleted once finished.
		default:
			s.logger.Printf("routes: %s: cancel queued message %d: %v (its route gate refuses it)", r.RouteID, *r.SendQueueID, err)
		}
	}
	s.noteToChief(ctx, r, ownerAnsweredNote)
}
