package ui

import (
	"strings"
	"testing"
)

// TestTheMessageMatchesListOnlyBodyMatchesLinkedPerSurface: the message
// search's answer (RenderChiefMessageMatches) draws a chat only when its
// MESSAGES matched (a Snippet: a title-only match is already a row of the tab),
// best first as given, under "Matches in messages", with the words around the
// match — as a link to the chief's page on the page, as an hx-get of the panel
// body in the panel; nothing when nothing matched; and a failed read as a
// failure in the warning colour (state "error"), its Reason when given.
func TestTheMessageMatchesListOnlyBodyMatchesLinkedPerSurface(t *testing.T) {
	matches := []SessionView{
		{ID: 31, Title: "Lark notes", Snippet: "…the lark sang twice…"},
		{ID: 32, Title: "Title only lark"},
		{ID: 33, Title: "", Snippet: "a lark at noon"},
	}
	page := renderString(t, chiefMessageMatches(ChiefMessageMatchesView{Surface: ChatSurfacePage, AgentName: "chiefy", Matches: matches}))
	for _, want := range []string{
		`data-chief-message-matches-state="rows"`, `>Matches in messages</h3>`,
		`<a href="` + AgentSessionHref("chiefy", 31) + `" data-chief-message-match="31"`, `>Lark notes</span>`, `…the lark sang twice…`,
		`<a href="` + AgentSessionHref("chiefy", 33) + `" data-chief-message-match="33"`, `>New chat</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page: lacks %s\n%s", want, page)
		}
	}
	if strings.Contains(page, `data-chief-message-match="32"`) {
		t.Error("page: a title-only match is drawn as a message match")
	}
	if strings.Index(page, `match="31"`) > strings.Index(page, `match="33"`) {
		t.Error("page: the matches are not in the order given")
	}
	panel := renderString(t, chiefMessageMatches(ChiefMessageMatchesView{Surface: ChatSurfaceChiefPanel, Matches: matches}))
	if !strings.Contains(panel, `<button type="button" data-chief-message-match="31"`) ||
		!strings.Contains(panel, `hx-get="/ui/chief/panel?session=31" hx-target="#chief-panel-body" hx-swap="innerHTML"`) || strings.Contains(panel, "<a ") {
		t.Errorf("panel: a match does not switch the panel body:\n%s", panel)
	}
	if got := renderString(t, chiefMessageMatches(ChiefMessageMatchesView{Matches: matches[1:2]})); got != "" {
		t.Errorf("only a title match: drew %q, want nothing", got)
	}
	failed := renderString(t, chiefMessageMatches(ChiefMessageMatchesView{Failed: true, Matches: matches}))
	if !strings.Contains(failed, `data-chief-message-matches-state="error"`) || !strings.Contains(failed, "text-st-warning-fg") ||
		!strings.Contains(failed, ChiefMessageSearchFailedText) || strings.Contains(failed, "data-chief-message-match=") {
		t.Errorf("a failed read: %s", failed)
	}
	if r := renderString(t, chiefMessageMatches(ChiefMessageMatchesView{Failed: true, Reason: "no database here"})); !strings.Contains(r, ">no database here<") {
		t.Errorf("a failed read's reason: %s", r)
	}
}

// TestTheSessionsScriptCountsMessageMatches: the tab's script counts the
// message matches with the rows before it shows "No sessions match", re-applies
// when the message search's answer is swapped in, and shuts the sheet on a
// match's tap as on a row's. A spelled guard on the script's source; e2e
// chief-sessions-tab.spec.ts drives the behaviour.
func TestTheSessionsScriptCountsMessageMatches(t *testing.T) {
	js := renderString(t, chiefSessionsScript())
	for _, want := range []string{
		"if (fresh) n += mc.querySelectorAll('[' + MATCH + ']').length;",
		"var fresh = !!mc && !!q && mc.getAttribute('data-answered') === q;",
		"document.addEventListener('htmx:afterSwap'",
		"t.closest('[' + ROW + '], [' + MATCH + ']')",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the Sessions script lacks %s", want)
		}
	}
	count := strings.Index(js, "if (fresh) n += mc.querySelectorAll('[' + MATCH + ']').length;")
	none := strings.Index(js, "if (none) none.hidden = n > 0 || pending || failed;")
	if count < 0 || none < 0 || count > none {
		t.Error("the message matches are not counted before the none line is decided")
	}
}

// TestTheSessionsTabSaysWhenItsListMayBeIncomplete: a failed read of the
// chief's chats (ChiefChatBar.SessionsFailed) puts a status line above the
// rows, on both surfaces, in the warning colour; none otherwise.
func TestTheSessionsTabSaysWhenItsListMayBeIncomplete(t *testing.T) {
	for _, surface := range []ChatSurface{ChatSurfacePage, ChatSurfaceChiefPanel} {
		for _, failed := range []bool{false, true} {
			html := renderString(t, sessionsPanel(&ChiefChatBar{AgentID: 42, SessionID: 5, Primary: true, SessionsFailed: failed}, "chiefy", chiefSheetSessions(5), surface))
			has := strings.Contains(html, `data-chief-sessions-failed="" role="status" class="px-3 py-2 text-xs text-st-warning-fg">`+ChiefSessionsFailedText+`<`)
			if has != failed {
				t.Errorf("%s failed=%v: the incomplete line shown=%v", surface, failed, has)
			}
		}
	}
}
