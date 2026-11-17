package api

import (
	"context"
	"errors"

	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// OWNER TOOLS REFUSE A SESSION MUSTER STARTED FOR THE CHIEF
// (docs/design/chief-dispatch.md 5.1, "Owner tools refuse spawned agents").
//
// Host spawns create a new kind of mirrored session: one muster started,
// running a prompt the chief relayed. mirroredSession() admits ANY listed
// session, and it was the caller rule of every tool that acts WITH THE OWNER'S
// AUTHORITY — so a host-spawned agent could have answered its owner's asks,
// approved plugin drafts, ended its own dispatch or restarted the chief.
//
// ownersOwnSession is that rule now, at every such site: mirroredSession()
// AND the caller is no host dispatch's, by TWO reads:
//
//   - THE SERVER'S OWN RECORD (host_dispatches): the caller's session is no
//     dispatch's agent_session — the id the host's own spawn reply named, ended
//     or not — and its window is no open dispatch's window. Neither is a tag
//     anything in the window can set, so an agent that clears its window's
//     @muster-dispatch option is still refused. An unreadable record refuses.
//   - the caller's own window — as the host lists it — carries no dispatch
//     tag (a spawn whose reply has not been recorded yet).
//
// A window that cannot be found is refused (the closed direction).
//
// ⚠ STILL A MITIGATION, NOT A BOUNDARY, on its own: the pane is the caller's
// claim (X-Muster-Pane), so a spawned agent can name ANOTHER window of the
// owner's, and its host's token can be read by any process of the machine's
// user. What closes that is the step-up gate (host_stepup.go): every
// owner-authority call from a machine that starts agents for the chief is a
// confirmation card on the owner's phone, whichever window it names.
// ---------------------------------------------------------------------------

// notAMirroredSession is ownersOwnSession's answer for a caller that is not one
// of the owner's mirrored sessions at all: the site answers with its own
// sentence for that.
const notAMirroredSession = "\x00not a mirrored session"

// ownerAuthoritySpawned refuses a session muster started for the chief.
const ownerAuthoritySpawned = "this tool acts with your owner's authority, and a session muster started for the chief cannot use it; ask your owner"

// ownerAuthorityUnknown refuses a caller whose window the host does not list.
const ownerAuthorityUnknown = "this tool acts with your owner's authority, and muster could not find your window in your host's listing to check that you are not a session it started for the chief; try again"

// ownerAuthorityRecordUnknown refuses a caller muster could not check against
// its own record of the host's dispatches (the closed direction).
const ownerAuthorityRecordUnknown = "this tool acts with your owner's authority, and muster could not read its record of the agents it started on your host to check that you are not one of them; try again"

// ownersOwnSession is "" when c is one of the owner's OWN mirrored sessions:
// mirroredSession(), no host dispatch's by the server's record
// (spawnedByRecord), and its window carries no dispatch. notAMirroredSession
// when it is not a mirrored session; otherwise the refusal.
//
// 🔴 mirroredSession()'S ONLY CALLER outside this file's predicate is the
// dispatch-hold door's caller check (a seam test pins the ledger).
func (s *Server) ownersOwnSession(ctx context.Context, c toolCaller) string {
	if !c.mirroredSession() {
		return notAMirroredSession
	}
	if why := s.spawnedByRecord(ctx, c); why != "" {
		return why
	}
	if c.mirrorWindow == "" {
		// A per-session host lists no windows, so it can carry no tag: no host
		// spawns on one (spawning needs a per-window, v2 host). A remote caller
		// (remoteMirrorCaller) has no window here either: its host is not
		// mirrored to this muster, and its own mirror refuses a window that
		// carries a dispatch before it sends (mirrorhost's MCPProxy) — the same
		// host-reported fact this loop reads from a listing.
		return ""
	}
	if s.ext.Mirror == nil {
		return ownerAuthorityUnknown
	}
	for _, v := range s.ext.Mirror.Hosts(ctx) {
		if v.Host != c.asker.MirrorHost {
			continue
		}
		for _, w := range v.Windows {
			if w.ID != c.mirrorWindow {
				continue
			}
			if w.Dispatch > 0 {
				return ownerAuthoritySpawned
			}
			return ""
		}
	}
	return ownerAuthorityUnknown
}

// spawnedByRecord is ownerAuthoritySpawned when the server's OWN record of
// its host dispatches (host_dispatches) says c is one: its session is a
// dispatch's agent_session on c's host, ended or not (a session muster
// started for the chief never becomes the owner's own), or its window is an
// OPEN dispatch's window there (an ended row's window id may be reissued to
// another window, so only an open row counts). ownerAuthorityRecordUnknown
// when the record cannot be read; "" otherwise, and with no host-dispatch
// store. 🔴 Neither read is a tag a window can set: the session and window
// are the ones the host's own spawn reply named (applySpawnReply).
func (s *Server) spawnedByRecord(ctx context.Context, c toolCaller) string {
	st := s.hostDispatches()
	if st == nil {
		return ""
	}
	host, sid := c.asker.MirrorHost, c.asker.MirrorSession
	_, err := st.HostDispatchForSession(ctx, host, sid)
	switch {
	case err == nil:
		return ownerAuthoritySpawned
	case !errors.Is(err, plugins.ErrHostDispatchNotFound):
		s.logger.Printf("owner authority: read host %s's dispatch for session %s: %v; refused", host, sid, err)
		return ownerAuthorityRecordUnknown
	}
	if c.mirrorWindow == "" {
		return ""
	}
	rows, err := st.HostDispatchesForHost(ctx, host)
	if err != nil {
		s.logger.Printf("owner authority: read host %s's dispatches: %v; refused", host, err)
		return ownerAuthorityRecordUnknown
	}
	for _, r := range rows {
		if r.Status != plugins.HostDispatchEnded && r.Window != "" && r.Window == c.mirrorWindow {
			return ownerAuthoritySpawned
		}
	}
	return ""
}
