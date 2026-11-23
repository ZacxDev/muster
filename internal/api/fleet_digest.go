package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/redact"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE FLEET DIGEST CARD's routes (internal/ui fleet_digest.go; docs/chief.md
// "Fleet digest"):
//
//	GET  /ui/chief/fleet            the card (?look=1: the panel was opened on it;
//	                                ?fleet=proposed[&proposal=<id>]: a fleet link's
//	                                arrival, fleetFocus)
//	GET  /ui/chief/fleet/session    one session's drill-in (?id=<fleet id>)
//	POST /ui/chief/fleet/chat       the Fleet side chat, with a draft (about=)
//
// 🔴 MUSTER'S OWN RENDERING, NO CHIEF TURN. The card is the fleet reader
// (fleet_read.go) grouped by internal/fleet; nothing here calls a model or
// queues a message. The Fleet chat route OPENS a side chat and fills its box:
// the owner presses Send, or nothing is sent.
//
// 🔴 OFF UNLESS MUSTER_FLEET_DIGEST=1: every route answers 404, the shell
// draws no slot (ui.Features.FleetDigest), and fleet_list is as it was.
//
// 🔴 EVERY ROUTE IS THE OWNER'S OWN: the reader is the principal's own hosts
// and agents, and a drill-in names a session by its fleet id, answered only
// when the owner's own fleet holds it — another owner's id, a team's window,
// or the chief itself is "not in your fleet now", the same answer for each.
// ---------------------------------------------------------------------------

func (s *Server) registerFleetDigestRoutes(mux Mux) {
	mux.HandleFunc("GET "+ui.FleetDigestPath, s.requireSession(s.handleFleetDigest))
	mux.HandleFunc("GET "+ui.FleetDrillPath, s.requireSession(s.handleFleetDrill))
	mux.HandleFunc("POST "+ui.FleetChatPath, s.requireOperatorWrite(s.handleFleetChat))
}

// The card's honest answers when there is no digest to draw.
const (
	fleetDigestNoState = "The fleet's states come from muster's session-state pass, which is off on this server " +
		"(MUSTER_SESSION_STATE), so there is no digest."
	fleetDigestReadFailed = "Could not read the fleet just now — this is not an empty fleet. It is read again on the next change."
	fleetDrillGone        = "This session is not in your fleet now."
)

// fleetDigestFor is the digest as ctx's principal sees it, against baseline,
// and the subjects it was built from (by fleet id); unavailable is the card's
// sentence when there is none.
func (s *Server) fleetDigestFor(ctx context.Context, baseline time.Time) (d fleet.Digest, unavailable string) {
	d, unavailable, _ = s.fleetDigestRead(ctx, baseline)
	return d, unavailable
}

// fleetDigestRead is fleetDigestFor, and the subjects it was built from by
// fleet id (the proposal cards' Open session and Where).
func (s *Server) fleetDigestRead(ctx context.Context, baseline time.Time) (d fleet.Digest, unavailable string, subs map[string]fleetSubject) {
	if s.ext.Episodes == nil {
		return fleet.Digest{}, fleetDigestNoState, nil
	}
	fr, err := s.readFleet(ctx, fleetListFilter{})
	if err != nil || !fr.stateOn {
		if err != nil {
			s.logger.Printf("fleet digest: %v", err)
		}
		return fleet.Digest{}, fleetDigestReadFailed, nil
	}
	var sessions []fleet.Session
	subs = map[string]fleetSubject{}
	for _, sub := range fr.subjects {
		// The chief is not one of the sessions it orchestrates: its own open
		// cards are the Needs you asks.
		if sub.chief {
			continue
		}
		subs[sub.row.ID] = sub
		sessions = append(sessions, fleetSessionOf(sub))
	}
	var asks []fleet.Ask
	if chief, err := s.chiefAgent(ctx); err == nil {
		for _, a := range s.chiefOpenCards(ctx, chief) {
			asks = append(asks, fleet.Ask{ID: a.ID, Question: a.Question, CreatedAt: a.CreatedAt})
		}
	} else if !errors.Is(err, ErrNoChief) {
		s.logger.Printf("fleet digest: the chief: %v", err)
	}
	return fleet.Build(sessions, asks, baseline), "", subs
}

// fleetSessionOf is subject sub as the digest draws it.
func fleetSessionOf(sub fleetSubject) fleet.Session {
	return fleet.Session{ID: sub.row.ID, Kind: sub.kind, Name: firstNonBlank(sub.row.Name, sub.row.ID), Where: sub.row.Where,
		Href: fleetSessionHref(sub), State: sub.state, Episode: fleetEpisodeOf(sub.episode), PRs: sub.prs, InFlight: sub.inFlight}
}

