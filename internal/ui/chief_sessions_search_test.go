package ui

import (
	"strings"
	"testing"
)

// THE SESSIONS TAB'S MESSAGE SEARCH, page-level (the panel's thread list was
// folded into the tab). Like chief_sessions_tab_test.go it names only what the
// code before the fold had too, and spells every mark as a literal, so it runs —
// and is red — against that code.

// TestTheSessionsSearchAlsoSearchesMessages: on the chief page and in the
// panel, the Sessions tab's search box filters the rows in the browser AND asks
// the server for the words in the chats' messages — GET
// /ui/chief/threads/search with the chat on screen and the asking surface,
// 300ms after the typing stops (and on the box's own clear) — into an empty
// "Matches in messages" container in the same tab. Each surface's box asks into
// its own sheet's container. RED before the fold: the box filtered titles only;
// the message search was the panel's thread list's.
func TestTheSessionsSearchAlsoSearchesMessages(t *testing.T) {
	for surface, markup := range chiefSurfaces(t, 5, chiefSheetSessions(5)) {
		sheet, id := chiefSheetOf(t, surface, markup)
		box := findAttr(sheet, "data-chief-sessions-search")
		if box == nil {
			t.Fatalf("%s: no search box", surface)
		}
		want := "page"
		if surface == "panel" {
			want = "chief-panel"
		}
		for k, v := range map[string]string{
			"name":       "q",
			"hx-get":     "/ui/chief/threads/search?session=5&surface=" + want,
			"hx-trigger": "input changed delay:300ms, search",
			"hx-target":  "#" + id + "-messages",
			"hx-swap":    "innerHTML",
		} {
			if got := attrOf(box, k); got != v {
				t.Errorf("%s: the search box's %s is %q, want %q", surface, k, got, v)
			}
		}
		tab := byID(sheet, id+"-tabs-panel-sessions")
		matches := byID(tab, id+"-messages")
		if matches == nil || !hasAttr(matches, "data-chief-message-matches") || matches.FirstChild != nil {
			t.Errorf("%s: no empty message-matches container in the Sessions tab: %v", surface, matches)
		}
	}
}

// TestThePanelDrawsNoThreadList: the chief panel's body carries no thread list
// beside its sheet — no clock launcher, no checkbox sub-view, no search of its
// own, no "New session" runtime picker — so its chats are listed ONCE, in the
// Sessions tab. RED before the fold.
func TestThePanelDrawsNoThreadList(t *testing.T) {
	panel := chiefSurfaces(t, 6, chiefSheetSessions(6))["panel"]
	for _, banned := range []string{`for="chief-threads"`, `id="chief-threads"`, `id="chief-thread-search"`, `id="chief-thread-results"`,
		"data-chief-thread=", "data-session-picker", `aria-label="Chat history"`} {
		if strings.Contains(panel, banned) {
			t.Errorf("the panel still draws its thread list: %s", banned)
		}
	}
	if n := strings.Count(panel, `data-chief-chip="6"`); n != 1 {
		t.Errorf("chat 6 is listed %d times in the panel, want once", n)
	}
}
