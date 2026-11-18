package ui

import (
	"strconv"
)

// The pin routes (internal/api pins.go). The one toggle that posts here is the
// session switcher's (SwitcherPinToggle, the item of the bar's ⋯ menu): a
// form with hx-post, answered with the item in its new state and the bar's rows out of band. It also carries
// method/action, and without JavaScript the server sends the browser back to
// the session's page.
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

// ⚠ A PIN IS SET AND CLEARED FROM THE SESSION SWITCHER ONLY. The retired Agents
// list drew pinned entries first and its cards carried a pin toggle; that page
// is gone. A pinned entry is still ordered first in the switcher (orderList)
// and shown in its Pins view, and the bar's ⋯ menu (switcher_pin.go) pins or
// unpins the page's own session.
