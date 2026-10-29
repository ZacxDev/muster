package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// THE OWNER ANSWERED IT THEMSELVES: docs/plugins.md#when-you-answer-it-yourself.
//
// POST /plugin/v1/owner-replies {thread, at} is a plugin's report that the
// owner posted on upstream conversation `thread` at `at` — a reply the owner
// sent by hand, not through muster. Every route of THIS plugin's messages on
// that thread that the reply answers (plugins.SettleOwnerReplied: made before
// it, and still waiting or timed out at most OwnerReplyGrace before it) ends
// owner_answered: a paste still queued is cancelled, a hand-back still owed is
// withdrawn, and the chief is told in a note.
//
// 🔴 THE SIGNAL IS THE PLUGIN'S EXPLICIT REPORT, NEVER TEXT. muster does not
// read a message body for who wrote it; the plugin, which can tell the owner's
// own posts from the sender's, says so in this one structured call.
//
// 🔴 ONE PLUGIN, ONE OWNER. The plugin token acts as its owner (row-level
// security), and only routes of messages the SAME plugin delivered are
// candidates, so one plugin cannot settle another plugin's routes, and no
// plugin another owner's.

const (
	pluginOwnerRepliesPath = pluginAPIPrefix + "/owner-replies"
	// ownerReplySkew is how far in the future a reported `at` may be: a
	// plugin's clock against muster's. Past it the report is refused rather
	// than allowed to settle routes made after now.
	ownerReplySkew = 5 * time.Minute
)

// handlePluginOwnerReply is POST /plugin/v1/owner-replies {thread, at}: 200
// {"settled": [route ids]} (an empty list when the reply answers no route).
// 404 with session routing off, as every routing door.
func (s *Server) handlePluginOwnerReply(w http.ResponseWriter, r *http.Request) {
	if !routingOn(s.ext) {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Thread string     `json:"thread"`
		At     *time.Time `json:"at"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.At == nil || !plugins.ValidThread(in.Thread) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"thread": "<thread id>", "at": "<RFC 3339 time of the owner's reply>"}`})
		return
	}
	if in.At.After(time.Now().Add(ownerReplySkew)) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at is in the future"})
		return
	}
	p := pluginFromContext(r.Context())
	settled, err := s.ext.Routes.SettleOwnerReplied(r.Context(), p.ID, in.Thread, *in.At)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
		return
	default:
		s.logger.Printf("routes: plugin %d: owner reply on %q: %v", p.ID, in.Thread, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the owner's reply could not be recorded"})
		return
	}
	ids := make([]string, 0, len(settled))
	for _, st := range settled {
		s.endOwnerAnswered(context.WithoutCancel(r.Context()), st)
		ids = append(ids, st.Route.RouteID)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"settled": ids})
}

// Why a route ended owner_answered, as its chief's note says it.
const (
	ownerAnsweredNote = "your owner answered the sender directly, on the conversation itself, so it needs nothing more."
	// ownerAnsweredAfterHandBack: the route had timed out and its hand-back
	// was already delivered before the owner's reply was reported.
	ownerAnsweredAfterHandBack = "your owner answered the sender directly after it came back to you, so it needs nothing more: " +
		"no reply is needed from you."
)

// endOwnerAnswered finishes a route SettleOwnerReplied ended: its paste, if
// still queued, is cancelled (and were the cancel to fail, the route's gate
// refuses the paste: sendQueueRoutedGate takes only a routed route), and the
// chief is told in a note — never a turn: there is nothing for it to do.
func (s *Server) endOwnerAnswered(ctx context.Context, st plugins.OwnerSettled) {
	r := st.Route
	s.logger.Printf("routes: %s: answered by the owner directly (reported for %s; was %s)", r.RouteID,
		r.OwnerRepliedAt.UTC().Format(time.RFC3339), st.From)
	if st.From == plugins.RouteRouted && r.SendQueueID != nil && s.ext.SendQueue != nil {
		switch _, err := s.ext.SendQueue.Cancel(ctx, *r.SendQueueID, s.sendQueueNow()); {
		case err == nil:
			s.logger.Printf("routes: %s: its queued message %d was cancelled", r.RouteID, *r.SendQueueID)
		case errors.Is(err, sendqueue.ErrNotCancellable), errors.Is(err, sendqueue.ErrNotFound):
			// Already pasted (the session has it, and its answer is refused:
			// the route is closed), or deleted once finished.
		default:
			s.logger.Printf("routes: %s: cancel queued message %d: %v (its route gate refuses it)", r.RouteID, *r.SendQueueID, err)
		}
	}
	what := ownerAnsweredNote
	if st.From == plugins.RouteTimedOut && !st.HandBackWithdrawn {
		what = ownerAnsweredAfterHandBack
	}
	s.noteToChief(ctx, r, what)
}
