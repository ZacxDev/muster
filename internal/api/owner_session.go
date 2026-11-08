package api

import "context"

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
// AND the caller's own window — as the host lists it — carries no dispatch
// tag. A window that cannot be found is refused (the closed direction).
//
// ⚠ A MITIGATION, NOT A BOUNDARY. The pane is the caller's claim
// (X-Muster-Pane) and an agent with a shell can clear its own window's tag:
// both are shell commands normal permission mode puts to the owner as an ask.
// What it stops is the plain case — an agent calling attention_answer, or
// agent_destroy on its own dispatch, as itself.
// ---------------------------------------------------------------------------

// notAMirroredSession is ownersOwnSession's answer for a caller that is not one
// of the owner's mirrored sessions at all: the site answers with its own
// sentence for that.
const notAMirroredSession = "\x00not a mirrored session"

// ownerAuthoritySpawned refuses a session muster started for the chief.
const ownerAuthoritySpawned = "this tool acts with your owner's authority, and a session muster started for the chief cannot use it; ask your owner"

// ownerAuthorityUnknown refuses a caller whose window the host does not list.
const ownerAuthorityUnknown = "this tool acts with your owner's authority, and muster could not find your window in your host's listing to check that you are not a session it started for the chief; try again"

// ownersOwnSession is "" when c is one of the owner's OWN mirrored sessions:
// mirroredSession(), and its window carries no dispatch. notAMirroredSession
// when it is not a mirrored session; otherwise the refusal.
//
// 🔴 mirroredSession()'S ONLY CALLER outside this file's predicate is the
// dispatch-hold door's caller check (a seam test pins the ledger).
func (s *Server) ownersOwnSession(ctx context.Context, c toolCaller) string {
	if !c.mirroredSession() {
		return notAMirroredSession
	}
	if c.mirrorWindow == "" {
		// A per-session host lists no windows, so it can carry no tag: no host
		// spawns on one (spawning needs a per-window, v2 host).
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
