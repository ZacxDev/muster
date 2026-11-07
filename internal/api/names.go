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
// 🔴 THESE FOUR ARE THE ONLY CALLERS OF sessionname.Agent AND sessionname.Mirror,
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
