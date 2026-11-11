package api

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
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
// A session claim is RELEASED (claimReleasedBy, on setNoteStatus — the funnel
// every status writer passes):
//   - the task set back to open by a person, a session or an agent (the card,
//     the machine route, the task tool, an agent's own route and tool) —
//     stored or queued for an upstream;
//   - the upstream moving the task back to open (a reference's snapshot,
//     stored): the upstream reopened it;
//   - the task stored as complete, by ANY writer: done is done;
//   - the claiming session ENDED (sessionEnded): its window is gone from the
//     host it claimed from while that host is online, reporting status and
//     listing windows — muster's one "a session is gone" rule
//     (sessionstate.EndGone). Applied by the claim sweep (ReleaseEndedClaims)
//     and at each decision that reads a claim (releaseEndedClaimsOf).
//
// 🔴 A CLAIM'S HOST IS ONLY ONE THE MIRROR SAW THE SESSION ON (requestClaimant,
// a mirrored session's own call, linkUpstreamClaimants); otherwise it is "",
// and an empty host is never judged ended. A request's own host header is not
// used: it names the machine, not a host whose window list could prove the
// session gone.
//
// The UPSTREAM's claim is released by a later snapshot without it — and, the
// same way as a session's, when its claimant session (linked to the task with
// a host, linkUpstreamClaimants) ended: the release is recorded on that link
// for the snapshot's revision (notes.UpstreamReleaseMark), so the next
// snapshot naming the same session at the same revision does not bring it
// back; a newer revision (a new pickup, any upstream change) is judged afresh.
//
// 🔴 AN AGENT'S OPEN RELEASES EVERY SESSION CLAIM ON ITS TASK, NOT ONLY "ITS
// OWN": a dispatched agent names no session, so no claim is its own to pick
// out. It is bound to the task, and setting the task back to open is it
// handing the work back — the operator's D4 — so the task reads unclaimed and
// its next assignment gets its card. Nothing cheaper says which sessions an
// agent "relates to".
//
// The upstream's claim is not written here at all: it is read from the
// reference's snapshot (a later snapshot without it is the release). What
// this file does with it is link the claimant's session, when it is a
// mirrored session one of the owner's hosts lists now (linkUpstreamClaimants).
// ---------------------------------------------------------------------------

// claimant is who a status write came from, as a claim records it: the
// session id ("" for none: no claim), the name to show, and its host.
type claimant struct {
	session, name, host string
}

// unclaimWriters are the writers whose `open` is an unclaim: a person's, a
// session's or an agent's own write. Not muster's (plugin_reference — whose
// stored open is the upstream reopen, claimReleasedBy — dispatch-advance,
// agent-destroy-reopen, task-merge).
var unclaimWriters = map[statusWriter]bool{
	writerHumanUI: true, writerMachineAPI: true, writerTaskTool: true, writerAgentRoute: true, writerAgentTool: true,
}

// claimReleasedBy is THE release rule on a status write (see the banner):
// whether writer's accepted write of status — stored, or queued for an
// upstream — releases the task's session claims. Pure, so its cases are pinned.
func claimReleasedBy(writer statusWriter, status string, queued bool) bool {
	switch status {
	case notes.StatusOpen:
		return unclaimWriters[writer] || (writer == writerPluginReference && !queued)
	case notes.StatusComplete:
		return !queued
	}
	return false
}

// releaseClaimsOnStatus releases task id's session claims when writer's
// accepted write of status does (claimReleasedBy), reporting whether any was.
// It never fails the write it follows.
func (s *Server) releaseClaimsOnStatus(ctx context.Context, writer statusWriter, id int64, status string, queued bool) bool {
	if !claimReleasedBy(writer, status, queued) {
		return false
	}
	n, err := s.ext.Notes.ReleaseClaims(ctx, id)
	if err != nil {
		s.logger.Printf("task claims: task %d: release on %s by %s: %v", id, status, writer, err)
		return false
	}
	if n > 0 {
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(id, 10))
	}
	return n > 0
}

