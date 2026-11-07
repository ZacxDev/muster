package api

import (
	"errors"
	"net/http"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionname"
)

// registerSessionNameRoutes wires the rename of a MIRRORED session
// (internal/sessionname, ui/session_name.go): POST /sessions/name with
// host=&session=&window= — the window whose conversation is renamed — and
// name ("" hands the session back to its automatic name).
//
// A dispatched agent is renamed at its own POST /agents/{id}/name
// (agents.Store.SetDisplayName), which makes its display name manual.
//
// 🔴 THE NAME IS THE CONVERSATION'S, NOT THE WINDOW'S. It is stored on the
// agent_focus row keyed by the conversation the window shows NOW
// (mirror.SessionView.SessionKey) — the key its focus and auto name have — so
// it follows the conversation across tmux windows and a host restart, and a
// window that later holds another conversation is not called by it. A window
// with no conversation muster can see (a shell, an older muster-mirror) has
// nothing to name: 409.
//
// The pins' conventions (pins.go): a browser-session route, CSRF-checked
// (requireOperatorWrite), no step-up — a name is the viewer's own label and
// sends nothing anywhere. Posted by htmx, the answer reloads the page
// (HX-Refresh), so every surface reads the new name; a plain form post is
// answered with a 303 back to the session's page (switcherPinBack).
func (s *Server) registerSessionNameRoutes(mux Mux) {
	// Spelled literally (ui.SessionNamePath is the same string): the route
	// walks in this package's tests read the pattern from the source.
	mux.HandleFunc("POST /sessions/name", s.requireOperatorWrite(s.handleSessionName))
}

// mirrorNameUnchanged reports whether typed, cleaned, is exactly the
// automatic name e is called by now (a Save of the prefilled dialog).
func (s *Server) mirrorNameUnchanged(r *http.Request, e mirror.SessionView, typed string) bool {
	n := s.mirrorName(r.Context(), e)
	clean, err := focus.CleanName(typed)
	return err == nil && n.Source != sessionname.SourceManual && n.Human() && clean == n.Text
}

func (s *Server) handleSessionName(w http.ResponseWriter, r *http.Request) {
	if s.ext.Focus == nil || s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	window := r.PostFormValue("window")
	if window != "" && mirror.ValidWindowID(window) != nil {
		http.Error(w, "bad window", http.StatusBadRequest)
		return
	}
	_, e, listed, _, ok := s.mirrorEntry(r, r.PostFormValue("host"), r.PostFormValue("session"), window)
	if !ok || !listed {
		http.NotFound(w, r)
		return
	}
	key := e.SessionKey()
	if key == "" {
		http.Error(w, "This window has no conversation muster can name yet.", http.StatusConflict)
		return
	}
	// 🔴 SAVING THE NAME IT ALREADY HAS CHANGES NOTHING. The dialog is
	// prefilled with what the session is called now; a Save that kept an
	// automatic name must not turn it into a manual one (which would stop
	// it ever being handed back, and every later rule treating it as the
	// person's). Only a name that differs from the shown one is stored.
	var err error
	if name := r.PostFormValue("name"); !s.mirrorNameUnchanged(r, e, name) {
		_, err = s.ext.Focus.SetManualName(r.Context(), focus.MirrorSubject(key), name)
	}
	switch {
	case errors.Is(err, focus.ErrBadName):
		http.Error(w, "A name is 1 to 60 characters.", http.StatusBadRequest)
		return
	case err != nil:
		s.logger.Printf("session names: %v", err)
		http.Error(w, "could not save the name", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, switcherPinBack(r.PostFormValue("back")), http.StatusSeeOther)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.Header().Set("HX-Trigger", "agents:changed")
	w.WriteHeader(http.StatusOK)
}
