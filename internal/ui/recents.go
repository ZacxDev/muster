package ui

// RECENTLY OPENED ENTRIES: the entries the viewer last opened (an agent's page,
// a mirrored window's view or terminal), newest first. Stored per user on the
// server (internal/recents, migration 0018), so they are the same on every
// device. The session switcher's Recent view draws them (switcher.go).
//
// ⚠ THE AGENTS LIST'S "RECENTLY VIEWED" STRIP THAT ALSO DREW THEM IS GONE
// (operator ask): the switcher docked at the bottom of the list supersedes it.

// RecentRef is one recently opened entry, as the store keys it: an agent by id
// (Mirror zero), or a mirrored entry by its address (AgentID 0).
type RecentRef struct {
	AgentID int64
	Mirror  MirrorRef
}
