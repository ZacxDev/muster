package ui

import (
	"strings"
	"testing"
)

// TestTheMessageSearchIsOnlyOnTheViewersOwnChief: the Sessions tab's box asks
// the message search (its route searches the VIEWER's own chief) only when the
// chief on screen is the viewer's own (ChiefChatBar.MessageSearch). On a team's
// chief (a member's shared room) or another owner's (an admin viewing it) the
// box filters the rows by title only: no hx-get, no "Matches in messages"
// container, and it does not offer to search messages. The owner's tab has all
// of it. On both surfaces. RED before the round-1 fix: every chief's tab asked
// the route, which answered the viewer's chief's chats.
func TestTheMessageSearchIsOnlyOnTheViewersOwnChief(t *testing.T) {
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		id := chiefIndexID(surface)
		for _, c := range []struct {
			name string
			bar  ChiefChatBar
			on   bool
		}{
			{"owner", ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true, MessageSearch: true}, true},
			{"team shared room", ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true, SharedRoom: true}, false},
			{"another owner's chief", ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true}, false},
		} {
			bar := c.bar
			html := renderString(t, sessionsPanel(&bar, "chiefy", chiefSheetSessions(5), surface))
			box := findAttr(parseFragment(t, html), "data-chief-sessions-search")
			if box == nil {
				t.Fatalf("%s %s: no search box (the title filter is everyone's)", surface, c.name)
			}
			wired := attrOf(box, "hx-get") != "" || attrOf(box, "hx-target") != "" || attrOf(box, "hx-trigger") != ""
			container := strings.Contains(html, `id="`+id+`-messages"`) || strings.Contains(html, "data-chief-message-matches=")
			if wired != c.on || container != c.on {
				t.Errorf("%s %s: message search wired %v, container %v, want %v", surface, c.name, wired, container, c.on)
			}
			placeholder := "Search sessions…"
			controls := id + "-sessions"
			if c.on {
				placeholder = "Search sessions and messages…"
				controls += " " + id + "-messages"
			}
			if attrOf(box, "placeholder") != placeholder || attrOf(box, "aria-controls") != controls {
				t.Errorf("%s %s: placeholder %q, aria-controls %q", surface, c.name, attrOf(box, "placeholder"), attrOf(box, "aria-controls"))
			}
		}
	}
}
