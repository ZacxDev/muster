package api

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// CLAIMS, THE WRITERS (docs/tasks.md#claims; the predicate is notes.Claims).
//
// A session's claim is made by picking the task up through muster's task
// tools: a status write to in_progress that CARRIES the session's id — the
// machine route with its session header (what `muster task status <id>
// in_progress` sends from inside a session), or a mirrored session's own
// task_set_status call. The claim is recorded on the session's task link
// whatever became of the status: on a plugin task reference the status is
// only QUEUED for the upstream, which may refuse it (an upstream status set
// that has no "in progress"), and the claim must not be lost with it.
//
// The unclaim is a status write back to open by a person, a session or an
// agent (the card, the machine route, the task tool): every session claim on
// the task is cleared. muster's own writers (a snapshot, a dispatch advance,
// the reopen after an agent is destroyed) neither claim nor unclaim.
//
// The upstream's claim is not written here at all: it is read from the
// reference's snapshot. What this file does with it is link the claimant's
// session, when the upstream recorded one and it is a mirrored session one of
// the owner's hosts lists now (linkUpstreamClaimants).
// ---------------------------------------------------------------------------

// claimant is who a status write came from, as a claim records it: the
// session id ("" for none: no claim), the name to show, and its host.
type claimant struct {
	session, name, host string
}

// unclaimWriters are the writers whose `open` is an unclaim: a person's, a
// session's or an agent's own write. Not muster's (plugin_reference,
// dispatch-advance, agent-destroy-reopen, task-merge).
var unclaimWriters = map[statusWriter]bool{
	writerHumanUI: true, writerMachineAPI: true, writerTaskTool: true, writerAgentRoute: true, writerAgentTool: true,
}

// claimOnStatus applies the claim rule to an ACCEPTED status write (stored or
// queued) of status on task id by writer from who: in_progress with a session
// claims; open from an unclaim writer releases every session claim. It never
// fails the write it follows; it reports whether a claim changed.
func (s *Server) claimOnStatus(ctx context.Context, writer statusWriter, id int64, status string, who claimant) bool {
	changed := false
	switch {
	case status == notes.StatusInProgress && who.session != "":
		name := who.name
		if name == "" {
			name = notes.SessionClaimName(who.session)
		}
		ok, err := s.ext.Notes.ClaimByLink(ctx, id, notes.SessionLink{SessionID: who.session, Host: who.host}, name, false)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			s.logger.Printf("task claims: task %d: claim by %s: %v", id, who.session, err)
		default:
			changed = ok
		}
	case status == notes.StatusOpen && unclaimWriters[writer]:
		n, err := s.ext.Notes.ReleaseClaims(ctx, id, "")
		if err != nil {
			s.logger.Printf("task claims: task %d: release: %v", id, err)
		}
		changed = n > 0
	}
	if changed {
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(id, 10))
	}
	return changed
}

// knownMirrorSession reports whether sid is a Claude session one of the
// owner's online hosts lists now, with its host and the name muster shows for
// it.
func (s *Server) knownMirrorSession(ctx context.Context, sid string) (host, name string, ok bool) {
	if s.ext.Mirror == nil {
		return "", "", false
	}
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		if !h.Online {
			continue
		}
		if e, ok := s.mirrorSessionOf(ctx, h.Host, sid); ok {
			return h.Host, s.mirrorLabel(ctx, e), true
		}
	}
	return "", "", false
}

// linkUpstreamClaimants links each claimant of reference n's snapshot whose
// recorded session is a mirrored session muster knows to the task (`worked`):
// the session that claimed the upstream record is then on the task's thread.
// The claim itself stays the snapshot's (notes.Claims).
func (s *Server) linkUpstreamClaimants(ctx context.Context, n notes.Note) {
	if n.Ref == nil {
		return
	}
	for _, c := range n.Ref.Snapshot.Claimants {
		if c.Session == "" {
			continue
		}
		host, _, ok := s.knownMirrorSession(ctx, c.Session)
		if !ok {
			continue
		}
		if _, _, err := s.ext.Notes.LinkSession(ctx, n.ID, notes.SessionLink{SessionID: c.Session, Role: notes.RoleWorked, Host: host}, false); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			s.logger.Printf("task claims: task %d: link the upstream claimant's session: %v", n.ID, err)
		}
	}
}

// requestClaimant is the claimant a machine request names: its session
// header, the mirrored session's name when one of the owner's hosts lists it,
// and the host header.
func (s *Server) requestClaimant(ctx context.Context, sessionID, hostHeader string) claimant {
	c := claimant{session: sessionID, host: hostHeader}
	if sessionID == "" {
		return c
	}
	if host, name, ok := s.knownMirrorSession(ctx, sessionID); ok {
		c.name = name
		if c.host == "" {
			c.host = host
		}
	}
	return c
}
