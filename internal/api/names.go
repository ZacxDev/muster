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

// notifyName is how a NOTIFICATION names a mirrored session whose HUMAN name
// is name (an attention item's Name: a push's title, the Asks list, the
// attention summary; an episode's push title and "Stalled / waiting" row):
// name alone — byte for byte what it was before the preference existed —
// unless tmux is on, its owner's "Show tmux sessions"
// (notifyTmuxOn), and at names the window as muster did before sessions had
// names (mirror.SessionView.TitleAt: the host's label for it, else its tmux
// address, and its host); then name, " · ", and that:
//
//	Billing migration · mango:1 @ atlas
//	Billing migration · scratch3:5 @ atlas   (a host that sent no label)
//
// A session with no human name is already called by its place, so name is ""
// there and stays "" (the caller shows the place: no address twice). An agent
// has no tmux window, so no at, and is never changed; nor is a session no
// window shows now (muster names no place it cannot see).
//
// 🔴 THE ONE PLACE THE ADDRESS IS ADDED. Its callers are a ledger
// (TestTheSessionNameCallerLedger's tmuxAddressSurfaces): every surface on the
// name ledger is either one of them or says why a notification's address does
// not apply to it, so a new notification surface cannot skip the preference.
func notifyName(name string, at mirror.Place, tmux bool) string {
	if p := tmuxPlace(name, at, tmux); p != "" {
		return name + mirror.QualifierSep + p
	}
	return name
}

// tmuxPlace is THE RULE for "Show tmux sessions": the window's place, at
// (mirror.SessionView.Place, shown "mango:1 @ atlas"), when tmux is on and the
// session is called by a human name; "" otherwise — off, a session called by
// its place already (no codename twice), a human name that IS the codename
// (a manual name "mango:1" on the window labelled "mango:1": the headline
// already says it, so no "mango:1 · mango:1 @ atlas"), or no window. notifyName adds it to a
// notification; mirrorCards puts it on the session lists' entries (the bar's
// pills and the dock's rows, ui.MirrorCardView.TmuxAt). Its callers are a
// ledger (TestEveryNotificationSurfaceAsksForTheTmuxAddress).
func tmuxPlace(name string, at mirror.Place, tmux bool) string {
	if !tmux || name == "" || at.Code == "" || name == at.Code {
		return ""
	}
	return at.String()
}

// codenameFront is THE RULE for "Start notification titles with the tmux
// name" (notify_prefs.go): what a push title starts with — the window's
// codename (the host's label for it, else its tmux address: at.Code, as "Show
// tmux sessions" falls back) and its host, compact: "mango:1@laptop",
// "scratch3:5@laptop". The host is in it because hosts build their labels from
// one shared slot table, so "mango:1" alone can be two windows. "" for no
// window (an agent, a session no window shows now).
func codenameFront(at mirror.Place) string {
	if at.Code == "" {
		return ""
	}
	if at.Host == "" {
		return at.Code
	}
	return at.Code + "@" + at.Host
}

// codenameTitle is a push title that names a mirrored session by words — its
// HUMAN name, never notifyName's decorated one, so with "Show tmux sessions"
// on too the place is said ONCE, at the front — followed by rest, with
// codenameFront and " · " IN FRONT; keep is the part the title rule keeps
// verbatim (goNotify's TitleKeep for it):
//
//	mango:1@laptop · Billing migration needs you
//
// A human name that IS the codename is replaced by the front ("mango:1@laptop
// is asking": no codename twice). "" (the push keeps its title) for no window,
// or no human name: the title already calls the session by its place, which
// starts with its codename and names its host.
func codenameTitle(words, rest string, at mirror.Place) (title, keep string) {
	front := codenameFront(at)
	if words == "" || front == "" {
		return "", ""
	}
	if words == at.Code {
		return front + rest, front
	}
	keep = front + mirror.QualifierSep + words
	return keep + rest, keep
}

// codenameLed is a push title about a tmux window that names it by no human
// name — the waiting window's fixed title, a host dispatch's outcome, a
// successor's start — with codenameFront and " · " in front, and keep the
// front alone (the rest of title is plained as before):
//
//	mango:1@laptop · A session is waiting on you
//	agents:3@atlas · Claude Code is waiting for you: …
//
// The caller passes title WITHOUT its own " on <host>", which the front now
// says. "" for no window.
//
// Surfaces only DECLARE these titles (RouterNotification.CodenameTitle);
// goNotify, the one push title sink, sends one instead of Title when the
// owner turned the switch on. Every push title is classified
// (TestEveryPushTitleIsClassifiedForCodenameFirst).
func codenameLed(title string, at mirror.Place) (string, string) {
	front := codenameFront(at)
	if front == "" {
		return "", ""
	}
	return front + mirror.QualifierSep + title, front
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
