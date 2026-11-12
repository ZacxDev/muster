package api

import (
	"github.com/ZacxDev/muster/internal/agents"

	"time"

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
// page shows (resolved by the page's own adapter call); displayNameAt is when
// an agent's display name was set (agents.Agent.DisplayNameAt), zero for a
// mirrored session.
//
// 🔴 ONLY FOCUS CHANGES AFTER THE NAME TOOK EFFECT COUNT (round 1 of #508):
// the moment is the name's own — a manual mirrored name's name_manual_at, an
// auto name's name_auto_at, an agent's manual or task name's display_name_at
// — so a session renamed a minute ago is not offered a rename on the history
// it was renamed over.
func (s *Server) driftOffer(r *http.Request, sub focus.Subject, n sessionname.Name, displayNameAt time.Time) string {
	if s.ext.Focus == nil || !driftable(n.Source) {
		return ""
	}
	ctx := r.Context()
	page, err := s.ext.Focus.History(ctx, sub, "", focus.DriftMinTitles)
	if err != nil {
		s.logger.Printf("names: drift of %s: %v", sub, err)
		return ""
	}
	stored := s.storedName(ctx, sub)
	var since time.Time
	switch {
	case n.Source == sessionname.SourceAuto:
		since = stored.AutoAt
	case sub.Kind == focus.KindMirror:
		since = stored.ManualAt
	default:
		since = displayNameAt
	}
	return focus.DriftOffer(n.Text, page.Entries, stored.DriftDismissed, since, s.now())
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
func (s *Server) agentDrift(r *http.Request, a agents.Agent, n sessionname.Name) *ui.SessionDrift {
	id := a.ID
	offer := s.driftOffer(r, focus.AgentSubject(id), n, a.DisplayNameAt)
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
	offer := s.driftOffer(r, focus.MirrorSubject(key), n, time.Time{})
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
	// IN PLACE, NOT A RELOAD (operator: a tap on the offer reloaded the whole
	// page — this answered HX-Refresh). The banner goes, out of band; nothing
	// else on the page changes, so nothing else is drawn.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderDriftKept(w); err != nil {
		s.logger.Printf("names: render the kept offer: %v", err)
	}
}

// driftAnswerInPlace reports whether a rename was posted by the drift offer's
// Rename with htmx (ui.SessionDriftParam, sent only by htmx): the rename
// routes then answer with the page's name redrawn out of band
// (answerAgentDriftRenamed, answerMirrorDriftRenamed) instead of a reload. A
// plain form post is still answered with its 303, and the rename dialog's Save
// still with HX-Refresh.
func driftAnswerInPlace(r *http.Request) bool {
	return r.Header.Get("HX-Request") != "" && r.PostFormValue(ui.SessionDriftParam) == "1"
}

// answerAgentDriftRenamed answers the offer's Rename on agent a's page (a as
// it is NOW, renamed): its title, its rename dialogs and the switcher, drawn
// by the helpers the page itself uses, with agents:changed for the sidebar.
func (s *Server) answerAgentDriftRenamed(w http.ResponseWriter, r *http.Request, a agents.Agent) {
	ctx := r.Context()
	n := s.agentName(ctx, a)
	noteID, _ := s.agentTaskStatus(ctx, a)
	v := ui.AgentDetailView{ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, SessionName: n, NoteID: noteID,
		Card:     ui.AgentCardView{ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, SessionName: n},
		Switcher: s.switcher(ctx, ui.SwitcherCurrent{AgentID: a.ID})}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Trigger", "agents:changed")
	if err := ui.RenderAgentDriftRenamed(w, v); err != nil {
		s.logger.Printf("names: render agent %d renamed: %v", a.ID, err)
	}
}

// answerMirrorDriftRenamed answers the offer's Rename on window e's Rendered
// view (host h): its header's name and full title, its <title> and the
// switcher, with agents:changed for the sidebar.
func (s *Server) answerMirrorDriftRenamed(w http.ResponseWriter, r *http.Request, h mirror.HostView, e mirror.SessionView) {
	ctx := r.Context()
	v := ui.MirrorViewView{Host: h.Host, Session: e.Session, Window: e.Window, Title: mirrorTitle(e), Label: e.Label,
		Qualifier: e.Qualifier, SessionName: s.mirrorName(ctx, e),
		Switcher: s.switcher(ctx, ui.SwitcherCurrent{Mirror: ui.MirrorRef{Host: h.Host, Session: e.Session, Window: e.Window}})}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Trigger", "agents:changed")
	if err := ui.RenderMirrorDriftRenamed(w, v); err != nil {
		s.logger.Printf("names: render %s renamed: %v", e.SessionKey(), err)
	}
}
