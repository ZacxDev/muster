package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionname"
)

// ---------------------------------------------------------------------------
// A SESSION'S NAME, AS EVERY SURFACE HERE ASKS FOR IT (internal/sessionname;
// docs/focus.md#a-sessions-name).
//
// 🔴 THESE ARE THE ONLY CALLERS OF sessionname.Agent AND sessionname.Mirror,
// and every surface that names a session reaches the rule through one of them:
// the batch pair over a focusIndex (a list, read once per render), or the
// single pair (one Get), for a surface that names one session. Their callers
// are a LEDGER (TestTheSessionNameCallerLedger): it fails when the set grows
// or shrinks, so a new surface is a decision, and one quietly dropped is too.
// ---------------------------------------------------------------------------

// agentName is agent a's name from the viewer's focus index (its stored name
// halves; none when the index has no row for it).
func (fi focusIndex) agentName(a agents.Agent) sessionname.Name {
	return sessionname.Agent(a, fi[focus.AgentSubject(a.ID)].Name)
}

// mirrorName is mirrored entry e's name from the viewer's focus index: its
// conversation's stored halves, none when its host reports no conversation.
func (fi focusIndex) mirrorName(e mirror.SessionView) sessionname.Name {
	var stored focus.Name
	if key := e.SessionKey(); key != "" {
		stored = fi[focus.MirrorSubject(key)].Name
	}
	return sessionname.Mirror(e, stored)
}

// agentName is agent a's name for a surface that names ONE session: its
// stored halves read as ctx's principal. No focus store, or a failed read
// (logged), resolves without them — the agent's display name or slug.
func (s *Server) agentName(ctx context.Context, a agents.Agent) sessionname.Name {
	return sessionname.Agent(a, s.storedName(ctx, focus.AgentSubject(a.ID)))
}

// mirrorName is mirrored entry e's name for a surface that names ONE session.
func (s *Server) mirrorName(ctx context.Context, e mirror.SessionView) sessionname.Name {
	var stored focus.Name
	if key := e.SessionKey(); key != "" {
		stored = s.storedName(ctx, focus.MirrorSubject(key))
	}
	return sessionname.Mirror(e, stored)
}

// subjectName is the stable name of the session sub names, for a chief tool
// that holds only its subject (focus_search's hits): its agent's, or the
// online window's showing its conversation, through fi's stored halves; ""
// when neither is visible to ctx's principal now.
func (s *Server) subjectName(ctx context.Context, fi focusIndex, sub focus.Subject) string {
	return s.subjectNameOf(ctx, fi, sub).Text
}

// subjectNameOf is subjectName as the resolved name.
func (s *Server) subjectNameOf(ctx context.Context, fi focusIndex, sub focus.Subject) sessionname.Name {
	switch sub.Kind {
	case focus.KindAgent:
		if s.ext.Agents == nil {
			return sessionname.Name{}
		}
		if a, err := s.ext.Agents.Get(ctx, sub.AgentID()); err == nil {
			return fi.agentName(a)
		}
	case focus.KindMirror:
		if e, ok := s.mirrorFocusEntryFor(ctx, sub.Key); ok {
			return fi.mirrorName(e)
		}
	}
	return sessionname.Name{}
}

// storedMirrorName is the name of the mirrored conversation keyed key from
// its stored halves ALONE — no window read — for a surface that needs only a
// HUMAN name and shows the session's place itself otherwise (an attention
// item: its Source already names the window, read once per build). Its
// fallback and technical id are empty: never draw them from here.
func (fi focusIndex) storedMirrorName(key string) sessionname.Name {
	return sessionname.Mirror(mirror.SessionView{}, fi[focus.MirrorSubject(key)].Name)
}

// humanName is subjectName when it is a HUMAN name (internal/sessionname:
// anything but the technical fallback), "" otherwise — for a surface that
// shows the session's place itself when it has no name.
func (s *Server) humanName(ctx context.Context, fi focusIndex, sub focus.Subject) string {
	var n sessionname.Name
	if sub.Kind == focus.KindMirror {
		n = fi.storedMirrorName(sub.Key)
	} else {
		n = s.subjectNameOf(ctx, fi, sub)
	}
	if !n.Human() {
		return ""
	}
	return n.Text
}

// mirrorLabel is how a chief-facing text names mirrored entry e (an ask's
// label, a comment's author, a route's session): its HUMAN name when it has
// one, else — as before the rule — its title and its host, "mango:1 @ ws",
// which tells the chief two unnamed windows on two hosts apart.
func (s *Server) mirrorLabel(ctx context.Context, e mirror.SessionView) string {
	if n := s.mirrorName(ctx, e); n.Human() {
		return n.Text
	}
	return e.DisplayTitle() + " @ " + e.Host
}

// humanMirrorName is the HUMAN name of the mirrored conversation keyed key
// from its stored halves (storedMirrorName), "" when it has none — for the
// stall prompt, which already gives the chief the window's place.
func (fi focusIndex) humanMirrorName(key string) string {
	if n := fi.storedMirrorName(key); n.Human() {
		return n.Text
	}
	return ""
}

// storedName is sub's stored name halves, the zero value for none.
func (s *Server) storedName(ctx context.Context, sub focus.Subject) focus.Name {
	if s.ext.Focus == nil {
		return focus.Name{}
	}
	f, ok, err := s.ext.Focus.Get(ctx, sub)
	if err != nil {
		s.logger.Printf("names: %s: %v", sub, err)
		return focus.Name{}
	}
	if !ok {
		return focus.Name{}
	}
	return f.Name
}
