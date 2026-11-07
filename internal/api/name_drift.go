package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionname"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// DRIFT: A RENAME OFFERED, NEVER MADE (internal/focus drift.go;
// docs/focus.md#when-the-work-moves-on).
//
// A session's page offers "Rename to '<current focus>'" when its focus has
// stopped being about its name for long enough (focus.DriftOffer). Accepting
// is an ordinary rename by the person — POST /agents/{id}/name or
// POST /sessions/name with that title — so the name it gives is MANUAL.
// "Keep the name" records a dismissal at that focus title (POST
// /sessions/name/drift-dismiss), which holds until the title changes.
//
// 🔴 EVERY STABLE NAME DRIFTS BUT A ROLE'S. A manual, task (dispatch) or auto
// name can be offered a rename — a task's name is the agent's stable name and
// never changes on its own (operator, #504 Q1), so the offer is how it moves
// on. A system name (the chief, an officer) never is — it is how muster finds
// the role — nor a technical fallback, which is no name to drift from.
// ---------------------------------------------------------------------------

// driftOffer is the rename sub's page offers, "" for none: n is the name the
// page shows (resolved by the page's own adapter call).
func (s *Server) driftOffer(r *http.Request, sub focus.Subject, n sessionname.Name) string {
	if s.ext.Focus == nil || !driftable(n.Source) {
		return ""
	}
	ctx := r.Context()
	page, err := s.ext.Focus.History(ctx, sub, "", focus.DriftMinTitles)
	if err != nil {
		s.logger.Printf("names: drift of %s: %v", sub, err)
		return ""
	}
	return focus.DriftOffer(n.Text, page.Entries, s.storedName(ctx, sub).DriftDismissed, s.now())
}

// driftable reports whether a name of source src may be offered a rename on
// drift: manual, dispatch and auto names; never a system name or a fallback.
func driftable(src sessionname.Source) bool {
	switch src {
	case sessionname.SourceManual, sessionname.SourceDispatch, sessionname.SourceAuto:
		return true
	}
	return false
}

// agentDrift is the offer banner for agent id's page, nil for no offer.
func (s *Server) agentDrift(r *http.Request, id int64, n sessionname.Name) *ui.SessionDrift {
	offer := s.driftOffer(r, focus.AgentSubject(id), n)
	if offer == "" {
		return nil
	}
	ids := strconv.FormatInt(id, 10)
	return &ui.SessionDrift{Offer: offer, RenameAction: "/agents/" + ids + "/name",
		Fields: [][2]string{{"kind", "agent"}, {"agent_id", ids}}}
}

// mirrorDrift is the offer banner for window e's Rendered view, nil for none.
func (s *Server) mirrorDrift(r *http.Request, e mirror.SessionView, n sessionname.Name) *ui.SessionDrift {
	key := e.SessionKey()
	if key == "" {
		return nil
	}
	offer := s.driftOffer(r, focus.MirrorSubject(key), n)
	if offer == "" {
		return nil
	}
	return &ui.SessionDrift{Offer: offer, RenameAction: ui.SessionNamePath,
		Fields: [][2]string{{"kind", "mirror"}, {"host", e.Host}, {"session", e.Session}, {"window", e.Window}},
		Back:   mirrorRef(e).ViewPath()}
}

// registerSessionDriftRoutes wires "Keep the name": POST
// /sessions/name/drift-dismiss with the session (kind=agent&agent_id=, or
// kind=mirror&host=&session=&window=) and title, the offer dismissed. The
// pins' conventions (session_names.go).
func (s *Server) registerSessionDriftRoutes(mux Mux) {
	mux.HandleFunc("POST /sessions/name/drift-dismiss", s.requireOperatorWrite(s.handleDriftDismiss))
}

func (s *Server) handleDriftDismiss(w http.ResponseWriter, r *http.Request) {
	if s.ext.Focus == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var sub focus.Subject
	switch r.PostFormValue("kind") {
	case "agent":
		id, err := strconv.ParseInt(r.PostFormValue("agent_id"), 10, 64)
		if err != nil || id <= 0 || s.ext.Agents == nil {
			http.Error(w, "bad agent_id", http.StatusBadRequest)
			return
		}
		if _, err := s.ext.Agents.Get(r.Context(), id); err != nil {
			http.NotFound(w, r)
			return
		}
		sub = focus.AgentSubject(id)
	case "mirror":
		window := r.PostFormValue("window")
		if window != "" && mirror.ValidWindowID(window) != nil {
			http.Error(w, "bad window", http.StatusBadRequest)
			return
		}
		_, e, listed, _, ok := s.mirrorEntry(r, r.PostFormValue("host"), r.PostFormValue("session"), window)
		if !ok || !listed || e.SessionKey() == "" {
			http.NotFound(w, r)
			return
		}
		sub = focus.MirrorSubject(e.SessionKey())
	default:
		http.Error(w, "kind must be agent or mirror", http.StatusBadRequest)
		return
	}
	if err := s.ext.Focus.DismissDrift(r.Context(), sub, r.PostFormValue("title")); err != nil {
		if errors.Is(err, focus.ErrBadName) {
			http.Error(w, "bad title", http.StatusBadRequest)
			return
		}
		s.logger.Printf("names: dismiss drift of %s: %v", sub, err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, switcherPinBack(r.PostFormValue("back")), http.StatusSeeOther)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}
