package api

import (
	"github.com/ZacxDev/muster/internal/agents"

	"time"

	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf16"

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
// "Keep" records a dismissal at that focus title (POST
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

// registerSessionDriftRoutes wires "Keep": POST
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
	w.Header().Set("Cache-Control", "no-store")
	// Focus to the page's title, not <body> where the deleted banner leaves
	// it (the offer is on an agent's page or a Rendered view only).
	focusOn := ui.MirrorHeadingID
	if p := renamePageOf(r); p == renameOnAgentPage || (p == renameOnUnknown && sub.Kind != focus.KindMirror) {
		focusOn = ui.AgentTitleID
	}
	w.Header().Set("HX-Trigger-After-Settle", renameEvent("", focusOn))
	if err := ui.RenderDriftKept(w); err != nil {
		s.logger.Printf("names: render the kept offer: %v", err)
	}
}

// renamePage is which session page posted an htmx rename — read from
// HX-Current-URL, the address of the document that posted (htmx sends it on
// every request; a view framed by its other half sends ITS address, so its
// own sinks are the ones swapped). Each page names the session in its own
// places and titles its tab its own way, so the in-place answer is the posting
// page's (ui.Renamed). An address that is no session page (or none) is
// answered as the subject's own page would be.
type renamePage int

const (
	renameOnUnknown renamePage = iota
	renameOnAgentPage
	renameOnAgentTerminal
	renameOnMirrorView
	renameOnMirrorTerminal
)

func renamePageOf(r *http.Request) renamePage {
	u, err := url.Parse(r.Header.Get("HX-Current-URL"))
	if err != nil {
		return renameOnUnknown
	}
	pathOf := func(p string) string { return strings.SplitN(p, "?", 2)[0] }
	switch p := u.Path; {
	case p == pathOf(ui.MirrorRef{}.ViewPath()):
		return renameOnMirrorView
	case p == pathOf(ui.MirrorRef{}.TerminalPath()):
		return renameOnMirrorTerminal
	case strings.HasPrefix(p, "/agents/") && strings.HasSuffix(p, "/terminal") && strings.Count(p, "/") == 3:
		return renameOnAgentTerminal
	case strings.HasPrefix(p, "/agents/") && strings.Count(p, "/") == 2:
		return renameOnAgentPage
	}
	return renameOnUnknown
}

// renameEvent is the HX-Trigger-After-Settle value an in-place answer carries:
// ui.RenameEvent with the page's new title ("" leaves it) and the element to
// focus. 🔴 ASCII ONLY: a header is read by the browser as bytes, one
// character each, so a UTF-8 name would arrive as mojibake — every non-ASCII
// rune is a \u escape (JSON's own), and the title reaches document.title
// exactly as the page draws it.
func renameEvent(title, focus string) string {
	b, _ := json.Marshal(map[string]map[string]string{ui.RenameEvent: {"title": title, "focus": focus}})
	var out strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r > 0xFFFF:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&out, `\u%04x\u%04x`, r1, r2)
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.String()
}

// answerRenamed writes a rename's in-place answer: rn's out-of-band swaps,
// agents:changed (the sidebar's refresh) and, once they have settled, the
// title and focus (renameEvent).
func (s *Server) answerRenamed(w http.ResponseWriter, rn ui.Renamed) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("HX-Trigger", "agents:changed")
	w.Header().Set("HX-Trigger-After-Settle", renameEvent(rn.Title, rn.Focus))
	if err := rn.Body.Render(w); err != nil {
		s.logger.Printf("names: render a rename's answer: %v", err)
	}
}

// answerAgentRenamed answers an htmx rename of agent a (a as it is NOW) on
// the page that posted it: its Terminal page, else its own page.
func (s *Server) answerAgentRenamed(w http.ResponseWriter, r *http.Request, a agents.Agent) {
	ctx := r.Context()
	n := s.agentName(ctx, a)
	if renamePageOf(r) == renameOnAgentTerminal {
		s.answerRenamed(w, ui.TerminalRenamed(ui.AgentTerminalView{Name: a.Name, DisplayName: a.DisplayName, SessionName: n,
			Switcher: s.switcher(ctx, ui.SwitcherCurrent{AgentID: a.ID, Back: ui.AgentTerminalPath(a.Name)})}))
		return
	}
	noteID, _ := s.agentTaskStatus(ctx, a)
	s.answerRenamed(w, ui.AgentPageRenamed(ui.AgentDetailView{ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, SessionName: n, NoteID: noteID,
		Card:     ui.AgentCardView{ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, SessionName: n},
		Switcher: s.switcher(ctx, ui.SwitcherCurrent{AgentID: a.ID})}))
}

// answerMirrorRenamed answers an htmx rename of window e (host h) on the page
// that posted it: its Terminal page, else its Rendered view — each view's
// name fields set as that page's own handler sets them.
func (s *Server) answerMirrorRenamed(w http.ResponseWriter, r *http.Request, h mirror.HostView, e mirror.SessionView) {
	ctx := r.Context()
	ref := ui.MirrorRef{Host: h.Host, Session: e.Session, Window: e.Window}
	n := s.mirrorName(ctx, e)
	icon := s.mirrorHostIcon(ctx, h.Host)
	if renamePageOf(r) == renameOnMirrorTerminal {
		rawName := ""
		if e.Label != "" && e.Label != e.Session {
			rawName = e.Session
		}
		s.answerRenamed(w, ui.TerminalRenamed(ui.AgentTerminalView{Name: e.Session, SessionName: n, DisplayName: e.Title(),
			TitleQualifier: e.Qualifier, RawName: rawName, Badge: "Mirrored · " + h.Host, BadgeHost: h.Host, BadgeHostIcon: icon,
			Switcher: s.switcher(ctx, ui.SwitcherCurrent{Mirror: ref, Back: mirrorRef(e).TerminalPath()})}))
		return
	}
	s.answerRenamed(w, ui.MirrorViewRenamed(ui.MirrorViewView{Host: h.Host, Session: e.Session, Window: e.Window, Title: mirrorTitle(e),
		Label: e.Label, Qualifier: e.Qualifier, SessionName: n, HostIcon: icon, Switcher: s.switcher(ctx, ui.SwitcherCurrent{Mirror: ref})}))
}