// claimOnStatus applies the claim rule to an ACCEPTED status write (stored or
// queued) of status on task id from who: in_progress with a session claims
// (the release half is setNoteStatus's, releaseClaimsOnStatus). It never fails
// the write it follows; it reports whether a claim changed.
func (s *Server) claimOnStatus(ctx context.Context, id int64, status string, who claimant) bool {
	if status != notes.StatusInProgress || who.session == "" {
		return false
	}
	name := who.name
	if name == "" {
		name = notes.SessionClaimName(who.session)
	}
	ok, err := s.ext.Notes.ClaimByLink(ctx, id, notes.SessionLink{SessionID: who.session, ClaimHost: who.host}, name, false)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false
	case err != nil:
		s.logger.Printf("task claims: task %d: claim by %s: %v", id, who.session, err)
		return false
	}
	if ok {
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(id, 10))
	}
	return ok
}

// sessionEnded is THE "the claiming session ended" rule: session sid's window
// is gone from host — the host it claimed from — while that host is online,
// reports status and lists windows (an empty list is unread, never "every
// window gone"; sessionstate.EndGone's rule). A claim with no host, or whose
// host is offline, unknown or not reporting, has not ended. Pure.
func sessionEnded(hs []mirror.HostView, host, sid string) bool {
	if host == "" || sid == "" {
		return false
	}
	for _, h := range hs {
		if h.Host != host {
			continue
		}
		if !h.Online || !h.StatusReport || len(h.Windows) == 0 {
			return false
		}
		for _, w := range h.Windows {
			if (w.Claude && w.ClaudeSession == sid) || slices.Contains(w.OtherClaude, sid) {
				return false
			}
		}
		return true
	}
	return false
}

// releaseEndedClaimsOf releases n's claims whose session ended (sessionEnded,
// on one read of the owner's hosts) — its sessions' own, and the upstream's
// whose claimant session is linked with a host — and answers n as it stands
// after (re-read when anything was released).
func (s *Server) releaseEndedClaimsOf(ctx context.Context, n notes.Note) notes.Note {
	if s.ext.Mirror == nil {
		return n
	}
	upstream := map[string]bool{}
	mark := ""
	if n.Ref != nil {
		mark = notes.UpstreamReleaseMark(n.Ref.Snapshot.Rev)
		for _, c := range n.Ref.Snapshot.Claimants {
			upstream[c.AgentSession] = c.AgentSession != ""
		}
	}
	var hs []mirror.HostView
	released := false
	for _, l := range n.Sessions {
		own := l.ClaimedAt != nil
		up := upstream[l.SessionID] && l.UpstreamReleasedRev != mark
		if (!own && !up) || l.ClaimHost == "" {
			continue
		}
		if hs == nil {
			hs = s.ext.Mirror.Hosts(ctx)
		}
		if !sessionEnded(hs, l.ClaimHost, l.SessionID) {
			continue
		}
		if own {
			if ok, err := s.ext.Notes.ReleaseSessionClaim(ctx, n.ID, l.SessionID); err != nil {
				s.logger.Printf("task claims: task %d: release ended session %s: %v", n.ID, l.SessionID, err)
			} else if ok {
				released = true
			}
		}
		if up {
			if ok, err := s.ext.Notes.ReleaseUpstreamClaim(ctx, n.ID, l.SessionID, mark); err != nil {
				s.logger.Printf("task claims: task %d: release the upstream claim of ended session %s: %v", n.ID, l.SessionID, err)
			} else if ok {
				released = true
			}
		}
	}
	if !released {
		return n
	}
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(n.ID, 10))
	if fresh, err := s.ext.Notes.Get(ctx, n.ID); err == nil {
		return fresh
	}
	return n
}

// claimSweepBatch bounds one claim sweep's read; ClaimSweepInterval is its
// period.
const (
	claimSweepBatch    = 2000
	ClaimSweepInterval = time.Minute
)

