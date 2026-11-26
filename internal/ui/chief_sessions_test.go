package ui

import (
	"strings"
	"testing"
	"time"
)

// chiefPanelWithSessions is a chief panel on its primary (92) with two side
// chats on Claude Code models.
func chiefPanelWithSessions() ChiefPanelView {
	now := time.Now()
	bar := ChiefChatBar{AgentID: 105, AgentName: "chief", SessionID: 92, Primary: true,
		NewChatModels: []string{"opus", "sonnet"}, Runtimes: []SessionRuntimeView{{Runtime: "claude-code", Label: "Claude Code"}}}
	return ChiefPanelView{
		Configured: true,
		Chat: AgentDetailView{ID: 105, Name: "chief", Surface: ChatSurfaceChiefPanel, ActiveSessionID: 92, ChiefChat: &bar, Sessions: []SessionView{
			{ID: 92, Title: "the original", Active: true, LastActive: now, IsDefault: true, DefaultPath: "/agents/105/sessions/92/default"},
			{ID: 95, Title: "opus thread", LastActive: now, Runtime: "claude-code", RuntimeLabel: "Claude Code", Model: "opus",
				DefaultPath: "/agents/105/sessions/95/default"},
			{ID: 96, Title: "haiku thread", LastActive: now, Runtime: "claude-code", RuntimeLabel: "Claude Code",
				Model: "claude-haiku-4-5", DefaultPath: "/agents/105/sessions/96/default"},
		}},
	}
}

// TestThePanelHasOneListOfTheChiefsChats: the chief panel draws its chats ONCE
// — the Sessions tab of its sheet — and no thread list beside it: no clock
// launcher in its title bar, no checkbox sub-view, no "New session" runtime
// picker, no server-ranked rows. What the thread list did is in the tab: each
// chat's row with its model tag, the primary's ★ on exactly one row, Make
// primary in every OTHER row's ⋯ menu (keeping the panel on the chat it is
// on), and the "+" menu's Claude Code models. RED before the fold: the panel
// drew the thread list beside the tab (a second list of the same chats).
func TestThePanelHasOneListOfTheChiefsChats(t *testing.T) {
	var b strings.Builder
	if err := RenderChiefPanelBody(&b, chiefPanelWithSessions()); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, banned := range []string{`for="chief-threads"`, `id="chief-threads"`, `id="chief-thread-search"`, `id="chief-thread-results"`,
		"data-chief-thread=", "data-session-picker", "data-new-session=", "data-make-default", "data-session-runtime="} {
		if strings.Contains(html, banned) {
			t.Errorf("the panel still draws its thread list: %s", banned)
		}
	}
	if n := strings.Count(html, "data-chief-chip=\""); n != 3 {
		t.Errorf("%d rows of chats, want 3 (one list)", n)
	}
	for _, want := range []string{
		`data-chief-chip-model="opus"`, `data-chief-chip-model="claude-haiku-4-5"`,
		`hx-post="/agents/105/sessions/95/default?surface=chief-panel&amp;current=92"`,
		`hx-post="/agents/105/sessions/96/default?surface=chief-panel&amp;current=92"`,
		`data-chief-new-chat-model="opus"`, `data-chief-new-chat-model="sonnet"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the Sessions tab lacks %s", want)
		}
	}
	if strings.Contains(html, "/sessions/92/default") {
		t.Error("the primary offers to make itself the primary")
	}
	if n := strings.Count(html, sessionDefaultAttr+`=""`); n != 2 {
		// The ★ on the primary's row, and on the bar naming it over the transcript.
		t.Errorf("%d ★ marks, want 2 (the primary's row and the bar)", n)
	}
}
