package ui

import (
	"strconv"
)

// The pin routes (internal/api pins.go). The toggles are forms with hx-post:
// htmx posts them in place and the answer is the re-rendered list. They also
// carry method/action, but that is not a no-JavaScript path — #agents-list and
// its cards are loaded only by htmx, so without JavaScript no toggle is drawn.
const (
	PinPath   = "/agents/pins"
	UnpinPath = "/agents/pins/delete"
)

// pinIcon is a push-pin, drawn in currentColor.
const pinIcon = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 17v5"/><path d="M9 3h6l-1 6 3 3v2H7v-2l3-3z"/></svg>`

// agentPinFields and mirrorPinFields are the subject fields an entry's pin
// form posts.
func agentPinFields(a AgentCardView) [][2]string {
	return [][2]string{{"kind", "agent"}, {"agent_id", strconv.FormatInt(a.ID, 10)}}
}

func mirrorPinFields(m MirrorCardView) [][2]string {
	return [][2]string{{"kind", "mirror"}, {"host", m.Host}, {"session", m.Session}, {"window", m.Window}}
}

// ⚠ PINNED ENTRIES ARE CARDS AGAIN, AND THEIR OWN TOGGLE UNPINS THEM. A pinned
// entry used to be drawn as a chip in a row above the cards, unpinned from the
// chip's ×; that row is gone (operator ask: the session switcher's Pins view
// supersedes it). A pinned entry is now its card, still drawn first
// (orderList), and the card's pin toggle — the ⋯ menu's item on an agent, the
// pin button on a mirrored window — pins or unpins it.