// ReleaseEndedClaims is one claim sweep: every owner's session claims (one
// system-scoped read, notes.ClaimedSessions), each owner's hosts read once,
// and every claim whose session ended (sessionEnded) released as its owner.
// It reports how many it released.
func (s *Server) ReleaseEndedClaims(ctx context.Context) (int, error) {
	if s.ext.Mirror == nil || s.ext.Notes == nil {
		return 0, nil
	}
	claims, err := s.ext.Notes.ClaimedSessions(authz.System(ctx), claimSweepBatch)
	if err != nil {
		return 0, err
	}
	released := 0
	hosts := map[int64][]mirror.HostView{}
	for _, c := range claims {
		if c.Host == "" {
			continue // no host to judge: released by open or complete only
		}
		octx := s.attentionPrincipal(ctx, c.OwnerID)
		hs, ok := hosts[c.OwnerID]
		if !ok {
			hs = s.ext.Mirror.Hosts(octx)
			hosts[c.OwnerID] = hs
		}
		if !sessionEnded(hs, c.Host, c.SessionID) {
			continue
		}
		release := func() (bool, error) { return s.ext.Notes.ReleaseSessionClaim(octx, c.NoteID, c.SessionID) }
		if c.Upstream != "" {
			release = func() (bool, error) { return s.ext.Notes.ReleaseUpstreamClaim(octx, c.NoteID, c.SessionID, c.Upstream) }
		}
		if ok, err := release(); err != nil {
			s.logger.Printf("task claims: task %d: release ended session %s: %v", c.NoteID, c.SessionID, err)
		} else if ok {
			released++
			s.broadcastTo(c.OwnerID, EventTaskChanged, strconv.FormatInt(c.NoteID, 10))
		}
	}
	return released, nil
}

// RunClaimSweep runs ReleaseEndedClaims every interval (<= 0:
// ClaimSweepInterval) until ctx ends. It does nothing without a mirror.
func (s *Server) RunClaimSweep(ctx context.Context, interval time.Duration) {
	if s.ext.Mirror == nil || s.ext.Notes == nil {
		return
	}
	if interval <= 0 {
		interval = ClaimSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := s.ReleaseEndedClaims(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("task claims: sweep: %v", err)
		} else if n > 0 {
			s.logger.Printf("task claims: released %d claim(s) of ended sessions", n)
		}
	}
}

// knownMirrorSession reports whether sid is a Claude session one of the
// owner's online hosts lists now, with its host and the name muster shows for
// it.
func (s *Server) knownMirrorSession(ctx context.Context, sid string) (host, name string, ok bool) {
	// A CLAUDE session only: mirrorSessionOf also binds an opencode caller's
	// key, which no claimant names.
	if s.ext.Mirror == nil || !mirror.ValidClaudeSession(sid) {
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
// session is a mirrored session muster knows to the task (`worked`): the
// session that picked the upstream record up is then on the task's thread.
// The claim itself stays the snapshot's (notes.Claims).
func (s *Server) linkUpstreamClaimants(ctx context.Context, n notes.Note) {
	if n.Ref == nil {
		return
	}
	for _, c := range n.Ref.Snapshot.Claimants {
		if c.AgentSession == "" {
			continue
		}
		host, _, ok := s.knownMirrorSession(ctx, c.AgentSession)
		if !ok {
			continue
		}
		if _, _, err := s.ext.Notes.LinkSession(ctx, n.ID, notes.SessionLink{SessionID: c.AgentSession, Role: notes.RoleWorked}, false); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				s.logger.Printf("task claims: task %d: link the upstream claimant's session: %v", n.ID, err)
			}
			continue
		}
		if err := s.ext.Notes.SetClaimHost(ctx, n.ID, c.AgentSession, host); err != nil {
			s.logger.Printf("task claims: task %d: record the upstream claimant's host: %v", n.ID, err)
		}
	}
}

// requestClaimant is the claimant a machine request names: its session
// header, and — only when one of the owner's hosts lists that session — the
// mirrored session's name and THAT host (what sessionEnded judges the claim
// by). Otherwise the host is "": the request's own host header names a
// machine, not a host whose window list could ever prove the session gone.
func (s *Server) requestClaimant(ctx context.Context, sessionID string) claimant {
	c := claimant{session: sessionID}
	if sessionID == "" {
		return c
	}
	if host, name, ok := s.knownMirrorSession(ctx, sessionID); ok {
		c.name, c.host = name, host
	}
	return c
}