// handleFleetDigest serves GET /ui/chief/fleet: the card. ?look=1 records a
// look (the panel opened on it) and draws "since" against the look before;
// a refresh draws against the stored baseline and moves nothing.
func (s *Server) handleFleetDigest(w http.ResponseWriter, r *http.Request) {
	if !s.ext.FleetDigest {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	now := s.now()
	var baseline time.Time
	if s.ext.Fleet != nil {
		var err error
		if r.URL.Query().Get("look") == "1" {
			baseline, err = s.ext.Fleet.Look(ctx, now)
		} else {
			baseline, err = s.ext.Fleet.Baseline(ctx)
		}
		if err != nil {
			s.logger.Printf("fleet digest: the look: %v", err)
		}
	}
	v := s.fleetDigestViewFor(ctx, baseline)
	// A fleet link's arrival (ui.FleetProposalsURL, passed on by the panel
	// script): the Proposed group open on the proposal it named.
	if q := r.URL.Query(); q.Get(ui.FleetFocusParam) == ui.FleetGroupProposed {
		id, _ := strconv.ParseInt(q.Get(ui.FleetProposalParam), 10, 64)
		s.fleetFocus(ctx, &v, max(id, 0))
	}
	if err := ui.RenderFleetDigest(w, v); err != nil {
		s.logger.Printf("fleet digest: render: %v", err)
	}
}

// fleetDigestView is d as the card draws it, at now.
func fleetDigestView(d fleet.Digest, now time.Time) ui.FleetDigestView {
	v := ui.FleetDigestView{Total: d.Total, Since: fleetSinceLine(d.Since, now)}
	for _, gr := range d.Groups {
		gv := ui.FleetGroupView{Key: gr.Key, Label: gr.Label, Note: fleet.GroupNote[gr.Key]}
		for _, a := range gr.Asks {
			gv.Asks = append(gv.Asks, ui.FleetAskView{ID: a.ID, Question: cutRunes(strings.Join(strings.Fields(a.Question), " "), 200),
				Age: fleetAge(now.Sub(a.CreatedAt)), Href: askPagePath(a.ID)})
		}
		for _, se := range gr.Sessions {
			since := ""
			if se.Episode != nil {
				since = fleetAge(now.Sub(se.Episode.Since))
			}
			pr := ""
			if len(se.PRs) > 0 {
				pr = se.PRs[0].Repo + "#" + strconv.FormatInt(se.PRs[0].Number, 10) + " " + se.PRs[0].State
			}
			gv.Rows = append(gv.Rows, ui.FleetRowView{ID: se.ID, Name: se.Name, State: se.State,
				Detail: ui.FleetDrillDetail(se.Where, se.State, since, pr)})
		}
		v.Groups = append(v.Groups, gv)
	}
	return v
}

// fleetSinceLine is the "since you last looked" line ("" with no baseline).
func fleetSinceLine(si fleet.Since, now time.Time) string {
	if si.Baseline.IsZero() {
		return ""
	}
	var parts []string
	add := func(n int, one, many string) {
		if n == 1 {
			parts = append(parts, "1 "+one)
		} else if n > 1 {
			parts = append(parts, strconv.Itoa(n)+" "+many)
		}
	}
	add(si.Finished, "finished", "finished")
	add(si.NewStops, "new stop", "new stops")
	add(si.NewQuestions, "new chief question", "new chief questions")
	head := "Since you last looked (" + fleetAge(now.Sub(si.Baseline)) + " ago): "
	if len(parts) == 0 {
		return head + "nothing new."
	}
	return head + strings.Join(parts, " · ")
}

// fleetAge is d as the card says it: "just now", "12m", "5h", "3d".
func fleetAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

// --- the drill-in ---------------------------------------------------------------

// fleetDrillStops bounds the drill-in's timeline; fleetDrillMessageBytes its
// last message (after session_context's redaction and cut).
const (
	fleetDrillStops        = 10
	fleetDrillMessageBytes = 600
)

// episodeRecentReader is the episode store's per-session history
// (sessionstate.PG.Recent); a store without it draws no timeline.
type episodeRecentReader interface {
	Recent(ctx context.Context, kind, key string, prefix bool, since time.Time, limit int) ([]sessionstate.Episode, error)
}

// handleFleetDrill serves GET /ui/chief/fleet/session?id=: one session of the
// owner's fleet, expanded — its stops in the last day, its pull requests, its
// linked tasks, its last message (redacted and cut as session_context shows
// it) and where Open session goes.
func (s *Server) handleFleetDrill(w http.ResponseWriter, r *http.Request) {
	if !s.ext.FleetDigest {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	write := func(v ui.FleetDrillView) {
		if err := ui.RenderFleetDrill(w, v); err != nil {
			s.logger.Printf("fleet drill-in: render: %v", err)
		}
	}
	sub, ok := s.fleetSubjectFor(ctx, r.URL.Query().Get("id"))
	if !ok {
		write(ui.FleetDrillView{Failed: fleetDrillGone})
		return
	}
	now := s.now()
	v := ui.FleetDrillView{ID: sub.row.ID, Name: sub.row.Name, Where: sub.row.Where, State: strings.ReplaceAll(sub.state, "_", " "),
		Href: fleetSessionHref(sub)}
	if rr, ok := s.ext.Episodes.(episodeRecentReader); ok {
		key, prefix := sub.key, false
		if sub.kind == chiefnudge.KindAgent {
			key, prefix = sub.key+":", true
		}
		eps, err := rr.Recent(ctx, sub.kind, key, prefix, now.Add(-24*time.Hour), fleetDrillStops)
		if err != nil {
			s.logger.Printf("fleet drill-in %s: stops: %v", sub.row.ID, err)
		}
		for _, ep := range eps {
			st := ui.FleetStopView{Opened: fleetAge(now.Sub(ep.OpenedAt)) + " ago", State: strings.ReplaceAll(ep.State, "_", " ")}
			if ep.Waiting {
				st.State = "waiting"
			}
			if !ep.EndedAt.IsZero() {
				st.Ended = "ended " + ep.EndReason
			}
			v.Timeline = append(v.Timeline, st)
		}
	}
	for _, p := range sub.prs {
		v.PRs = append(v.PRs, ui.FleetPRView{Label: p.Repo + "#" + strconv.FormatInt(p.Number, 10), State: p.State, Head: p.HeadSHA12})
	}
	if sub.kind == chiefnudge.KindMirror && s.ext.Notes != nil {
		by, err := s.ext.Notes.TasksForSessions(ctx, []string{sub.key}, fleetLinkedTasksMax, fleetLinkedRoles...)
		if err != nil {
			s.logger.Printf("fleet drill-in %s: linked tasks: %v", sub.row.ID, err)
		}
		for _, l := range by[sub.key] {
			v.Tasks = append(v.Tasks, ui.FleetTaskView{Title: l.Title, Status: l.Status, Role: l.Role})
		}
	}
	v.LastRole, v.LastMessage, v.LastNote = s.fleetLastMessage(ctx, sub)
	write(v)
}

// fleetSubjectFor is the subject of the owner's fleet whose fleet id is id;
// false for any other id (another owner's, a team's, the chief, or gone).
func (s *Server) fleetSubjectFor(ctx context.Context, id string) (fleetSubject, bool) {
	if strings.TrimSpace(id) == "" {
		return fleetSubject{}, false
	}
	fr, err := s.readFleet(ctx, fleetListFilter{})
	if err != nil {
		return fleetSubject{}, false
	}
	for _, sub := range fr.subjects {
		if sub.row.ID == id && !sub.chief {
			return sub, true
		}
	}
	return fleetSubject{}, false
}

// fleetLastMessage is sub's newest message: its role and its text, redacted
// by the one redactor and cut (session_context's tail rule, sessionContextTail),
// or a note saying why there is none.
func (s *Server) fleetLastMessage(ctx context.Context, sub fleetSubject) (role, text, note string) {
	if sub.kind == chiefnudge.KindMirror {
		entries, err := focusSessions{s}.Tail(ctx, focus.MirrorSubject(sub.key))
		if err != nil && len(entries) == 0 {
			return "", "", "Its transcript could not be read just now."
		}
		tail := sessionContextTail(entries, 1)
		if len(tail) == 0 {
			return "", "", "No message yet."
		}
		return tail[0].Role, cutBytes(tail[0].Text, fleetDrillMessageBytes), ""
	}
	if s.ext.Agents == nil {
		return "", "", "No message yet."
	}
	chat := int64(0)
	if sub.episode != nil {
		chat = sub.episode.ChatSessionID
	}
	if chat == 0 {
		sessions, err := s.ext.Agents.ListSessions(ctx, sub.agent.ID)
		if err != nil || len(sessions) == 0 {
			return "", "", "No message yet."
		}
		chat = sessions[0].ID
	}
	msgs, err := s.ext.Agents.ListRecentChatMessages(ctx, chat, 1)
	if err != nil {
		return "", "", "Its conversation could not be read just now."
	}
	if len(msgs) == 0 {
		return "", "", "No message yet."
	}
	m := msgs[len(msgs)-1]
	return m.Role, cutBytes(redact.Secrets(m.Content), fleetDrillMessageBytes), ""
}

// --- the Fleet side chat ----------------------------------------------------------

// fleetChatTitle is the Fleet side chat's name in the thread list.
const fleetChatTitle = "Fleet"

// sessionTitleSetter is the agent store's title write (agents.PGStore).
type sessionTitleSetter interface {
	SetSessionTitle(ctx context.Context, sessionID int64, title string) error
}

// handleFleetChat serves POST /ui/chief/fleet/chat: the owner's Fleet side
// chat — the one the digest's "Ask the chief about this" opens, never the
// primary — answered as the panel body on it, its box filled with a draft
// about the session `about` names (or the fleet). 🔴 NOTHING IS SENT: the
// draft is the box's, and the owner presses Send.
func (s *Server) handleFleetChat(w http.ResponseWriter, r *http.Request) {
	if !s.ext.FleetDigest {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if s.ext.Agents == nil {
		s.writeChiefPanelBody(ctx, w, "")
		return
	}
	a, err := s.chiefAgent(ctx)
	if err != nil {
		s.writeChiefPanelBody(ctx, w, "")
		return
	}
	sess, err := s.fleetChat(ctx, a)
	if err != nil {
		s.logger.Printf("fleet chat of %d: %v", a.ID, err)
		http.Error(w, "could not open the Fleet chat", http.StatusInternalServerError)
		return
	}
	s.writeChiefPanelBodyWith(ctx, w, a, strconv.FormatInt(sess.ID, 10), s.fleetDraft(ctx, r.FormValue("about")), false)
}

// fleetChat is chief a's Fleet side chat: the one recorded (fleet_owner_marks)
// while it is still a's and not archived, else a new side chat named
// fleetChatTitle, recorded. The primary is elected first, so the new chat can
// never be taken for it. 🔴 CREATE-OR-REUSE IS ONE STEP (fleet.Store
// FleetChatOr, a row lock on the owner's marks): two taps at once open one
// chat, not two with one orphaned.
func (s *Server) fleetChat(ctx context.Context, a agents.Agent) (agents.ChatSession, error) {
	if _, err := s.primarySession(ctx, a); err != nil {
		return agents.ChatSession{}, err
	}
	var kept agents.ChatSession
	keep := func(id int64) bool {
		cs, err := s.ext.Agents.GetSession(ctx, id)
		if err != nil || cs.AgentID != a.ID || cs.ArchivedAt != nil {
			return false
		}
		kept = cs
		return true
	}
	var made agents.ChatSession
	create := func() (int64, error) {
		var err error
		runtime := sideChatRuntime(a)
		if st, ok := s.sessionRuntimeStore(); ok && runtime != agents.SessionRuntimeLegacy {
			made, err = st.CreateSessionOn(ctx, a.ID, a.Name, runtime, "")
		} else {
			made, err = s.ext.Agents.CreateSession(ctx, a.ID, a.Name)
		}
		if err != nil {
			return 0, err
		}
		if ts, ok := s.ext.Agents.(sessionTitleSetter); ok {
			if err := ts.SetSessionTitle(ctx, made.ID, fleetChatTitle); err != nil {
				s.logger.Printf("fleet chat %d: title: %v", made.ID, err)
			} else {
				made.Title = fleetChatTitle
			}
		}
		s.logger.Printf("fleet chat: %s: side chat %d (runtime %q)", a.Name, made.ID, made.Runtime)
		return made.ID, nil
	}
	if s.ext.Fleet == nil {
		// No marks store (no database): a chat each time, nothing recorded.
		if _, err := create(); err != nil {
			return agents.ChatSession{}, err
		}
		return made, nil
	}
	// 🔴 ONE CALLER PER OWNER IN THIS PROCESS REACHES THE ROW LOCK (CI round,
	// statement timeouts): FleetChatOr holds a pooled connection on the locked
	// marks row while keep and create take MORE connections. With every
	// concurrent tap holding one connection blocked on that lock, a small pool
	// (CI's) was exhausted and the holder could never get the connection it
	// needed — the waiters timed out (57014) and answered 500. Waiting here, on
	// a mutex, holds no connection; the row lock still serialises callers on
	// other replicas, each of which waits on its own pool.
	mu, _ := s.fleetChatOpen.LoadOrStore(requestOwner(ctx), &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	id, err := s.ext.Fleet.FleetChatOr(ctx, keep, create)
	if err != nil {
		return agents.ChatSession{}, err
	}
	if made.ID == id {
		return made, nil
	}
	return kept, nil
}

// fleetDraft is the box's draft for a question about subject (a fleet id;
// "" or one not in the owner's fleet: the fleet as a whole). Muster's words,
// naming the session by its id and name, with its state — the owner writes the
// question after it.
func (s *Server) fleetDraft(ctx context.Context, about string) string {
	if about == "" {
		return "About my fleet: "
	}
	sub, ok := s.fleetSubjectFor(ctx, about)
	if !ok {
		return "About my fleet: "
	}
	d := "About " + sub.row.ID
	if n := strings.TrimSpace(sub.row.Name); n != "" {
		d += " (“" + cutRunes(n, 80) + "”)"
	}
	if sub.state != "" {
		d += ", " + strings.ReplaceAll(sub.state, "_", " ")
	}
	return d + ": "
}
