package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/ui"
)

// registerPinRoutes wires the Agents list's pin toggles (internal/pins,
// migration 0017): POST ui.PinPath and ui.UnpinPath, form-encoded, naming one
// subject — kind=agent&agent_id=N, or kind=mirror&host=&session=&window= (the
// window "" for a per-session host's entry).
//
// 🔴 BROWSER-SESSION ROUTES, CSRF-CHECKED: requireOperatorWrite is
// requireSession (so on the machine listener they answer 403 like every other
// browser route — refuseOffBrowserListener) plus requireSameOrigin. Both are
// idempotent: pinning a pinned entry, or unpinning an unpinned one, succeeds.
//
// The one toggle that posts here is the session switcher's (view=switcher,
// answerSwitcherPin): the Agents list page and its cards' toggles are retired.
// Any other post is answered without a body — an empty 200 with agents:changed
// for htmx, a 303 home for a plain form — so the route behaves the same for a
// caller that names no view (TestThePinRoutesPinAndUnpinIdempotently).
func (s *Server) registerPinRoutes(mux Mux) {
	// Spelled literally (ui.PinPath / ui.UnpinPath are the same strings): the
	// route walks in this package's tests read the pattern from the source.
	mux.HandleFunc("POST /agents/pins", s.requireOperatorWrite(s.handlePin))
	mux.HandleFunc("POST /agents/pins/delete", s.requireOperatorWrite(s.handleUnpin))
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request)   { s.handlePinToggle(w, r, true) }
func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) { s.handlePinToggle(w, r, false) }

func (s *Server) handlePinToggle(w http.ResponseWriter, r *http.Request, pin bool) {
	if s.ext.Pins == nil || s.ext.Agents == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var err error
	// The subject as the session switcher's pin toggle posts it back
	// (ui.SwitcherPinToggle), and as the page it names (ui.SwitcherCurrent).
	var fields [][2]string
	var cur ui.SwitcherCurrent
	switch r.PostFormValue("kind") {
	case "agent":
		id, perr := strconv.ParseInt(r.PostFormValue("agent_id"), 10, 64)
		if perr != nil || id <= 0 {
			http.Error(w, "bad agent_id", http.StatusBadRequest)
			return
		}
		if pin {
			err = s.ext.Pins.PinAgent(ctx, id)
		} else {
			err = s.ext.Pins.UnpinAgent(ctx, id)
		}
		fields = [][2]string{{"kind", "agent"}, {"agent_id", strconv.FormatInt(id, 10)}}
		cur = ui.SwitcherCurrent{AgentID: id}
	case "mirror":
		k := pins.MirrorKey{Host: r.PostFormValue("host"), Session: r.PostFormValue("session"), Window: r.PostFormValue("window")}
		if verr := k.Valid(); verr != nil {
			http.Error(w, verr.Error(), http.StatusBadRequest)
			return
		}
		if pin {
			err = s.ext.Pins.PinMirror(ctx, k)
		} else {
			err = s.ext.Pins.UnpinMirror(ctx, k)
		}
		fields = [][2]string{{"kind", "mirror"}, {"host", k.Host}, {"session", k.Session}, {"window", k.Window}}
		cur = ui.SwitcherCurrent{Mirror: ui.MirrorRef{Host: k.Host, Session: k.Session, Window: k.Window}}
	default:
		http.Error(w, "kind must be agent or mirror", http.StatusBadRequest)
		return
	}
	switch {
	case errors.Is(err, pins.ErrNoSuchAgent):
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	case errors.Is(err, pins.ErrTooMany):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.logger.Printf("pins: %v", err)
		http.Error(w, "could not save the pin", http.StatusInternalServerError)
		return
	}
	if r.PostFormValue("view") == ui.SwitcherPinView {
		s.answerSwitcherPin(w, r, pin, fields, cur)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, ui.HomePath, http.StatusSeeOther)
		return
	}
	w.Header().Set("HX-Trigger", "agents:changed")
	w.WriteHeader(http.StatusOK)
}

// answerSwitcherPin answers a post from the session switcher's pin toggle
// (ui.SwitcherPinToggle, on a session's page): for htmx, the toggle in its new
// state and the bar's rows and counts out of band, built for the page of cur
// as the page itself built them (s.switcher) — so the Pins view and its count
// follow at once; without script, a 303 back to the session's page.
//
// 🔴 THE "back" FIELD IS ONLY EVER A SESSION PAGE OF THIS SERVER: a path under
// /agents/ or /mirror/ (switcherPinBack). Anything else — another origin, a
// scheme-relative "//host", an empty field — goes home, so the field cannot be
// used as an open redirect.
func (s *Server) answerSwitcherPin(w http.ResponseWriter, r *http.Request, pin bool, fields [][2]string, cur ui.SwitcherCurrent) {
	back := switcherPinBack(r.PostFormValue("back"))
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderSwitcherPin(w, pin, fields, back, s.switcher(r.Context(), cur)); err != nil {
		s.logger.Printf("pins: render the switcher's toggle: %v", err)
	}
}

// switcherPinBack is v when it is a path to a session's page on this server
// (/agents/<name>…, /mirror/…), else home (ui.HomePath).
func switcherPinBack(v string) string {
	if (strings.HasPrefix(v, "/agents/") || strings.HasPrefix(v, "/mirror/")) && !strings.HasPrefix(v, "//") && !strings.ContainsAny(v, "\\\r\n") {
		return v
	}
	return ui.HomePath
}

// pinSet is the viewing principal's pins, read once per render. No store, or a
// failed read (logged), is "nothing pinned": the list still renders, in its
// unpinned order.
func (s *Server) pinSet(ctx context.Context) pins.Set {
	if s.ext.Pins == nil {
		return pins.Set{}
	}
	set, err := s.ext.Pins.List(ctx)
	if err != nil {
		s.logger.Printf("pins: list: %v", err)
		return pins.Set{}
	}
	return set
}
