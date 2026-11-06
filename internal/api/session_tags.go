package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessiontags"
	"github.com/ZacxDev/muster/internal/ui"
)

// registerSessionTagRoutes wires the session tag editor (internal/sessiontags,
// ui/session_tags.go): one subject — kind=agent&agent_id=N, or
// kind=mirror&host=&session= (a tmux SESSION; a `window` field is the page's
// own window, used only to build the answer's switcher rows).
//
//   - GET  ui.SessionTagsPath        the subject's tags, as {"tags": […]}.
//   - POST ui.SessionTagsPath        add `tag` (normalized; idempotent).
//   - POST ui.SessionTagsRemovePath  remove `tag` (idempotent).
//
// 🔴 THE PINS ROUTES' CONVENTIONS, EXACTLY (pins.go): browser-session routes
// (requireSession — so the machine listener answers them 403, like every other
// browser route), and the writes CSRF-checked (requireOperatorWrite =
// requireSession + requireSameOrigin). No step-up: a tag, like a pin, is the
// viewer's own private label and sends nothing anywhere.
//
// A write posted by htmx is answered with the editor's body, the header's
// chips and the session switcher's rows (ui.RenderSessionTagsAnswer); a plain
// form post with a 303 back to the session's page (switcherPinBack: only ever
// a session page of this server). A refusal is a plain-text status the editor
// shows (400 for a bad subject or tag, 404 for an agent the viewer cannot see,
// 409 at the per-session cap).
func (s *Server) registerSessionTagRoutes(mux Mux) {
	// Spelled literally (ui.SessionTagsPath / ui.SessionTagsRemovePath are the
	// same strings): the route walks in this package's tests read the pattern
	// from the source.
	mux.HandleFunc("GET /sessions/tags", s.requireSession(s.handleSessionTagsList))
	mux.HandleFunc("POST /sessions/tags", s.requireOperatorWrite(s.handleSessionTagAdd))
	mux.HandleFunc("POST /sessions/tags/delete", s.requireOperatorWrite(s.handleSessionTagRemove))
}

// sessionTagTarget is the subject a request names, the page's own window, or a
// 400's reason.
func sessionTagTarget(get func(string) string) (sessiontags.Target, string, error) {
	switch get("kind") {
	case "agent":
		id, err := strconv.ParseInt(get("agent_id"), 10, 64)
		if err != nil || id <= 0 {
			return sessiontags.Target{}, "", errors.New("bad agent_id")
		}
		return sessiontags.Target{AgentID: id}, "", nil
	case "mirror":
		t := sessiontags.Target{Host: get("host"), Session: get("session")}
		if err := t.Valid(); err != nil {
			return t, "", err
		}
		w := get("window")
		if w != "" {
			if err := mirror.ValidWindowID(w); err != nil {
				return t, "", err
			}
		}
		return t, w, nil
	}
	return sessiontags.Target{}, "", errors.New("kind must be agent or mirror")
}

func (s *Server) handleSessionTagsList(w http.ResponseWriter, r *http.Request) {
	if s.ext.SessionTags == nil {
		http.NotFound(w, r)
		return
	}
	t, _, err := sessionTagTarget(r.URL.Query().Get)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tags, err := s.ext.SessionTags.For(r.Context(), t)
	if err != nil {
		s.logger.Printf("session tags: list: %v", err)
		http.Error(w, "could not read the tags", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string][]string{"tags": tags})
}

func (s *Server) handleSessionTagAdd(w http.ResponseWriter, r *http.Request) {
	s.handleSessionTagWrite(w, r, true)
}

func (s *Server) handleSessionTagRemove(w http.ResponseWriter, r *http.Request) {
	s.handleSessionTagWrite(w, r, false)
}

func (s *Server) handleSessionTagWrite(w http.ResponseWriter, r *http.Request, add bool) {
	if s.ext.SessionTags == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	t, window, err := sessionTagTarget(r.PostFormValue)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var tags []string
	if add {
		tags, err = s.ext.SessionTags.Add(ctx, t, r.PostFormValue("tag"))
	} else {
		tags, err = s.ext.SessionTags.Remove(ctx, t, r.PostFormValue("tag"))
	}
	switch {
	case errors.Is(err, sessiontags.ErrEmpty):
		http.Error(w, "Type a tag first.", http.StatusBadRequest)
		return
	case errors.Is(err, sessiontags.ErrInvalid), errors.Is(err, sessiontags.ErrBadTarget):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, sessiontags.ErrNoSuchAgent):
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	case errors.Is(err, sessiontags.ErrTooMany):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.logger.Printf("session tags: %v", err)
		http.Error(w, "could not save the tag", http.StatusInternalServerError)
		return
	}
	back := switcherPinBack(r.PostFormValue("back"))
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	target := ui.SessionTagTarget{AgentID: t.AgentID, Host: t.Host, Session: t.Session, Window: window}
	cur := ui.SwitcherCurrent{AgentID: t.AgentID}
	if t.Mirror() {
		cur = ui.SwitcherCurrent{Mirror: ui.MirrorRef{Host: t.Host, Session: t.Session, Window: window}}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderSessionTagsAnswer(w, target, tags, back, s.switcher(ctx, cur)); err != nil {
		s.logger.Printf("session tags: render the answer: %v", err)
	}
}

// sessionTagSet is the viewer's tags, read once per render. No store, or a
// failed read (logged), is "no tags": the entries still render.
func (s *Server) sessionTagSet(ctx context.Context) sessiontags.Set {
	if s.ext.SessionTags == nil {
		return sessiontags.Set{}
	}
	set, err := s.ext.SessionTags.List(ctx)
	if err != nil {
		s.logger.Printf("session tags: list: %v", err)
		return sessiontags.Set{}
	}
	return set
}

// withSessionTags marks each entry with the viewer's tags on it — an agent by
// id, a window by its tmux session (every window of a session shares them):
// the switcher's words (the palette and the dock's filter) and the ⋯ menu's
// editor read them.
func (s *Server) withSessionTags(ctx context.Context, cards []ui.AgentCardView, mirrors []ui.MirrorCardView) {
	if s.ext.SessionTags == nil {
		return
	}
	set := s.sessionTagSet(ctx)
	for i := range cards {
		cards[i].Tags = set.Agent(cards[i].ID)
	}
	for i := range mirrors {
		mirrors[i].Tags = set.Mirror(mirrors[i].Host, mirrors[i].Session)
	}
}

// sessionTagsFor is one subject's tags for a page's header; nil with no store
// or on a failed read (logged).
func (s *Server) sessionTagsFor(ctx context.Context, t sessiontags.Target) []string {
	if s.ext.SessionTags == nil {
		return nil
	}
	tags, err := s.ext.SessionTags.For(ctx, t)
	if err != nil {
		s.logger.Printf("session tags: read: %v", err)
		return nil
	}
	return tags
}
