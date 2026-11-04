package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// ---------------------------------------------------------------------------
// THE OWNER OPENING A SESSION ENDS ITS EPISODE (operator decision: opening the
// session in muster — the web UI or the app, which shows the same pages —
// clears its notification, beside an own message or an answered prompt;
// docs/design/session-state.md, "Episodes"). end_reason 'viewed'
// (sessionstate.EndViewed).
//
// 🔴 ONLY THE PAGES THAT RENDER ONE SESSION, AND ONLY THEIR DOCUMENT: a
// mirrored window's rendered view and its terminal page, and an agent's page
// for the conversation it draws — the places that record a "recently viewed"
// visit (recents.go), once the request has resolved to the page actually
// rendered (never on a redirect or a 404). Not the bar, the Agents list, the
// Asks page or its Stalled / waiting rows, the tap route itself (it only
// redirects — to that same page, which then counts), the view's socket, its
// fragments or any poll: none of them is the owner opening THAT session. A
// prefetch, or a HEAD, is not an open either (viewRequest).
//
// 🔴 OWNER-SCOPED TWICE: the episode is read and ended under the request's
// principal (session_episodes' row-level security), and an episode that is
// not the requester's own is left alone with that rolled back too — another
// user who can open the page ends nothing of the owner's.
//
// The push is withdrawn here, at once, when it was shown; a push claimed by a
// pass between the read and the end is withdrawn by that owner's next pass
// (clearEndedEpisodes, the restart rule). A stop ended here is not opened
// again while its session sits on the same streak (sessionstate.Handled).
// Viewing in a tmux terminal outside muster cannot be seen and ends nothing.
// ---------------------------------------------------------------------------

// viewRequest reports whether r is the owner opening a page — a GET the
// browser made to show it — rather than a prefetch or a HEAD.
func viewRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	for _, h := range []string{"Sec-Purpose", "Purpose", "X-Purpose", "X-Moz"} {
		if strings.Contains(strings.ToLower(r.Header.Get(h)), "prefetch") {
			return false
		}
	}
	return true
}

// episodeViewedMirror ends the requester's open episode for mirrored window e
// on host h, whose page r rendered.
func (s *Server) episodeViewedMirror(ctx context.Context, r *http.Request, h mirror.HostView, e mirror.SessionView) {
	if !s.sessionStateOn() || !viewRequest(r) {
		return
	}
	for _, w := range h.Windows {
		if w.Session == e.Session && w.ID == e.Window {
			// The key the stall facts list the window under (stateWindowKey,
			// one rule with the pass and the waiting hook).
			if key, _, _ := stateWindowKey(w, sessionStateListsHermes); key != "" {
				s.episodeViewed(ctx, sessionstate.KindMirror, key, func() string {
					// The window's streak now, as the pass keys it (windowStreakKey).
					return windowStreakKey(s.ext.Mirror.AgentState(ctx, h.Host, w.Session, w.ID))
				})
			}
			return
		}
	}
}

// episodeViewedAgent ends the requester's open episode for agent agentID's
// conversation chatSessionID, whose page r rendered.
func (s *Server) episodeViewedAgent(ctx context.Context, r *http.Request, agentID, chatSessionID int64) {
	if !s.sessionStateOn() || !viewRequest(r) || agentID <= 0 || chatSessionID <= 0 {
		return
	}
	s.episodeViewed(ctx, sessionstate.KindAgent, chiefnudge.AgentKey(agentID, chatSessionID), func() string {
		// The conversation's streak now, as the pass keys it (stallFacts): its
		// newest message.
		if s.ext.Agents == nil {
			return ""
		}
		last, err := s.ext.Agents.ListRecentChatMessages(ctx, chatSessionID, 1)
		if err != nil || len(last) == 0 {
			return ""
		}
		return "msg:" + strconv.FormatInt(last[len(last)-1].ID, 10)
	})
}

// episodeViewed ends ctx's owner's open episode for session (kind, key)
// 'viewed' and withdraws its push. Best-effort: a failure is logged and never
// fails the page. streak reads the session's streak NOW — the stop the owner
// is being shown — which the episode ends on (PG.EndViewed), so the next pass
// does not open and push it as a new stop when the stored streak lags. ""
// (unreadable) keeps the stored one: at worst that one extra push.
func (s *Server) episodeViewed(ctx context.Context, kind, key string, streak func() string) {
	owner := requestOwner(ctx)
	if owner <= 0 {
		return
	}
	ep, ok, err := s.ext.Episodes.OpenFor(ctx, kind, key)
	if err != nil {
		s.logger.Printf("session state: viewed %s: %v", kind, err)
		return
	}
	if !ok || ep.Owner != owner {
		return
	}
	now := s.now()
	ended, err := s.ext.Episodes.EndViewed(ctx, ep.ID, streak(), now)
	if err != nil {
		s.logger.Printf("session state: episode %d: viewed: %v", ep.ID, err)
		return
	}
	if !ended {
		return // a pass ended it this instant; that pass clears it
	}
	s.ownerChecked.settle(ep.ID)
	if ep.PushedAt.IsZero() {
		s.attentionChanged(owner, episodeKey(ep.ID)) // its quiet row goes
		return
	}
	ep.EndedAt, ep.EndReason = now, sessionstate.EndViewed
	s.clearEpisode(ctx, ep)
}
